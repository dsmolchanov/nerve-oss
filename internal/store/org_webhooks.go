package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// OrgWebhook is a customer subscription to outbound delivery events.
type OrgWebhook struct {
	ID         string
	OrgID      string
	URL        string
	Secret     string
	Events     []string // empty means "all event types"
	CreatedAt  time.Time
	DisabledAt sql.NullTime
}

// WebhookDelivery is one dispatch attempt targeting a specific
// (webhook, outbox_event) pair. Mirrors the outbox_messages retry
// shape so operators can reason about both queues the same way.
type WebhookDelivery struct {
	ID             string
	OrgID          string
	WebhookID      string
	OutboxEventID  string
	EventType      string
	Payload        json.RawMessage
	Status         string
	AttemptCount   int
	NextAttemptAt  time.Time
	LastAttemptAt  sql.NullTime
	LastStatusCode sql.NullInt32
	LastError      sql.NullString
	LockedAt       sql.NullTime
	LockedBy       sql.NullString
	DeliveredAt    sql.NullTime
	CreatedAt      time.Time
}

// MaxWebhookRetries caps delivery attempts before a webhook is marked
// permanently failed. Matches the outbox convention.
const MaxWebhookRetries = 8

// generateWebhookSecret returns 32 bytes of random hex for signing.
func generateWebhookSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// CreateOrgWebhook inserts a subscription, generating a fresh signing
// secret. Returns the persisted row including the secret so the
// caller can show it once in the admin UI.
func (s *Store) CreateOrgWebhook(ctx context.Context, orgID, url string, events []string) (OrgWebhook, error) {
	if orgID == "" || url == "" {
		return OrgWebhook{}, errors.New("missing org_id or url")
	}
	secret, err := generateWebhookSecret()
	if err != nil {
		return OrgWebhook{}, fmt.Errorf("generate secret: %w", err)
	}
	if events == nil {
		events = []string{}
	}
	var w OrgWebhook
	var eventsText string
	row := s.q.QueryRowContext(ctx, `
		INSERT INTO org_webhooks (org_id, url, secret, events)
		VALUES ($1, $2, $3, $4)
		RETURNING id::text, org_id::text, url, secret, events::text, created_at, disabled_at
	`, orgID, url, secret, events)
	if err := row.Scan(&w.ID, &w.OrgID, &w.URL, &w.Secret, &eventsText, &w.CreatedAt, &w.DisabledAt); err != nil {
		return OrgWebhook{}, err
	}
	w.Events = parseTextArray(eventsText)
	return w, nil
}

