-- +goose Up
ALTER TABLE outbox_messages ADD COLUMN IF NOT EXISTS content_hash text;
CREATE UNIQUE INDEX IF NOT EXISTS idx_outbox_content_dedup
  ON outbox_messages (org_id, inbox_id, content_hash)
  WHERE status IN ('queued', 'sending') AND content_hash IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_outbox_content_dedup;
ALTER TABLE outbox_messages DROP COLUMN IF EXISTS content_hash;
