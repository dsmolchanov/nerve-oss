-- +goose Up
ALTER TABLE orgs
  ADD COLUMN IF NOT EXISTS mcp_endpoint text;

ALTER TABLE orgs
  ADD COLUMN IF NOT EXISTS updated_at timestamptz NOT NULL DEFAULT now();

ALTER TABLE orgs
  DROP CONSTRAINT IF EXISTS orgs_mcp_endpoint_scheme_check;

ALTER TABLE orgs
  ADD CONSTRAINT orgs_mcp_endpoint_scheme_check
  CHECK (
    mcp_endpoint IS NULL
    OR mcp_endpoint = ''
    OR mcp_endpoint ~* '^https?://'
  );

-- +goose Down
ALTER TABLE orgs
  DROP CONSTRAINT IF EXISTS orgs_mcp_endpoint_scheme_check;

ALTER TABLE orgs
  DROP COLUMN IF EXISTS mcp_endpoint;

ALTER TABLE orgs
  DROP COLUMN IF EXISTS updated_at;
