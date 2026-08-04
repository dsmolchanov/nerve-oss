-- +goose Up

-- A4: Customer-facing outbound webhooks. Tenants subscribe to delivery
-- events for their own outbox messages and stop polling. Mirrors the
-- outbox worker pattern (claim/backoff/retry) intentionally so the
-- same operational tooling works for both.

-- Webhook endpoints. One row per (org, url). Secrets are 32 bytes of
-- random hex, rotated on demand via the admin API. Empty events[]
-- means "all event types" so the default subscription receives
-- everything without listing.
CREATE TABLE IF NOT EXISTS org_webhooks (
  id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id      uuid        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  url         text        NOT NULL,
  secret      text        NOT NULL,
  events      text[]      NOT NULL DEFAULT ARRAY[]::text[],
  created_at  timestamptz NOT NULL DEFAULT now(),
  disabled_at timestamptz,
  CONSTRAINT chk_org_webhook_url CHECK (url ~* '^https?://')
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_org_webhooks_org_active
  ON org_webhooks (org_id, url)
  WHERE disabled_at IS NULL;

-- RLS: same tenant isolation pattern as the rest of the schema.
ALTER TABLE org_webhooks ENABLE ROW LEVEL SECURITY;
ALTER TABLE org_webhooks FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_org_webhooks ON org_webhooks;
CREATE POLICY tenant_isolation_org_webhooks ON org_webhooks
  USING (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  )
  WITH CHECK (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  );

-- Delivery attempts. One row per (webhook, outbox_event) so the
-- unique constraint prevents double dispatches on webhook-subscription
-- replay or concurrent fan-out. Claim loop mirrors outbox_messages.
CREATE TABLE IF NOT EXISTS org_webhook_deliveries (
  id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id            uuid        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  webhook_id        uuid        NOT NULL REFERENCES org_webhooks(id) ON DELETE CASCADE,
  outbox_event_id   uuid        NOT NULL REFERENCES outbox_events(id) ON DELETE CASCADE,
  event_type        text        NOT NULL,
  payload           jsonb       NOT NULL,
  status            text        NOT NULL DEFAULT 'queued',
  attempt_count     int         NOT NULL DEFAULT 0,
  next_attempt_at   timestamptz NOT NULL DEFAULT now(),
  last_attempt_at   timestamptz,
  last_status_code  int,
  last_error        text,
  locked_at         timestamptz,
  locked_by         text,
  delivered_at      timestamptz,
  created_at        timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT chk_webhook_delivery_status
    CHECK (status IN ('queued', 'delivering', 'delivered', 'failed'))
);

-- Fast claim lookup: pending deliveries ordered by next_attempt_at.
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_claim
  ON org_webhook_deliveries (status, next_attempt_at)
  WHERE status IN ('queued', 'delivering');

-- Idempotency: one attempt row per (webhook, event). Fan-out is safe
-- to retry, replay, or run concurrently.
CREATE UNIQUE INDEX IF NOT EXISTS idx_webhook_deliveries_unique
  ON org_webhook_deliveries (webhook_id, outbox_event_id);

-- RLS for deliveries as well.
ALTER TABLE org_webhook_deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE org_webhook_deliveries FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_webhook_deliveries ON org_webhook_deliveries;
CREATE POLICY tenant_isolation_webhook_deliveries ON org_webhook_deliveries
  USING (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  )
  WITH CHECK (
    coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid
  );

-- +goose Down

DROP INDEX IF EXISTS idx_webhook_deliveries_unique;
DROP INDEX IF EXISTS idx_webhook_deliveries_claim;
DROP POLICY IF EXISTS tenant_isolation_webhook_deliveries ON org_webhook_deliveries;
DROP TABLE IF EXISTS org_webhook_deliveries;

DROP INDEX IF EXISTS idx_org_webhooks_org_active;
DROP POLICY IF EXISTS tenant_isolation_org_webhooks ON org_webhooks;
DROP TABLE IF EXISTS org_webhooks;
