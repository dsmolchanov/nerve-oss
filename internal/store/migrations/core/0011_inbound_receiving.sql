-- +goose Up

-- Add resend_receiving_enabled + catch_all_enabled to org_domains
ALTER TABLE org_domains
  ADD COLUMN IF NOT EXISTS resend_receiving_enabled boolean NOT NULL DEFAULT false,
  ADD COLUMN IF NOT EXISTS catch_all_enabled boolean NOT NULL DEFAULT false;

-- Add delivery_status to outbox_messages for webhook-driven tracking
ALTER TABLE outbox_messages
  ADD COLUMN IF NOT EXISTS delivery_status text NOT NULL DEFAULT 'unknown',
  ADD COLUMN IF NOT EXISTS delivery_status_at timestamptz;

-- Add threading headers + raw_headers to messages for RFC 5322 threading and debugging
ALTER TABLE messages
  ADD COLUMN IF NOT EXISTS in_reply_to text,
  ADD COLUMN IF NOT EXISTS "references" text[],
  ADD COLUMN IF NOT EXISTS raw_headers jsonb;

-- Add received_email_id to messages for on-demand attachment fetching
-- (Resend download URLs expire, so we store the stable ID and re-fetch when needed)
ALTER TABLE messages
  ADD COLUMN IF NOT EXISTS received_email_id text;

-- Index for looking up outbox messages by provider_message_id (for delivery events)
CREATE INDEX IF NOT EXISTS idx_outbox_provider_msg
  ON outbox_messages (provider_message_id)
  WHERE provider_message_id IS NOT NULL;

-- Append-only delivery event timeline for outbound observability
CREATE TABLE IF NOT EXISTS outbox_events (
  id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id              uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  outbox_message_id   uuid NOT NULL REFERENCES outbox_messages(id) ON DELETE CASCADE,
  provider_message_id text NOT NULL,
  event_type          text NOT NULL,
  raw_payload         jsonb NOT NULL,
  reason              text,
  created_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_outbox_events_msg
  ON outbox_events (provider_message_id, event_type);

-- Monotonic delivery status severity function.
-- Prevents out-of-order webhook events from regressing status
-- (e.g., "delivery_delayed" arriving after "delivered").
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION delivery_status_severity(status text) RETURNS integer AS $$
  SELECT CASE status
    WHEN 'unknown' THEN 0
    WHEN 'queued' THEN 1
    WHEN 'sent' THEN 2
    WHEN 'delivery_delayed' THEN 3
    WHEN 'delivered' THEN 4
    WHEN 'bounced' THEN 5
    WHEN 'failed' THEN 6
    WHEN 'complained' THEN 7
    WHEN 'suppressed' THEN 8
    ELSE 0
  END;
$$ LANGUAGE sql IMMUTABLE;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION IF EXISTS delivery_status_severity(text);
DROP INDEX IF EXISTS idx_outbox_events_msg;
DROP TABLE IF EXISTS outbox_events;
DROP INDEX IF EXISTS idx_outbox_provider_msg;
ALTER TABLE messages DROP COLUMN IF EXISTS received_email_id;
ALTER TABLE messages DROP COLUMN IF EXISTS raw_headers;
ALTER TABLE messages DROP COLUMN IF EXISTS "references";
ALTER TABLE messages DROP COLUMN IF EXISTS in_reply_to;
ALTER TABLE outbox_messages DROP COLUMN IF EXISTS delivery_status_at;
ALTER TABLE outbox_messages DROP COLUMN IF EXISTS delivery_status;
ALTER TABLE org_domains DROP COLUMN IF EXISTS catch_all_enabled;
ALTER TABLE org_domains DROP COLUMN IF EXISTS resend_receiving_enabled;
