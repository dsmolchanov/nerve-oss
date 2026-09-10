-- +goose Up

-- An additive, opt-in ledger. No existing outbox or entitlement is enrolled.
-- Period boundaries come from the billing authority; Core never rolls them.
CREATE TABLE org_recipient_periods (
  org_id uuid NOT NULL REFERENCES orgs(id),
  period_id uuid NOT NULL,
  meter_version integer NOT NULL DEFAULT 2 CHECK (meter_version = 2),
  starts_at timestamptz NOT NULL CHECK (isfinite(starts_at)),
  ends_at timestamptz NOT NULL CHECK (isfinite(ends_at)),
  recipient_limit bigint CHECK (recipient_limit >= 0),
  reserved bigint NOT NULL DEFAULT 0 CHECK (reserved >= 0),
  committed bigint NOT NULL DEFAULT 0 CHECK (committed >= 0),
  admission_closed boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (org_id, period_id),
  CHECK (starts_at < ends_at),
  CHECK (reserved <= 9223372036854775807 - committed),
  CHECK (recipient_limit IS NULL OR reserved <= recipient_limit - committed)
);

-- The opaque outbox ID survives queue retention. A cascade from the queue or
-- org would erase the evidence used by late unknown-outcome reconciliation.
CREATE TABLE recipient_reservations (
  org_id uuid NOT NULL,
  outbox_id uuid NOT NULL,
  period_id uuid NOT NULL,
  recipient_count bigint NOT NULL CHECK (recipient_count > 0),
  state text NOT NULL DEFAULT 'reserved' CHECK (state IN ('reserved', 'committed', 'released')),
  created_at timestamptz NOT NULL DEFAULT now(),
  resolved_at timestamptz,
  PRIMARY KEY (org_id, outbox_id),
  FOREIGN KEY (org_id, period_id) REFERENCES org_recipient_periods(org_id, period_id),
  CHECK ((state = 'reserved' AND resolved_at IS NULL) OR
         (state <> 'reserved' AND resolved_at IS NOT NULL))
);
CREATE INDEX idx_recipient_reservations_period ON recipient_reservations(org_id, period_id, state);

ALTER TABLE org_recipient_periods ENABLE ROW LEVEL SECURITY;
ALTER TABLE org_recipient_periods FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_org_recipient_periods ON org_recipient_periods
  USING (coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid)
  WITH CHECK (coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid);
ALTER TABLE recipient_reservations ENABLE ROW LEVEL SECURITY;
ALTER TABLE recipient_reservations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_recipient_reservations ON recipient_reservations
  USING (coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid)
  WITH CHECK (coalesce(current_setting('app.cloud_mode', true), 'false') <> 'true'
    OR org_id = nullif(current_setting('app.current_org_id', true), '')::uuid);

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM org_recipient_periods) OR EXISTS (SELECT 1 FROM recipient_reservations) THEN
    RAISE EXCEPTION 'cannot roll back core migration 0030: recipient ledger rows exist';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE recipient_reservations;
DROP TABLE org_recipient_periods;
