-- +goose Up

-- Notify the outbox worker immediately when a new message is enqueued,
-- so it can wake from LISTEN instead of polling on a timer.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION notify_outbox_insert() RETURNS trigger AS $$
BEGIN
  PERFORM pg_notify('outbox_new', NEW.id::text);
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

DROP TRIGGER IF EXISTS trg_outbox_notify ON outbox_messages;

CREATE TRIGGER trg_outbox_notify
  AFTER INSERT ON outbox_messages
  FOR EACH ROW
  EXECUTE FUNCTION notify_outbox_insert();
