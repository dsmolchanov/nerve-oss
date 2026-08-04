-- +goose Up
ALTER TABLE org_domains ADD COLUMN IF NOT EXISTS forward_to text;

-- +goose Down
ALTER TABLE org_domains DROP COLUMN IF EXISTS forward_to;
