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

DROP INDEX IF EXISTS idx_outbox_events_outbox_msg;
ALTER TABLE outbox_events
  ALTER COLUMN provider_message_id SET NOT NULL;
DROP INDEX IF EXISTS idx_suppressions_org_created;
DROP TABLE IF EXISTS suppressions;
