-- +goose Up

-- B2: Per-org suppression list for hard bounces, complaints, and manual blocks.
-- Subsequent enqueues to a suppressed (org_id, email) pair short-circuit to
-- delivery_status='suppressed' without hitting the provider, preventing
-- reputational damage and wasted retry budget.
CREATE TABLE IF NOT EXISTS suppressions (
  org_id      uuid        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  email_lower text        NOT NULL,
  reason      text        NOT NULL,
  source      text        NOT NULL CHECK (source IN ('bounce', 'complaint', 'manual')),
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (org_id, email_lower)
);

CREATE INDEX IF NOT EXISTS idx_suppressions_org_created
  ON suppressions (org_id, created_at DESC);

-- Pre-link outbox_events: relax provider_message_id NOT NULL so events can
-- be appended for messages that never reached a provider (suppression at
-- enqueue, future pre-flight rejection, etc.). The outbox_message_id column
-- is the durable primary link and is already NOT NULL from migration 0011.
ALTER TABLE outbox_events
  ALTER COLUMN provider_message_id DROP NOT NULL;

-- Index outbox_message_id for direct event lookups (B5 status endpoint
-- and operator timeline view in B3).
CREATE INDEX IF NOT EXISTS idx_outbox_events_outbox_msg
  ON outbox_events (outbox_message_id, created_at DESC);

-- +goose Down

-- Rows without a provider_message_id are valid from this migration onward.
-- Restoring NOT NULL cannot preserve those audit events, and inventing a
-- provider id would make the timeline dishonest. Refuse the rollback before
-- changing any schema so the operator can explicitly resolve the data first.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1
    FROM outbox_events
    WHERE provider_message_id IS NULL
  ) THEN
    RAISE EXCEPTION
      'cannot roll back core migration 0016: outbox_events.provider_message_id contains NULL rows';
  END IF;
END;
$$;
-- +goose StatementEnd

DROP INDEX IF EXISTS idx_outbox_events_outbox_msg;
ALTER TABLE outbox_events
  ALTER COLUMN provider_message_id SET NOT NULL;
DROP INDEX IF EXISTS idx_suppressions_org_created;
DROP TABLE IF EXISTS suppressions;
