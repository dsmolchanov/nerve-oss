-- +goose Up

CREATE TABLE org_feature_flags (
  id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id     uuid        REFERENCES orgs(id) ON DELETE CASCADE,
  flag       text        NOT NULL CHECK (btrim(flag) <> ''),
  enabled    boolean     NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now(),
  updated_by text        NOT NULL CHECK (btrim(updated_by) <> '')
);

CREATE UNIQUE INDEX uq_org_feature_flags_org
  ON org_feature_flags (org_id, flag)
  WHERE org_id IS NOT NULL;

CREATE UNIQUE INDEX uq_org_feature_flags_global
  ON org_feature_flags (flag)
  WHERE org_id IS NULL;

CREATE INDEX idx_org_feature_flags_lookup
  ON org_feature_flags (flag, org_id);

ALTER TABLE org_feature_flags ENABLE ROW LEVEL SECURITY;
ALTER TABLE org_feature_flags FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_org_feature_flags ON org_feature_flags
  USING (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id IS NULL
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  )
  WITH CHECK (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR (
      org_id IS NOT NULL
      AND org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
    )
  );

-- +goose Down

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM org_feature_flags) THEN
    RAISE EXCEPTION 'cannot roll back core migration 0026: feature flag rows exist';
  END IF;
END $$;
-- +goose StatementEnd

DROP POLICY tenant_isolation_org_feature_flags ON org_feature_flags;
DROP TABLE org_feature_flags;
