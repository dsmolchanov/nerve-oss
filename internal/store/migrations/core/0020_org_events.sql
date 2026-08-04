-- +goose Up

CREATE TABLE org_events (
  id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id        uuid        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  event_type    text        NOT NULL,
  ref_kind      text        NOT NULL,
  ref_id        uuid        NOT NULL,
  payload       jsonb       NOT NULL,
  fanned_out_at timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT uq_org_events_ref UNIQUE (org_id, event_type, ref_kind, ref_id)
);

CREATE INDEX idx_org_events_fanout_owed
  ON org_events (created_at)
  WHERE fanned_out_at IS NULL;

ALTER TABLE org_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE org_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_org_events ON org_events
  USING (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  )
  WITH CHECK (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  );

ALTER TABLE org_webhook_deliveries
  ADD COLUMN org_event_id uuid REFERENCES org_events(id) ON DELETE CASCADE;

-- +goose Down

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM org_events) THEN
    RAISE EXCEPTION 'cannot roll back core migration 0020: org event journal rows exist';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE org_webhook_deliveries
  DROP COLUMN org_event_id;

DROP POLICY tenant_isolation_org_events ON org_events;
DROP INDEX idx_org_events_fanout_owed;
DROP TABLE org_events;