// ListOrgWebhooks returns all webhooks for an org, newest first.
// Disabled webhooks are included so operators can see them.
func (s *Store) ListOrgWebhooks(ctx context.Context, orgID string) ([]OrgWebhook, error) {
	if orgID == "" {
		return nil, errors.New("missing org_id")
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT id::text, org_id::text, url, secret, events::text, created_at, disabled_at
		FROM org_webhooks
		WHERE org_id = $1
		ORDER BY created_at DESC
	`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrgWebhook
	for rows.Next() {
		var w OrgWebhook
		var eventsText string
		if err := rows.Scan(&w.ID, &w.OrgID, &w.URL, &w.Secret, &eventsText, &w.CreatedAt, &w.DisabledAt); err != nil {
			return nil, err
		}
		w.Events = parseTextArray(eventsText)
		out = append(out, w)
	}
	return out, rows.Err()
}

// GetOrgWebhook returns a single webhook by id, scoped to the org.
func (s *Store) GetOrgWebhook(ctx context.Context, orgID, id string) (OrgWebhook, error) {
	if orgID == "" || id == "" {
		return OrgWebhook{}, errors.New("missing org_id or id")
	}
	var w OrgWebhook
	var eventsText string
	row := s.q.QueryRowContext(ctx, `
		SELECT id::text, org_id::text, url, secret, events::text, created_at, disabled_at
		FROM org_webhooks
		WHERE org_id = $1 AND id = $2
	`, orgID, id)
	if err := row.Scan(&w.ID, &w.OrgID, &w.URL, &w.Secret, &eventsText, &w.CreatedAt, &w.DisabledAt); err != nil {
		return OrgWebhook{}, err
	}
	w.Events = parseTextArray(eventsText)
	return w, nil
}

// DeleteOrgWebhook removes a subscription entirely. Pending deliveries
// cascade via FK.
func (s *Store) DeleteOrgWebhook(ctx context.Context, orgID, id string) (bool, error) {
	if orgID == "" || id == "" {
		return false, errors.New("missing org_id or id")
	}
	result, err := s.q.ExecContext(ctx, `
		DELETE FROM org_webhooks WHERE org_id = $1 AND id = $2
	`, orgID, id)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

// RotateOrgWebhookSecret generates and installs a new signing secret.
// Used when a secret may have leaked or on a scheduled rotation.
func (s *Store) RotateOrgWebhookSecret(ctx context.Context, orgID, id string) (string, error) {
	if orgID == "" || id == "" {
		return "", errors.New("missing org_id or id")
	}
	secret, err := generateWebhookSecret()
	if err != nil {
		return "", fmt.Errorf("generate secret: %w", err)
	}
	result, err := s.q.ExecContext(ctx, `
		UPDATE org_webhooks SET secret = $3
		WHERE org_id = $1 AND id = $2
	`, orgID, id, secret)
	if err != nil {
		return "", err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", sql.ErrNoRows
	}
	return secret, nil
}

// EnqueueWebhookDelivery inserts a delivery job for (webhook, event).
// The (webhook_id, outbox_event_id) unique constraint makes this
// idempotent — concurrent fan-out is safe. Returns the row id on
// insert OR on conflict with an existing row.
func (s *Store) EnqueueWebhookDelivery(ctx context.Context, orgID, webhookID, outboxEventID, eventType string, payload json.RawMessage) (string, error) {
	if orgID == "" || webhookID == "" || outboxEventID == "" || eventType == "" {
		return "", errors.New("missing enqueue webhook delivery field")
	}
	var id string
	row := s.q.QueryRowContext(ctx, `
		INSERT INTO org_webhook_deliveries
		    (org_id, webhook_id, outbox_event_id, event_type, payload)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (webhook_id, outbox_event_id)
		DO UPDATE SET webhook_id = org_webhook_deliveries.webhook_id
		RETURNING id::text
	`, orgID, webhookID, outboxEventID, eventType, []byte(payload))
	if err := row.Scan(&id); err != nil {
		return "", err
	}
	return id, nil
}

// FanOutWebhookDeliveries enqueues a delivery job for each active
// webhook in the org whose event filter matches the given eventType.
// Returns the number of deliveries created (or already present).
func (s *Store) FanOutWebhookDeliveries(ctx context.Context, orgID, outboxEventID, eventType string, payload json.RawMessage) (int, error) {
	if orgID == "" || outboxEventID == "" || eventType == "" {
		return 0, nil
	}
	// Pull only active subscriptions that match the event filter.
	// An empty events array means "all events", so we include rows
	// where events = {} OR $2 = ANY(events).
	rows, err := s.q.QueryContext(ctx, `
		SELECT id::text FROM org_webhooks
		WHERE org_id = $1
		  AND disabled_at IS NULL
		  AND (cardinality(events) = 0 OR $2 = ANY(events))
	`, orgID, eventType)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var webhookIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		webhookIDs = append(webhookIDs, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	created := 0
	for _, wid := range webhookIDs {
		if _, err := s.EnqueueWebhookDelivery(ctx, orgID, wid, outboxEventID, eventType, payload); err != nil {
			return created, err
		}
		created++
	}
	return created, nil
}

// ClaimWebhookDeliveries atomically claims up to limit deliveries for
// the given worker. Rows in state 'queued' with next_attempt_at <= now
// are claimed, as are 'delivering' rows whose locks are stale. Mirrors
// ClaimOutboxMessages.
func (s *Store) ClaimWebhookDeliveries(ctx context.Context, limit int, workerID string, now time.Time, staleLockAfter time.Duration) ([]WebhookDelivery, error) {
	if limit <= 0 {
		limit = 10
	}
	if workerID == "" {
		workerID = "webhook-worker"
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if staleLockAfter <= 0 {
		staleLockAfter = 5 * time.Minute
	}
	staleCutoff := now.Add(-staleLockAfter)

	rows, err := s.q.QueryContext(ctx, `
		WITH picked AS (
			SELECT d.id
			FROM org_webhook_deliveries d
			WHERE (
				(d.status = 'queued' AND d.next_attempt_at <= $1)
				OR
				(d.status = 'delivering' AND d.locked_at <= $4)
			)
			ORDER BY d.next_attempt_at ASC
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		UPDATE org_webhook_deliveries d
		SET status = 'delivering',
		    locked_at = $1,
		    locked_by = $3,
		    attempt_count = d.attempt_count + 1,
		    last_attempt_at = $1
		FROM picked
		WHERE d.id = picked.id
		RETURNING d.id::text, d.org_id::text, d.webhook_id::text, d.outbox_event_id::text,
		          d.event_type, d.payload, d.status, d.attempt_count,
		          d.next_attempt_at, d.last_attempt_at, d.last_status_code, d.last_error,
		          d.locked_at, d.locked_by, d.delivered_at, d.created_at
	`, now, limit, workerID, staleCutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []WebhookDelivery
	for rows.Next() {
		var d WebhookDelivery
		if err := rows.Scan(
			&d.ID, &d.OrgID, &d.WebhookID, &d.OutboxEventID,
			&d.EventType, &d.Payload, &d.Status, &d.AttemptCount,
			&d.NextAttemptAt, &d.LastAttemptAt, &d.LastStatusCode, &d.LastError,
			&d.LockedAt, &d.LockedBy, &d.DeliveredAt, &d.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// LookupWebhookSubscription returns the URL and secret for a webhook id.
// Returns sql.ErrNoRows if the webhook has been deleted between claim
// and dispatch; callers should treat that as "drop the delivery".
func (s *Store) LookupWebhookSubscription(ctx context.Context, webhookID string) (string, string, error) {
	var url, secret string
	row := s.q.QueryRowContext(ctx, `
		SELECT url, secret FROM org_webhooks
		WHERE id = $1 AND disabled_at IS NULL
	`, webhookID)
	if err := row.Scan(&url, &secret); err != nil {
		return "", "", err
	}
	return url, secret, nil
}

// MarkWebhookDeliveryDelivered marks a delivery as successfully delivered.
func (s *Store) MarkWebhookDeliveryDelivered(ctx context.Context, id string, statusCode int) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE org_webhook_deliveries
		SET status = 'delivered',
		    last_status_code = $2,
		    delivered_at = now(),
		    last_error = null,
		    locked_at = null,
		    locked_by = null
		WHERE id = $1
	`, id, statusCode)
	return err
}

// RequeueWebhookDelivery schedules the next retry attempt.
func (s *Store) RequeueWebhookDelivery(ctx context.Context, id string, nextAttemptAt time.Time, statusCode int, lastError string) error {
	var statusCodePtr any
	if statusCode > 0 {
		statusCodePtr = statusCode
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE org_webhook_deliveries
		SET status = 'queued',
		    next_attempt_at = $2,
		    last_status_code = $3,
		    last_error = nullif($4, ''),
		    locked_at = null,
		    locked_by = null
		WHERE id = $1
	`, id, nextAttemptAt, statusCodePtr, lastError)
	return err
}

// MarkWebhookDeliveryFailed marks a delivery as permanently failed.
func (s *Store) MarkWebhookDeliveryFailed(ctx context.Context, id string, statusCode int, lastError string) error {
	var statusCodePtr any
	if statusCode > 0 {
		statusCodePtr = statusCode
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE org_webhook_deliveries
		SET status = 'failed',
		    last_status_code = $2,
		    last_error = nullif($3, ''),
		    locked_at = null,
		    locked_by = null
		WHERE id = $1
	`, id, statusCodePtr, lastError)
	return err
}

// parseTextArray parses a Postgres text[] cast to text. Handles the
// "{a,b,c}" representation that pgx returns for `::text` casts of
// text[] columns, including quoted values and escapes.
func parseTextArray(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" || raw == "null" || raw == "NULL" {
		return []string{}
	}
	if !strings.HasPrefix(raw, "{") || !strings.HasSuffix(raw, "}") {
		return []string{}
	}
	inner := raw[1 : len(raw)-1]
	if inner == "" {
		return []string{}
	}
	var out []string
	var buf strings.Builder
	inQuote := false
	escaped := false
	for i := 0; i < len(inner); i++ {
		ch := inner[i]
		if escaped {
			buf.WriteByte(ch)
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if ch == '"' {
			inQuote = !inQuote
			continue
		}
		if ch == ',' && !inQuote {
			out = append(out, buf.String())
			buf.Reset()
			continue
		}
		buf.WriteByte(ch)
	}
	if buf.Len() > 0 {
		out = append(out, buf.String())
	}
	return out
}
