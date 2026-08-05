-- +goose Up

CREATE TABLE outbox_delivery_holds (
  id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id             uuid        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  idempotency_key    text        NOT NULL CHECK (btrim(idempotency_key) <> ''),
  reason             text        NOT NULL CHECK (btrim(reason) <> ''),
  held_by            text        NOT NULL CHECK (btrim(held_by) <> ''),
  hold_replay_id     uuid        NOT NULL,
  created_at         timestamptz NOT NULL,
  expires_at         timestamptz NOT NULL,
  released_at        timestamptz,
  released_by        text,
  release_replay_id  uuid,
  CHECK (expires_at > created_at),
  CHECK (
    (released_at IS NULL AND released_by IS NULL AND release_replay_id IS NULL)
    OR
    (released_at IS NOT NULL AND released_by IS NOT NULL AND btrim(released_by) <> '' AND release_replay_id IS NOT NULL)
  )
);

CREATE UNIQUE INDEX uq_outbox_delivery_holds_active
  ON outbox_delivery_holds (org_id, idempotency_key)
  WHERE released_at IS NULL;

CREATE INDEX idx_outbox_delivery_holds_claim
  ON outbox_delivery_holds (org_id, idempotency_key, expires_at)
  WHERE released_at IS NULL;

ALTER TABLE outbox_delivery_holds ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox_delivery_holds FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_outbox_delivery_holds ON outbox_delivery_holds
  USING (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  )
  WITH CHECK (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  );

-- +goose Down

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM outbox_delivery_holds) THEN
    RAISE EXCEPTION 'cannot roll back core migration 0028: outbox delivery hold history exists';
  END IF;
END $$;
-- +goose StatementEnd

DROP POLICY tenant_isolation_outbox_delivery_holds ON outbox_delivery_holds;
DROP TABLE outbox_delivery_holds;
