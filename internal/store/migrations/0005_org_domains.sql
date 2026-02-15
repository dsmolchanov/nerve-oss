-- +goose Up

CREATE TABLE IF NOT EXISTS org_domains (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  domain text NOT NULL,
  status text NOT NULL DEFAULT 'pending',
    -- pending: awaiting DNS verification (auto-expires after 7 days)
    -- verified_dns: DNS checks passed, awaiting Stalwart provisioning
    -- provisioning: Stalwart domain/account creation in progress
    -- active: fully operational (outbound + optional inbound)
    -- failed: DNS verification failed
  verification_token text NOT NULL,
  mx_verified boolean NOT NULL DEFAULT false,
  spf_verified boolean NOT NULL DEFAULT false,
  dkim_verified boolean NOT NULL DEFAULT false,
  dmarc_verified boolean NOT NULL DEFAULT false,
  inbound_enabled boolean NOT NULL DEFAULT false,
  dkim_selector text NOT NULL DEFAULT 'nerve',
  dkim_private_key_enc text,
  dkim_public_key text,
  dkim_method text NOT NULL DEFAULT 'cname',
  last_check_at timestamptz,
  verified_at timestamptz,
  expires_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT chk_domain_canonical CHECK (domain = lower(domain) AND domain NOT LIKE '%.')
);

-- Only verified/active domains enforce uniqueness (prevents domain-claim DoS)
CREATE UNIQUE INDEX idx_org_domains_verified ON org_domains(lower(domain))
  WHERE status IN ('verified_dns', 'provisioning', 'active');
-- Allow multiple pending claims for the same domain (they expire)
CREATE INDEX idx_org_domains_domain ON org_domains(lower(domain));
CREATE INDEX idx_org_domains_org ON org_domains(org_id);
CREATE INDEX idx_org_domains_expires ON org_domains(expires_at) WHERE status = 'pending';

-- RLS for org_domains (same pattern as inboxes in 0003_tenant_rls.sql)
ALTER TABLE org_domains ENABLE ROW LEVEL SECURITY;
ALTER TABLE org_domains FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_org_domains ON org_domains
  USING (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  )
  WITH CHECK (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  );

-- Add FK from inboxes to org_domains (nullable for legacy local.neuralmail inboxes)
ALTER TABLE inboxes ADD COLUMN org_domain_id uuid REFERENCES org_domains(id);

-- Add max_domains to entitlements
ALTER TABLE plan_entitlements ADD COLUMN max_domains int NOT NULL DEFAULT 1;
ALTER TABLE org_entitlements ADD COLUMN max_domains int NOT NULL DEFAULT 1;

-- +goose Down
ALTER TABLE org_entitlements DROP COLUMN IF EXISTS max_domains;
ALTER TABLE plan_entitlements DROP COLUMN IF EXISTS max_domains;
ALTER TABLE inboxes DROP COLUMN IF EXISTS org_domain_id;
DROP POLICY IF EXISTS tenant_isolation_org_domains ON org_domains;
DROP TABLE IF EXISTS org_domains;
