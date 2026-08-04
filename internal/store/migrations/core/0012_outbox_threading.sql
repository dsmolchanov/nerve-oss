-- +goose Up

-- Add threading headers to outbox_messages for reply-chain continuity.
-- When an agent replies to an inbound email, the outbox worker sets
-- In-Reply-To and References headers so the recipient's email client
-- threads the reply correctly.

ALTER TABLE outbox_messages
  ADD COLUMN IF NOT EXISTS in_reply_to_message_id text,
  ADD COLUMN IF NOT EXISTS "references" text;

-- +goose Down
ALTER TABLE outbox_messages DROP COLUMN IF EXISTS "references";
ALTER TABLE outbox_messages DROP COLUMN IF EXISTS in_reply_to_message_id;
