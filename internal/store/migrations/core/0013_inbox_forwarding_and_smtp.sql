-- +goose Up

-- Inbox forwarding: nullable forward-to email address
ALTER TABLE inboxes ADD COLUMN IF NOT EXISTS forward_to text;

-- Custom SMTP configuration per inbox
CREATE TABLE IF NOT EXISTS inbox_smtp_configs (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  inbox_id uuid NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  host text NOT NULL,
  port int NOT NULL DEFAULT 587,
  username text NOT NULL,
  password_enc text NOT NULL,
  require_starttls boolean NOT NULL DEFAULT true,
  from_name text,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(inbox_id)
);

-- +goose Down
DROP TABLE IF EXISTS inbox_smtp_configs;
ALTER TABLE inboxes DROP COLUMN IF EXISTS forward_to;
