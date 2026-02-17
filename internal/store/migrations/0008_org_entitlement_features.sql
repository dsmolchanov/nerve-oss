-- +goose Up

ALTER TABLE org_entitlements
  ADD COLUMN IF NOT EXISTS features jsonb NOT NULL DEFAULT '{}';

-- +goose Down

ALTER TABLE org_entitlements
  DROP COLUMN IF EXISTS features;

