-- +goose Up
ALTER TABLE inboxes ENABLE ROW LEVEL SECURITY;
ALTER TABLE threads ENABLE ROW LEVEL SECURITY;
ALTER TABLE messages ENABLE ROW LEVEL SECURITY;

ALTER TABLE inboxes FORCE ROW LEVEL SECURITY;
ALTER TABLE threads FORCE ROW LEVEL SECURITY;
ALTER TABLE messages FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_inboxes ON inboxes;
DROP POLICY IF EXISTS tenant_isolation_threads ON threads;
DROP POLICY IF EXISTS tenant_isolation_messages ON messages;

CREATE POLICY tenant_isolation_inboxes ON inboxes
  USING (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  )
  WITH CHECK (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  );

CREATE POLICY tenant_isolation_threads ON threads
  USING (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  )
  WITH CHECK (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  );

CREATE POLICY tenant_isolation_messages ON messages
  USING (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  )
  WITH CHECK (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  );

-- +goose Down
DROP POLICY IF EXISTS tenant_isolation_messages ON messages;
DROP POLICY IF EXISTS tenant_isolation_threads ON threads;
DROP POLICY IF EXISTS tenant_isolation_inboxes ON inboxes;

ALTER TABLE messages NO FORCE ROW LEVEL SECURITY;
ALTER TABLE threads NO FORCE ROW LEVEL SECURITY;
ALTER TABLE inboxes NO FORCE ROW LEVEL SECURITY;

ALTER TABLE messages DISABLE ROW LEVEL SECURITY;
ALTER TABLE threads DISABLE ROW LEVEL SECURITY;
ALTER TABLE inboxes DISABLE ROW LEVEL SECURITY;
