-- +goose Up

CREATE TABLE IF NOT EXISTS outbox_messages (
  id uuid PRIMARY KEY,
  org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  inbox_id uuid NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  provider text NOT NULL,
  provider_message_id text,
  idempotency_key text NOT NULL,
  "to" text NOT NULL,
  "from" text NOT NULL,
  subject text NOT NULL,
  text_body text,
  html_body text,
  status text NOT NULL DEFAULT 'queued',
  attempt_count int NOT NULL DEFAULT 0,
  next_attempt_at timestamptz NOT NULL DEFAULT now(),
  last_attempt_at timestamptz,
  last_error text,
  locked_at timestamptz,
  locked_by text,
  CONSTRAINT chk_outbox_status CHECK (status IN ('queued', 'sending', 'sent', 'failed')),
  UNIQUE(org_id, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_outbox_next_attempt ON outbox_messages(status, next_attempt_at);

-- +goose Down

DROP INDEX IF EXISTS idx_outbox_next_attempt;
DROP TABLE IF EXISTS outbox_messages;

