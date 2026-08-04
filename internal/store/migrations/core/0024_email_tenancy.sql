-- +goose Up

ALTER TABLE orgs ADD COLUMN external_ref text;
ALTER TABLE orgs ADD COLUMN deleted_at timestamptz;
CREATE UNIQUE INDEX uq_orgs_external_ref ON orgs (external_ref) WHERE external_ref IS NOT NULL;

ALTER TABLE org_domains ADD COLUMN external_ref text;
CREATE UNIQUE INDEX uq_org_domains_external_ref ON org_domains (external_ref) WHERE external_ref IS NOT NULL;

ALTER TABLE inboxes ADD COLUMN external_ref text;
CREATE UNIQUE INDEX uq_inboxes_external_ref ON inboxes (external_ref) WHERE external_ref IS NOT NULL;

ALTER TABLE org_webhooks ADD COLUMN external_ref text;
CREATE UNIQUE INDEX uq_org_webhooks_external_ref ON org_webhooks (external_ref) WHERE external_ref IS NOT NULL;

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT lower(address) FROM inboxes GROUP BY lower(address) HAVING count(*) > 1
  ) THEN
    RAISE EXCEPTION 'cannot apply core migration 0024: duplicate canonical inbox addresses exist';
  END IF;
END;
$$;
-- +goose StatementEnd

CREATE UNIQUE INDEX uq_inboxes_canonical_address ON inboxes (lower(address));

CREATE TABLE org_domain_grants (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  owner_org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE RESTRICT,
  org_domain_id uuid NOT NULL REFERENCES org_domains(id) ON DELETE RESTRICT,
  grantee_org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE RESTRICT,
  external_ref text NOT NULL UNIQUE,
  status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'revoked')),
  created_at timestamptz NOT NULL DEFAULT now(),
  revoked_at timestamptz,
  CHECK (owner_org_id <> grantee_org_id)
);
CREATE UNIQUE INDEX uq_org_domain_grants_active
  ON org_domain_grants (org_domain_id, grantee_org_id) WHERE status = 'active';
CREATE INDEX idx_org_domain_grants_grantee
  ON org_domain_grants (grantee_org_id, org_domain_id);

-- +goose StatementBegin
CREATE FUNCTION enforce_inbox_domain_access() RETURNS trigger AS $$
BEGIN
  IF NEW.org_domain_id IS NULL THEN
    RETURN NEW;
  END IF;
  PERFORM 1 FROM org_domains d
  WHERE d.id = NEW.org_domain_id
    AND d.status = 'active'
    AND d.org_id = NEW.org_id;
  IF FOUND THEN
    RETURN NEW;
  END IF;

  -- Pair this key-share lock with RevokeOrgDomainGrant's row lock so inbox
  -- activation and grant revocation cannot race past one another.
  PERFORM 1
  FROM org_domain_grants g
  JOIN org_domains d ON d.id = g.org_domain_id
  WHERE d.id = NEW.org_domain_id
    AND d.status = 'active'
    AND g.grantee_org_id = NEW.org_id
    AND g.status = 'active'
  FOR KEY SHARE OF g;
  IF FOUND THEN
    RETURN NEW;
  END IF;

  RAISE EXCEPTION 'inbox org has no active access to domain' USING ERRCODE = '23514';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER trg_enforce_inbox_domain_access
  BEFORE INSERT OR UPDATE OF org_id, org_domain_id, status ON inboxes
  FOR EACH ROW EXECUTE FUNCTION enforce_inbox_domain_access();

ALTER TABLE org_domain_grants ENABLE ROW LEVEL SECURITY;
ALTER TABLE org_domain_grants FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_read_org_domain_grants ON org_domain_grants
  FOR SELECT USING (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR owner_org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
    OR grantee_org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  );
CREATE POLICY tenant_write_org_domain_grants ON org_domain_grants
  FOR ALL USING (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR owner_org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  ) WITH CHECK (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR owner_org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  );

DROP POLICY tenant_isolation_org_domains ON org_domains;
CREATE POLICY tenant_read_org_domains ON org_domains
  FOR SELECT USING (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
    OR EXISTS (
      SELECT 1 FROM org_domain_grants g
      WHERE g.org_domain_id = org_domains.id
        AND g.grantee_org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
        AND g.status = 'active'
    )
  );
CREATE POLICY tenant_write_org_domains ON org_domains
  FOR ALL USING (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  ) WITH CHECK (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  );

-- +goose Down

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM org_domain_grants) THEN
    RAISE EXCEPTION 'cannot roll back core migration 0024: organization domain grants exist';
  END IF;
END $$;
-- +goose StatementEnd

DROP POLICY tenant_write_org_domains ON org_domains;
DROP POLICY tenant_read_org_domains ON org_domains;
CREATE POLICY tenant_isolation_org_domains ON org_domains
  USING (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  ) WITH CHECK (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  );
DROP TRIGGER trg_enforce_inbox_domain_access ON inboxes;
DROP FUNCTION enforce_inbox_domain_access();
DROP TABLE org_domain_grants;
DROP INDEX uq_inboxes_canonical_address;
DROP INDEX uq_org_webhooks_external_ref;
ALTER TABLE org_webhooks DROP COLUMN external_ref;
DROP INDEX uq_inboxes_external_ref;
ALTER TABLE inboxes DROP COLUMN external_ref;
DROP INDEX uq_org_domains_external_ref;
ALTER TABLE org_domains DROP COLUMN external_ref;
DROP INDEX uq_orgs_external_ref;
ALTER TABLE orgs DROP COLUMN deleted_at;
ALTER TABLE orgs DROP COLUMN external_ref;
