-- +goose Up

-- The first accepted compose survives tier and lifecycle transitions. Existing
-- orgs are initialized lazily from the immutable usage journal on their first
-- successful policy-v2 compose; the index bounds that one-time lookup.
ALTER TABLE org_outbound_policy_state
  ADD COLUMN first_compose_accepted_at timestamptz
    CHECK (first_compose_accepted_at IS NULL OR isfinite(first_compose_accepted_at));

CREATE INDEX idx_usage_events_first_compose
  ON usage_events (org_id, created_at)
  WHERE meter_name = 'autonomous_outbound_send_day' AND status = 'success';

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM org_outbound_policy_state WHERE first_compose_accepted_at IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot roll back core migration 0032: warmup origin evidence exists';
  END IF;
END $$;
-- +goose StatementEnd

DROP INDEX idx_usage_events_first_compose;
ALTER TABLE org_outbound_policy_state DROP COLUMN first_compose_accepted_at;
