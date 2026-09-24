-- +goose Up

-- Core 30 introduced the opt-in ledger but no outbox row has enrolled yet.
-- The provider-start fence must also cover paid v2 rows that have no
-- autonomous policy epoch. The marker is written with the reservation in the
-- enqueue transaction and remains on the outbox row after ledger settlement.
ALTER TABLE outbox_messages
  ADD COLUMN recipient_meter_version integer
    CHECK (recipient_meter_version IS NULL OR recipient_meter_version = 2);

ALTER TABLE outbox_messages
  DROP CONSTRAINT chk_outbox_provider_fence_shape,
  ADD CONSTRAINT chk_outbox_provider_fence_shape
    CHECK (
      (provider_started_at IS NULL AND provider_operation_id IS NULL AND provider_resolved_at IS NULL)
      OR
      (
        (autonomous_policy_epoch IS NOT NULL OR recipient_meter_version IS NOT DISTINCT FROM 2)
        AND provider_started_at IS NOT NULL
        AND provider_operation_id IS NOT NULL
        AND btrim(provider_operation_id) <> ''
        AND (provider_resolved_at IS NULL OR provider_resolved_at >= provider_started_at)
      )
    );

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM outbox_messages WHERE recipient_meter_version IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot roll back core migration 0031: metered outbox rows exist';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE outbox_messages
  DROP CONSTRAINT chk_outbox_provider_fence_shape,
  ADD CONSTRAINT chk_outbox_provider_fence_shape
    CHECK (
      (provider_started_at IS NULL AND provider_operation_id IS NULL AND provider_resolved_at IS NULL)
      OR
      (
        autonomous_policy_epoch IS NOT NULL
        AND provider_started_at IS NOT NULL
        AND provider_operation_id IS NOT NULL
        AND btrim(provider_operation_id) <> ''
        AND (provider_resolved_at IS NULL OR provider_resolved_at >= provider_started_at)
      )
    );

ALTER TABLE outbox_messages DROP COLUMN recipient_meter_version;
