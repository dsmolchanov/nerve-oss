-- +goose Up
CREATE TABLE IF NOT EXISTS plan_entitlements (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  plan_code text NOT NULL UNIQUE,
  mcp_rpm int NOT NULL,
  monthly_units bigint NOT NULL,
  max_inboxes int NOT NULL,
  features jsonb NOT NULL DEFAULT '{}',
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS subscriptions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  provider text NOT NULL,
  external_customer_id text NOT NULL,
  external_subscription_id text NOT NULL,
  status text NOT NULL,
  current_period_start timestamptz,
  current_period_end timestamptz,
  cancel_at_period_end boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(org_id),
  UNIQUE(external_subscription_id)
);

CREATE TABLE IF NOT EXISTS org_entitlements (
  org_id uuid PRIMARY KEY REFERENCES orgs(id) ON DELETE CASCADE,
  plan_code text NOT NULL,
  subscription_status text NOT NULL,
  mcp_rpm int NOT NULL,
  monthly_units bigint NOT NULL,
  max_inboxes int NOT NULL,
  usage_period_start timestamptz NOT NULL,
  usage_period_end timestamptz NOT NULL,
  grace_until timestamptz,
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS org_usage_counters (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  meter_name text NOT NULL,
  period_start timestamptz NOT NULL,
  period_end timestamptz NOT NULL,
  used bigint NOT NULL DEFAULT 0,
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(org_id, meter_name, period_start)
);

CREATE TABLE IF NOT EXISTS usage_events (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  meter_name text NOT NULL,
  quantity int NOT NULL DEFAULT 1,
  tool_name text NOT NULL,
  replay_id text,
  audit_id uuid,
  status text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS webhook_events (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  provider text NOT NULL,
  external_event_id text NOT NULL,
  event_type text NOT NULL,
  payload_hash text,
  processed_at timestamptz NOT NULL DEFAULT now(),
  status text NOT NULL,
  error_message text,
  UNIQUE(provider, external_event_id)
);

CREATE TABLE IF NOT EXISTS cloud_api_keys (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  key_prefix text NOT NULL,
  key_hash text NOT NULL,
  label text,
  scopes text[] NOT NULL DEFAULT '{}',
  revoked_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(key_hash)
);

CREATE INDEX IF NOT EXISTS idx_subscriptions_org ON subscriptions(org_id);
CREATE INDEX IF NOT EXISTS idx_subscriptions_status ON subscriptions(status);
CREATE INDEX IF NOT EXISTS idx_usage_events_org_created ON usage_events(org_id, created_at);
CREATE UNIQUE INDEX IF NOT EXISTS idx_usage_events_replay ON usage_events(replay_id) WHERE replay_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_org_usage_meter_period ON org_usage_counters(org_id, meter_name, period_start);

WITH existing_org AS (
  SELECT id FROM orgs ORDER BY created_at ASC LIMIT 1
),
inserted_org AS (
  INSERT INTO orgs (name)
  SELECT 'default'
  WHERE NOT EXISTS (SELECT 1 FROM existing_org)
  RETURNING id
),
fallback_org AS (
  SELECT id FROM existing_org
  UNION ALL
  SELECT id FROM inserted_org
  LIMIT 1
)
UPDATE inboxes
SET org_id = (SELECT id FROM fallback_org)
WHERE org_id IS NULL;

ALTER TABLE threads
  ADD COLUMN IF NOT EXISTS org_id uuid REFERENCES orgs(id) ON DELETE CASCADE;
UPDATE threads t
SET org_id = i.org_id
FROM inboxes i
WHERE t.inbox_id = i.id
  AND t.org_id IS NULL;
ALTER TABLE threads
  ALTER COLUMN org_id SET NOT NULL;

ALTER TABLE messages
  ADD COLUMN IF NOT EXISTS org_id uuid REFERENCES orgs(id) ON DELETE CASCADE;
UPDATE messages m
SET org_id = i.org_id
FROM inboxes i
WHERE m.inbox_id = i.id
  AND m.org_id IS NULL;
ALTER TABLE messages
  ALTER COLUMN org_id SET NOT NULL;

CREATE INDEX IF NOT EXISTS idx_threads_org ON threads(org_id);
CREATE INDEX IF NOT EXISTS idx_messages_org ON messages(org_id);

-- +goose Down
DROP INDEX IF EXISTS idx_messages_org;
DROP INDEX IF EXISTS idx_threads_org;

ALTER TABLE messages DROP COLUMN IF EXISTS org_id;
ALTER TABLE threads DROP COLUMN IF EXISTS org_id;

DROP INDEX IF EXISTS idx_org_usage_meter_period;
DROP INDEX IF EXISTS idx_usage_events_replay;
DROP INDEX IF EXISTS idx_usage_events_org_created;
DROP INDEX IF EXISTS idx_subscriptions_status;
DROP INDEX IF EXISTS idx_subscriptions_org;

DROP TABLE IF EXISTS cloud_api_keys;
DROP TABLE IF EXISTS webhook_events;
DROP TABLE IF EXISTS usage_events;
DROP TABLE IF EXISTS org_usage_counters;
DROP TABLE IF EXISTS org_entitlements;
DROP TABLE IF EXISTS subscriptions;
DROP TABLE IF EXISTS plan_entitlements;
