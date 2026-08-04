-- +goose Up

ALTER TABLE messages
  ADD CONSTRAINT uq_messages_org_id UNIQUE (org_id, id),
  ADD COLUMN attachments_state text NOT NULL DEFAULT 'pending_backfill'
    CHECK (attachments_state IN ('known', 'pending_backfill', 'unknown_metadata_expired'));

-- Old writers remain live during the additive rollout. Provider-backed
-- inbound messages must stay pending for metadata backfill, while outbound and
-- providerless rows can never be backfilled and are known-empty immediately.
-- +goose StatementBegin
CREATE FUNCTION classify_providerless_message_attachments() RETURNS trigger AS $$
BEGIN
  IF NEW.direction = 'outbound'
     OR NEW.received_email_id IS NULL
     OR NEW.received_email_id = '' THEN
    NEW.attachments_state = 'known';
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER trg_messages_classify_providerless_attachments
BEFORE INSERT ON messages
FOR EACH ROW EXECUTE FUNCTION classify_providerless_message_attachments();

UPDATE messages
SET attachments_state = 'known'
WHERE direction = 'outbound' OR received_email_id IS NULL OR received_email_id = '';

UPDATE messages
SET attachments_state = 'unknown_metadata_expired'
WHERE attachments_state = 'pending_backfill'
  AND created_at < now() - interval '30 days';

CREATE TABLE message_attachments (
  id                     uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id                 uuid        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  message_id             uuid        NOT NULL,
  ordinal                int         NOT NULL CHECK (ordinal >= 0),
  provider_attachment_id text        NOT NULL,
  filename               text        NOT NULL DEFAULT '',
  content_type           text        NOT NULL DEFAULT 'application/octet-stream',
  content_disposition    text        NOT NULL DEFAULT '',
  content_id             text        NOT NULL DEFAULT '',
  size_bytes             bigint      CHECK (size_bytes IS NULL OR size_bytes >= 0),
  availability           text        NOT NULL DEFAULT 'pending'
    CHECK (availability IN ('pending', 'available', 'expired', 'too_large', 'failed')),
  blob_sha256            text,
  attempt_count          int         NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
  next_attempt_at        timestamptz NOT NULL DEFAULT now(),
  locked_at              timestamptz,
  locked_by              text,
  last_error             text,
  mirrored_at            timestamptz,
  created_at             timestamptz NOT NULL DEFAULT now(),
  UNIQUE (message_id, provider_attachment_id),
  UNIQUE (org_id, id),
  FOREIGN KEY (org_id, message_id) REFERENCES messages (org_id, id) ON DELETE CASCADE,
  FOREIGN KEY (org_id, blob_sha256) REFERENCES attachment_blobs (org_id, sha256)
);

CREATE INDEX idx_message_attachments_claim
  ON message_attachments (next_attempt_at)
  WHERE availability = 'pending';

ALTER TABLE message_attachments ENABLE ROW LEVEL SECURITY;
ALTER TABLE message_attachments FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_message_attachments ON message_attachments
  USING (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  )
  WITH CHECK (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  );

ALTER TABLE attachments ENABLE ROW LEVEL SECURITY;
ALTER TABLE attachments FORCE ROW LEVEL SECURITY;
CREATE POLICY deny_all_legacy_attachments ON attachments
  USING (false)
  WITH CHECK (false);
COMMENT ON TABLE attachments IS
  'DEPRECATED 2026-08 — superseded by message_attachments. Deny-all RLS. Drop after 0024 has been live 30 days.';

-- +goose StatementBegin
CREATE FUNCTION update_attachment_blob_ref_count() RETURNS trigger AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.blob_sha256 IS NOT NULL THEN
      UPDATE attachment_blobs
      SET ref_count = ref_count + 1, last_ref_at = now()
      WHERE org_id = NEW.org_id AND sha256 = NEW.blob_sha256;
    END IF;
    RETURN NEW;
  END IF;

  IF TG_OP = 'UPDATE' THEN
    IF OLD.blob_sha256 IS DISTINCT FROM NEW.blob_sha256 THEN
      IF OLD.blob_sha256 IS NOT NULL THEN
        UPDATE attachment_blobs
        SET ref_count = ref_count - 1
        WHERE org_id = OLD.org_id AND sha256 = OLD.blob_sha256;
      END IF;
      IF NEW.blob_sha256 IS NOT NULL THEN
        UPDATE attachment_blobs
        SET ref_count = ref_count + 1, last_ref_at = now()
        WHERE org_id = NEW.org_id AND sha256 = NEW.blob_sha256;
      END IF;
    END IF;
    RETURN NEW;
  END IF;

  IF OLD.blob_sha256 IS NOT NULL THEN
    UPDATE attachment_blobs
    SET ref_count = ref_count - 1
    WHERE org_id = OLD.org_id AND sha256 = OLD.blob_sha256;
  END IF;
  RETURN OLD;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER trg_message_attachments_blob_ref_insert
AFTER INSERT ON message_attachments
FOR EACH ROW EXECUTE FUNCTION update_attachment_blob_ref_count();

CREATE TRIGGER trg_message_attachments_blob_ref_update
AFTER UPDATE OF blob_sha256 ON message_attachments
FOR EACH ROW EXECUTE FUNCTION update_attachment_blob_ref_count();

CREATE TRIGGER trg_message_attachments_blob_ref_delete
AFTER DELETE ON message_attachments
FOR EACH ROW EXECUTE FUNCTION update_attachment_blob_ref_count();

-- +goose Down

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM message_attachments) THEN
    RAISE EXCEPTION 'cannot roll back core migration 0023: message attachment metadata exists';
  END IF;
END $$;
-- +goose StatementEnd

DROP TRIGGER trg_message_attachments_blob_ref_delete ON message_attachments;
DROP TRIGGER trg_message_attachments_blob_ref_update ON message_attachments;
DROP TRIGGER trg_message_attachments_blob_ref_insert ON message_attachments;
DROP FUNCTION update_attachment_blob_ref_count();

DROP POLICY tenant_isolation_message_attachments ON message_attachments;
DROP INDEX idx_message_attachments_claim;
DROP TABLE message_attachments;

DROP POLICY deny_all_legacy_attachments ON attachments;
COMMENT ON TABLE attachments IS NULL;
ALTER TABLE attachments NO FORCE ROW LEVEL SECURITY;
ALTER TABLE attachments DISABLE ROW LEVEL SECURITY;

DROP TRIGGER trg_messages_classify_providerless_attachments ON messages;
DROP FUNCTION classify_providerless_message_attachments();

ALTER TABLE messages
  DROP COLUMN attachments_state,
  DROP CONSTRAINT uq_messages_org_id;
