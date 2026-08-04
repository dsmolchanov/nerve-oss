-- +goose Up

CREATE TABLE attachment_blobs (
  org_id       uuid        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  sha256       text        NOT NULL,
  size_bytes   bigint      NOT NULL CHECK (size_bytes > 0),
  content_type text        NOT NULL,
  content      bytea       NOT NULL,
  ref_count    int         NOT NULL DEFAULT 0 CHECK (ref_count >= 0),
  created_at   timestamptz NOT NULL DEFAULT now(),
  last_ref_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (org_id, sha256)
);

CREATE TABLE org_attachment_usage (
  org_id      uuid        PRIMARY KEY REFERENCES orgs(id) ON DELETE CASCADE,
  bytes_used  bigint      NOT NULL DEFAULT 0 CHECK (bytes_used >= 0),
  bytes_quota bigint      NOT NULL DEFAULT 2147483648 CHECK (bytes_quota >= 0),
  updated_at  timestamptz NOT NULL DEFAULT now()
);

INSERT INTO org_attachment_usage (org_id)
SELECT id FROM orgs
ON CONFLICT DO NOTHING;

ALTER TABLE attachment_blobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE attachment_blobs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_attachment_blobs ON attachment_blobs
  USING (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  )
  WITH CHECK (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  );

ALTER TABLE org_attachment_usage ENABLE ROW LEVEL SECURITY;
ALTER TABLE org_attachment_usage FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_org_attachment_usage ON org_attachment_usage
  USING (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  )
  WITH CHECK (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  );

-- +goose Down

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM attachment_blobs) THEN
    RAISE EXCEPTION 'cannot roll back core migration 0022: attachment blobs exist';
  END IF;
END $$;
-- +goose StatementEnd

DROP POLICY tenant_isolation_org_attachment_usage ON org_attachment_usage;
DROP POLICY tenant_isolation_attachment_blobs ON attachment_blobs;
DROP TABLE org_attachment_usage;
DROP TABLE attachment_blobs;
