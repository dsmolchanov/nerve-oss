-- +goose Up

CREATE TABLE org_outbound_policy_state (
  org_id        uuid        PRIMARY KEY REFERENCES orgs(id) ON DELETE CASCADE,
  policy_epoch  bigint      NOT NULL DEFAULT 1 CHECK (policy_epoch > 0),
  updated_at    timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE org_outbound_policy_state ENABLE ROW LEVEL SECURITY;
ALTER TABLE org_outbound_policy_state FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_org_outbound_policy_state ON org_outbound_policy_state
  USING (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  )
  WITH CHECK (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  );

ALTER TABLE outbox_messages
  ADD COLUMN autonomous_policy_epoch bigint,
  ADD COLUMN provider_started_at timestamptz,
  ADD COLUMN provider_operation_id text,
  ADD COLUMN provider_resolved_at timestamptz,
  ADD CONSTRAINT chk_outbox_autonomous_policy_epoch
    CHECK (autonomous_policy_epoch IS NULL OR autonomous_policy_epoch > 0),
  ADD CONSTRAINT chk_outbox_provider_fence_shape
    CHECK (
      (provider_started_at IS NULL AND provider_operation_id IS NULL AND provider_resolved_at IS NULL)
      OR
      (
        autonomous_policy_epoch IS NOT NULL
        AND provider_started_at IS NOT NULL
        AND provider_operation_id IS NOT NULL
        AND btrim(provider_operation_id) <> ''
        AND (provider_resolved_at IS NULL OR provider_resolved_at >= provider_started_at)
      )
    );

CREATE INDEX idx_outbox_autonomous_claim
  ON outbox_messages (org_id, autonomous_policy_epoch, status, next_attempt_at)
  WHERE autonomous_policy_epoch IS NOT NULL AND status IN ('queued', 'sending');

CREATE INDEX idx_outbox_unresolved_provider_start
  ON outbox_messages (org_id, provider_started_at, id)
  WHERE provider_started_at IS NOT NULL AND provider_resolved_at IS NULL;

-- +goose Down

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM org_outbound_policy_state) THEN
    RAISE EXCEPTION 'cannot roll back core migration 0029: outbound policy epoch rows exist';
  END IF;
  IF EXISTS (
    SELECT 1
    FROM outbox_messages
    WHERE autonomous_policy_epoch IS NOT NULL
       OR provider_started_at IS NOT NULL
       OR provider_operation_id IS NOT NULL
       OR provider_resolved_at IS NOT NULL
  ) THEN
    RAISE EXCEPTION 'cannot roll back core migration 0029: outbox provider fence evidence exists';
  END IF;
END $$;
-- +goose StatementEnd

DROP INDEX idx_outbox_unresolved_provider_start;
DROP INDEX idx_outbox_autonomous_claim;

ALTER TABLE outbox_messages
  DROP CONSTRAINT chk_outbox_provider_fence_shape,
  DROP CONSTRAINT chk_outbox_autonomous_policy_epoch,
  DROP COLUMN provider_resolved_at,
  DROP COLUMN provider_operation_id,
  DROP COLUMN provider_started_at,
  DROP COLUMN autonomous_policy_epoch;

DROP POLICY tenant_isolation_org_outbound_policy_state ON org_outbound_policy_state;
DROP TABLE org_outbound_policy_state;
