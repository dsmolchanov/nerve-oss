-- +goose Up

CREATE TABLE IF NOT EXISTS tool_idempotency (
  org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  tool_name text NOT NULL,
  idempotency_key text NOT NULL,
  status text NOT NULL,
  cached_response jsonb,
  updated_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT chk_tool_idempotency_status CHECK (status IN ('started', 'succeeded', 'failed')),
  PRIMARY KEY (org_id, tool_name, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_tool_idempotency_updated ON tool_idempotency(updated_at);

-- +goose Down

DROP INDEX IF EXISTS idx_tool_idempotency_updated;
DROP TABLE IF EXISTS tool_idempotency;

