-- +goose Up

ALTER TABLE outbox_messages
  ADD COLUMN created_at timestamptz;

-- Legacy rows predate a dedicated creation timestamp. Preserve the earliest
-- durable timestamp still available on the row or its event timeline, then
-- keep that value immutable while scheduling fields continue to change.
UPDATE outbox_messages AS outbox
SET created_at = LEAST(
  outbox.next_attempt_at,
  COALESCE(outbox.last_attempt_at, outbox.next_attempt_at),
  COALESCE(outbox.delivery_status_at, outbox.next_attempt_at),
  COALESCE(
    (
      SELECT min(event.created_at)
      FROM outbox_events AS event
      WHERE event.outbox_message_id = outbox.id
    ),
    outbox.next_attempt_at
  )
);

ALTER TABLE outbox_messages
  ALTER COLUMN created_at SET DEFAULT now(),
  ALTER COLUMN created_at SET NOT NULL;

CREATE INDEX idx_outbox_inbox_created_at
  ON outbox_messages (inbox_id, created_at DESC, id DESC);

CREATE INDEX idx_outbox_failed_created_at
  ON outbox_messages (org_id, created_at DESC, id DESC)
  WHERE status = 'failed';

-- +goose Down

DROP INDEX IF EXISTS idx_outbox_failed_created_at;
DROP INDEX IF EXISTS idx_outbox_inbox_created_at;

ALTER TABLE outbox_messages
  DROP COLUMN IF EXISTS created_at;
