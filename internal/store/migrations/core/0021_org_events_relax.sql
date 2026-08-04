-- +goose Up

ALTER TABLE org_webhook_deliveries
  ALTER COLUMN outbox_event_id DROP NOT NULL,
  ADD CONSTRAINT chk_delivery_event_source
    CHECK ((outbox_event_id IS NOT NULL) <> (org_event_id IS NOT NULL));

CREATE UNIQUE INDEX idx_webhook_deliveries_unique_org_event
  ON org_webhook_deliveries (webhook_id, org_event_id)
  WHERE org_event_id IS NOT NULL;

UPDATE org_webhooks
SET disabled_at = now()
WHERE disabled_at IS NULL
  AND lower(url) NOT LIKE 'https://%'
  AND 'email.received' = ANY(events);

-- +goose Down

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM org_webhook_deliveries WHERE org_event_id IS NOT NULL
  ) THEN
    RAISE EXCEPTION 'cannot roll back core migration 0021: org event deliveries exist';
  END IF;
END $$;
-- +goose StatementEnd

DROP INDEX idx_webhook_deliveries_unique_org_event;

ALTER TABLE org_webhook_deliveries
  DROP CONSTRAINT chk_delivery_event_source,
  ALTER COLUMN outbox_event_id SET NOT NULL;
