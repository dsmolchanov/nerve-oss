-- +goose Up

-- Provider-managed outbound verification metadata (Resend).
-- This keeps DNS onboarding in Nerve (no Resend dashboard step) while staying optional
-- when Resend is not configured.
ALTER TABLE org_domains
  ADD COLUMN IF NOT EXISTS resend_domain_id text,
  ADD COLUMN IF NOT EXISTS resend_domain_status text,
  ADD COLUMN IF NOT EXISTS resend_dns_records jsonb;

-- +goose Down

ALTER TABLE org_domains
  DROP COLUMN IF EXISTS resend_dns_records,
  DROP COLUMN IF EXISTS resend_domain_status,
  DROP COLUMN IF EXISTS resend_domain_id;

