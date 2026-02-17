-- +goose Up

CREATE TABLE IF NOT EXISTS plan_entitlements (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  plan_code text NOT NULL UNIQUE,
  mcp_rpm int NOT NULL,
  monthly_units bigint NOT NULL,
  max_inboxes int NOT NULL,
  max_domains int NOT NULL DEFAULT 1,
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

CREATE INDEX IF NOT EXISTS idx_subscriptions_org ON subscriptions(org_id);
CREATE INDEX IF NOT EXISTS idx_subscriptions_status ON subscriptions(status);

-- +goose Down

DROP INDEX IF EXISTS idx_subscriptions_status;
DROP INDEX IF EXISTS idx_subscriptions_org;

DROP TABLE IF EXISTS webhook_events;
DROP TABLE IF EXISTS subscriptions;
DROP TABLE IF EXISTS plan_entitlements;
