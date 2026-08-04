package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type OrgEvent struct {
	ID          string
	OrgID       string
	EventType   string
	RefKind     string
	RefID       string
	Payload     json.RawMessage
	FannedOutAt sql.NullTime
	CreatedAt   time.Time
}

// OrgEventJournalAvailable reports whether the additive event-journal schema
// has been applied. Reconciliation binaries can run throughout the rollout
// window, including against core schema 0019 where org_events is absent.
func (s *Store) OrgEventJournalAvailable(ctx context.Context) (bool, error) {
	var available bool
	err := s.q.QueryRowContext(ctx, `
		SELECT to_regclass('public.org_events') IS NOT NULL
	`).Scan(&available)
	return available, err
}

// InsertOrgEventAndFanOut atomically journals one stable domain event, creates
// all matching delivery rows, and stamps the journal as fanned out. Replays
// return the original event ID without duplicating deliveries.
func (s *Store) InsertOrgEventAndFanOut(
	ctx context.Context,
	orgID string,
	eventType string,
	refKind string,
	refID string,
	payload json.RawMessage,
) (eventID string, deliveries int, err error) {
	if orgID == "" || eventType == "" || refKind == "" || refID == "" {
		return "", 0, errors.New("missing org event field")
	}
	if !IsValidWebhookEventType(eventType) {
		return "", 0, fmt.Errorf("unsupported webhook event type %q", eventType)
	}
	if !json.Valid(payload) {
		return "", 0, errors.New("invalid org event payload")
	}

	err = s.withTx(ctx, func(scoped *Store) error {
		fanOutPayload := payload
		var insertedID string
		scanErr := scoped.q.QueryRowContext(ctx, `
			INSERT INTO org_events (org_id, event_type, ref_kind, ref_id, payload)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (org_id, event_type, ref_kind, ref_id) DO NOTHING
			RETURNING id::text
		`, orgID, eventType, refKind, refID, []byte(payload)).Scan(&insertedID)
		if scanErr != nil && !errors.Is(scanErr, sql.ErrNoRows) {
			return scanErr
		}

		var fannedOutAt sql.NullTime
		if insertedID != "" {
			eventID = insertedID
		} else {
			if err := scoped.q.QueryRowContext(ctx, `
				SELECT id::text, fanned_out_at, payload
				FROM org_events
				WHERE org_id = $1 AND event_type = $2 AND ref_kind = $3 AND ref_id = $4
				FOR UPDATE
			`, orgID, eventType, refKind, refID).Scan(&eventID, &fannedOutAt, &fanOutPayload); err != nil {
				return err
			}
			if fannedOutAt.Valid {
				return nil
			}
		}

		var fanOutErr error
		deliveries, fanOutErr = scoped.fanOutOrgEventDeliveries(ctx, orgID, eventID, eventType, fanOutPayload)
		if fanOutErr != nil {
			return fanOutErr
		}
		result, updateErr := scoped.q.ExecContext(ctx, `
			UPDATE org_events SET fanned_out_at = now()
			WHERE id = $1 AND fanned_out_at IS NULL
		`, eventID)
		if updateErr != nil {
			return updateErr
		}
		updated, updateErr := result.RowsAffected()
		if updateErr != nil {
			return updateErr
		}
		if updated != 1 {
			return errors.New("org event fan-out stamp was not updated")
		}
		return nil
	})
	return eventID, deliveries, err
}

func (s *Store) fanOutOrgEventDeliveries(
	ctx context.Context,
	orgID string,
	orgEventID string,
	eventType string,
	payload json.RawMessage,
) (int, error) {
	webhookIDs, err := s.matchingOrgWebhookIDs(ctx, orgID, eventType)
	if err != nil {
		return 0, err
	}
	for index, webhookID := range webhookIDs {
		if _, err := s.enqueueOrgWebhookDelivery(ctx, orgID, webhookID, orgEventID, eventType, payload); err != nil {
			return index, err
		}
	}
	return len(webhookIDs), nil
}

func (s *Store) enqueueOrgWebhookDelivery(
	ctx context.Context,
	orgID string,
	webhookID string,
	orgEventID string,
	eventType string,
	payload json.RawMessage,
) (string, error) {
	if orgID == "" || webhookID == "" || orgEventID == "" || eventType == "" {
		return "", errors.New("missing enqueue org webhook delivery field")
	}
	var id string
	err := s.q.QueryRowContext(ctx, `
		INSERT INTO org_webhook_deliveries
		    (org_id, webhook_id, org_event_id, event_type, payload)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (webhook_id, org_event_id) WHERE org_event_id IS NOT NULL
		DO UPDATE SET webhook_id = org_webhook_deliveries.webhook_id
		RETURNING id::text
	`, orgID, webhookID, orgEventID, eventType, []byte(payload)).Scan(&id)
	return id, err
}

// ListPendingOrgEvents returns journal rows whose fan-out lease is owed. The
// reconciler deliberately handles old rows only so live ingest owns the fast
// path and a just-open transaction is never mistaken for abandoned work.
func (s *Store) ListPendingOrgEvents(ctx context.Context, before time.Time, limit int) ([]OrgEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT id::text, org_id::text, event_type, ref_kind, ref_id::text,
		       payload, fanned_out_at, created_at
		FROM org_events
		WHERE fanned_out_at IS NULL AND created_at < $1
		ORDER BY created_at, id
		LIMIT $2
	`, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []OrgEvent
	for rows.Next() {
		var event OrgEvent
		if err := rows.Scan(
			&event.ID,
			&event.OrgID,
			&event.EventType,
			&event.RefKind,
			&event.RefID,
			&event.Payload,
			&event.FannedOutAt,
			&event.CreatedAt,
		); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

// ReFanOutOrgEvent repairs one explicitly pending journal row. It is
// idempotent and uses the same delivery uniqueness as the ingest path.
func (s *Store) ReFanOutOrgEvent(ctx context.Context, eventID string) (deliveries int, err error) {
	if eventID == "" {
		return 0, errors.New("missing org event id")
	}
	err = s.withTx(ctx, func(scoped *Store) error {
		var event OrgEvent
		if err := scoped.q.QueryRowContext(ctx, `
			SELECT id::text, org_id::text, event_type, ref_kind, ref_id::text,
			       payload, fanned_out_at, created_at
			FROM org_events
			WHERE id = $1
			FOR UPDATE
		`, eventID).Scan(
			&event.ID,
			&event.OrgID,
			&event.EventType,
			&event.RefKind,
			&event.RefID,
			&event.Payload,
			&event.FannedOutAt,
			&event.CreatedAt,
		); err != nil {
			return err
		}
		if event.FannedOutAt.Valid {
			return nil
		}

		var fanOutErr error
		deliveries, fanOutErr = scoped.fanOutOrgEventDeliveries(
			ctx,
			event.OrgID,
			event.ID,
			event.EventType,
			event.Payload,
		)
		if fanOutErr != nil {
			return fanOutErr
		}
		result, updateErr := scoped.q.ExecContext(ctx, `
			UPDATE org_events SET fanned_out_at = now()
			WHERE id = $1 AND fanned_out_at IS NULL
		`, event.ID)
		if updateErr != nil {
			return updateErr
		}
		updated, updateErr := result.RowsAffected()
		if updateErr != nil {
			return updateErr
		}
		if updated != 1 {
			return errors.New("org event repair stamp was not updated")
		}
		return nil
	})
	return deliveries, err
}
