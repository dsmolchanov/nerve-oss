-- +goose Up
CREATE TABLE IF NOT EXISTS service_tokens (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  actor text NOT NULL,
  scopes text[] NOT NULL DEFAULT '{}',
  expires_at timestamptz NOT NULL,
  revoked_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_service_tokens_org ON service_tokens(org_id);
CREATE INDEX IF NOT EXISTS idx_service_tokens_active ON service_tokens(org_id, revoked_at, expires_at);

-- +goose Down
DROP INDEX IF EXISTS idx_service_tokens_active;
DROP INDEX IF EXISTS idx_service_tokens_org;
DROP TABLE IF EXISTS service_tokens;
