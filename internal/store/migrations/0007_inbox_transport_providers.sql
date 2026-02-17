-- +goose Up

ALTER TABLE inboxes
  ADD COLUMN IF NOT EXISTS inbound_provider text NOT NULL DEFAULT 'jmap',
  ADD COLUMN IF NOT EXISTS outbound_provider text NOT NULL DEFAULT 'smtp',
  ADD COLUMN IF NOT EXISTS inbound_provider_config_ref text,
  ADD COLUMN IF NOT EXISTS outbound_provider_config_ref text;

CREATE INDEX IF NOT EXISTS idx_inboxes_inbound_provider ON inboxes(inbound_provider);
CREATE INDEX IF NOT EXISTS idx_inboxes_outbound_provider ON inboxes(outbound_provider);

-- +goose Down

DROP INDEX IF EXISTS idx_inboxes_outbound_provider;
DROP INDEX IF EXISTS idx_inboxes_inbound_provider;

ALTER TABLE inboxes
  DROP COLUMN IF EXISTS outbound_provider_config_ref,
  DROP COLUMN IF EXISTS inbound_provider_config_ref,
  DROP COLUMN IF EXISTS outbound_provider,
  DROP COLUMN IF EXISTS inbound_provider;

