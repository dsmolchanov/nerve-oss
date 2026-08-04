-- +goose Up

-- Forward repair for databases that recorded core version 17 before the
-- corrected 0016/0017 files shipped. Replaying an edited historical migration
-- is not possible in Goose, so establish the same tenant-isolation and active
-- webhook identity guarantees at a new version.

-- Refuse rather than silently choosing which duplicate endpoint to disable.
-- The operator must preserve the intended subscription and explicitly disable
-- every duplicate active row before retrying this migration.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1
    FROM org_webhooks
    WHERE disabled_at IS NULL
    GROUP BY org_id, url
    HAVING count(*) > 1
  ) THEN
    RAISE EXCEPTION
      'cannot apply core migration 0018: duplicate active org_webhooks (org_id, url) rows exist';
  END IF;
END;
$$;
-- +goose StatementEnd

ALTER TABLE outbox_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox_events FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_outbox_events ON outbox_events;
CREATE POLICY tenant_isolation_outbox_events ON outbox_events
  USING (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  )
  WITH CHECK (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  );

ALTER TABLE inbox_smtp_configs ENABLE ROW LEVEL SECURITY;
ALTER TABLE inbox_smtp_configs FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_inbox_smtp_configs ON inbox_smtp_configs;
CREATE POLICY tenant_isolation_inbox_smtp_configs ON inbox_smtp_configs
  USING (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  )
  WITH CHECK (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  );

ALTER TABLE suppressions ENABLE ROW LEVEL SECURITY;
ALTER TABLE suppressions FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_suppressions ON suppressions;
CREATE POLICY tenant_isolation_suppressions ON suppressions
  USING (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  )
  WITH CHECK (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  );

DROP INDEX IF EXISTS idx_org_webhooks_org_active;
CREATE UNIQUE INDEX idx_org_webhooks_org_active
  ON org_webhooks (org_id, url)
  WHERE disabled_at IS NULL;

-- +goose Down

-- The version-17 source files already require these protections. Rolling the
-- repair marker back must not recreate the legacy insecure schema, so reassert
-- the corrected version-17 state rather than removing RLS or uniqueness.
ALTER TABLE outbox_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox_events FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_outbox_events ON outbox_events;
CREATE POLICY tenant_isolation_outbox_events ON outbox_events
  USING (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  )
  WITH CHECK (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  );

ALTER TABLE inbox_smtp_configs ENABLE ROW LEVEL SECURITY;
ALTER TABLE inbox_smtp_configs FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_inbox_smtp_configs ON inbox_smtp_configs;
CREATE POLICY tenant_isolation_inbox_smtp_configs ON inbox_smtp_configs
  USING (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  )
  WITH CHECK (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  );

ALTER TABLE suppressions ENABLE ROW LEVEL SECURITY;
ALTER TABLE suppressions FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_suppressions ON suppressions;
CREATE POLICY tenant_isolation_suppressions ON suppressions
  USING (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  )
  WITH CHECK (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  );

DROP INDEX IF EXISTS idx_org_webhooks_org_active;
CREATE UNIQUE INDEX idx_org_webhooks_org_active
  ON org_webhooks (org_id, url)
  WHERE disabled_at IS NULL;
