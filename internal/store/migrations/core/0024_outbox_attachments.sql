-- +goose Up

ALTER TABLE outbox_messages
  ADD CONSTRAINT uq_outbox_messages_org_id UNIQUE (org_id, id),
  ADD COLUMN terminal_at timestamptz,
  ADD COLUMN attachments_released_at timestamptz;

UPDATE outbox_messages
SET terminal_at = now()
WHERE status IN ('sent', 'failed');

CREATE INDEX idx_outbox_release_owed ON outbox_messages (terminal_at)
  WHERE terminal_at IS NOT NULL AND attachments_released_at IS NULL;

CREATE TABLE outbox_attachments (
  id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id            uuid        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  outbox_message_id uuid        NOT NULL,
  ordinal           int         NOT NULL CHECK (ordinal >= 0),
  filename          text        NOT NULL,
  content_type      text        NOT NULL,
  size_bytes        bigint      NOT NULL CHECK (size_bytes > 0),
  sha256            text        NOT NULL,
  blob_sha256       text,
  created_at        timestamptz NOT NULL DEFAULT now(),
  UNIQUE (outbox_message_id, ordinal),
  UNIQUE (org_id, id),
  FOREIGN KEY (org_id, outbox_message_id) REFERENCES outbox_messages (org_id, id) ON DELETE CASCADE,
  FOREIGN KEY (org_id, blob_sha256) REFERENCES attachment_blobs (org_id, sha256)
);

ALTER TABLE outbox_attachments ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox_attachments FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_outbox_attachments ON outbox_attachments
  USING (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  )
  WITH CHECK (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  );

CREATE TRIGGER trg_outbox_attachments_blob_ref_insert
AFTER INSERT ON outbox_attachments
FOR EACH ROW EXECUTE FUNCTION update_attachment_blob_ref_count();

CREATE TRIGGER trg_outbox_attachments_blob_ref_update
AFTER UPDATE OF blob_sha256 ON outbox_attachments
FOR EACH ROW EXECUTE FUNCTION update_attachment_blob_ref_count();

CREATE TRIGGER trg_outbox_attachments_blob_ref_delete
AFTER DELETE ON outbox_attachments
FOR EACH ROW EXECUTE FUNCTION update_attachment_blob_ref_count();

-- +goose Down

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM outbox_attachments) THEN
    RAISE EXCEPTION 'cannot roll back core migration 0024: outbox attachment metadata exists';
  END IF;
END $$;
-- +goose StatementEnd

DROP TRIGGER trg_outbox_attachments_blob_ref_delete ON outbox_attachments;
DROP TRIGGER trg_outbox_attachments_blob_ref_update ON outbox_attachments;
DROP TRIGGER trg_outbox_attachments_blob_ref_insert ON outbox_attachments;
DROP POLICY tenant_isolation_outbox_attachments ON outbox_attachments;
DROP TABLE outbox_attachments;

DROP INDEX idx_outbox_release_owed;
ALTER TABLE outbox_messages
  DROP COLUMN attachments_released_at,
  DROP COLUMN terminal_at,
  DROP CONSTRAINT uq_outbox_messages_org_id;
