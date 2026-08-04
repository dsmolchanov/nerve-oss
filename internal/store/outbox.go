package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// MaxOutboxRetries is the maximum number of delivery attempts before giving up.
const MaxOutboxRetries = 5

// ErrDomainNotVerified is returned by EnqueueOutboxMessage when the
// `From` address's domain exists in org_domains but is not in a
// sendable status (i.e. not `active` or `verified_dns`). Callers
// should surface this as a 4xx to the end user rather than enqueueing
// a message that would fail every retry.
var ErrDomainNotVerified = errors.New("domain not verified")

type OutboxMessage struct {
	ID                string
	OrgID             string
	InboxID           string
	Provider          string
	ProviderMessageID sql.NullString
	IdempotencyKey    string
	To                string
	From              string
	Subject           string
	TextBody          string
	HTMLBody          string
	ContentHash       string

	// Threading headers for reply-chain continuity (RFC 5322).
	// Set when replying to an inbound message so the recipient's
	// email client threads the reply correctly.
	InReplyToMessageID string // Internet-Message-ID of the message being replied to
	References         string // Space-separated list of ancestor message IDs

	Status           string
	DeliveryStatus   string
	DeliveryStatusAt sql.NullTime
	AttemptCount     int
	NextAttemptAt    time.Time
	LastAttemptAt    sql.NullTime
	LastError        sql.NullString
	LockedAt         sql.NullTime
	LockedBy         sql.NullString
	CreatedAt        time.Time
}

// OutboxEvent represents a delivery event in the append-only timeline.
//
// OutboxMessageID is the durable foreign key into outbox_messages and is
// the preferred linkage. ProviderMessageID is optional and only set after
// the provider has accepted the message and returned an ID; pre-provider
// events (suppression at enqueue, pre-flight rejection) leave it empty.
//
// At least one of OutboxMessageID or ProviderMessageID must be set.
// InsertOutboxEvent prefers the direct path when OutboxMessageID is
// non-empty and falls back to a join via provider_message_id otherwise
// (the legacy webhook callback path).
type OutboxEvent struct {
	OrgID             string
	OutboxMessageID   string
	ProviderMessageID string
	EventType         string
	RawPayload        json.RawMessage
	Reason            string
}

// Suppression is a per-org block on a recipient address.
type Suppression struct {
	OrgID     string
	Email     string
	Reason    string
	Source    string // "bounce", "complaint", "manual"
	CreatedAt time.Time
}

// OutboxEventRecord is a persisted row from outbox_events. Distinct
// from OutboxEvent (the insert-time input shape) because reads return
// the row id, creation timestamp, and stored provider_message_id.
type OutboxEventRecord struct {
	ID                string
	OrgID             string
	OutboxMessageID   string
	ProviderMessageID string
	EventType         string
	RawPayload        json.RawMessage
	Reason            string
	CreatedAt         time.Time
}

func contentHash(to, subject, textBody, htmlBody string) string {
	h := sha256.New()
	h.Write([]byte(to))
	h.Write([]byte{0})
	h.Write([]byte(subject))
	h.Write([]byte{0})
	h.Write([]byte(textBody))
	h.Write([]byte{0})
	h.Write([]byte(htmlBody))
	return fmt.Sprintf("%x", h.Sum(nil))
}

func (s *Store) EnqueueOutboxMessage(ctx context.Context, msg OutboxMessage) (string, error) {
	if msg.OrgID == "" || msg.InboxID == "" {
		return "", errors.New("missing org_id or inbox_id")
	}
	if msg.Provider == "" {
		return "", errors.New("missing provider")
	}
	if msg.IdempotencyKey == "" {
		return "", errors.New("missing idempotency_key")
	}
	if msg.To == "" || msg.From == "" {
		return "", errors.New("missing to/from")
	}
	if msg.Subject == "" {
		return "", errors.New("missing subject")
	}
	id := msg.ID
	if id == "" {
		id = uuid.NewString()
	}

	// B4: pre-flight domain verification. If the `From` address uses a
	// domain the org has claimed in org_domains but hasn't verified yet,
	// reject synchronously with a typed error rather than enqueueing a
	// row that would fail every retry. Domains NOT in org_domains for
	// this org are allowed (preserves dev/legacy local paths); we only
	// enforce on explicitly-claimed-but-unverified domains.
	if err := s.checkSendableDomain(ctx, msg.OrgID, msg.From); err != nil {
		return "", err
	}

	// B2: short-circuit suppressed recipients. The row is still inserted
	// (preserving the idempotency contract — callers always get a handle
	// back) but lands directly in failed/suppressed state without ever
	// hitting the provider.
	suppressed, suppressReason, err := s.IsSuppressed(ctx, msg.OrgID, msg.To)
	if err != nil {
		return "", fmt.Errorf("check suppression: %w", err)
	}
	if suppressed {
		return s.enqueueSuppressedMessage(ctx, id, msg, suppressReason)
	}

	hash := contentHash(msg.To, msg.Subject, msg.TextBody, msg.HTMLBody)

	row := s.q.QueryRowContext(ctx, `
		INSERT INTO outbox_messages (id, org_id, inbox_id, provider, idempotency_key, "to", "from", subject, text_body, html_body, content_hash, in_reply_to_message_id, "references")
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, nullif($9, ''), nullif($10, ''), $11, nullif($12, ''), nullif($13, ''))
		ON CONFLICT DO NOTHING
		RETURNING id
	`, id, msg.OrgID, msg.InboxID, msg.Provider, msg.IdempotencyKey, msg.To, msg.From, msg.Subject, msg.TextBody, msg.HTMLBody, hash,
		msg.InReplyToMessageID, msg.References)
	var outID string
	if err := row.Scan(&outID); err == nil {
		return outID, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}

	// ON CONFLICT DO NOTHING can wait for a concurrent insert that was not
	// visible in the INSERT statement's snapshot. Resolve the winner in a
	// separate statement so READ COMMITTED takes a fresh snapshot after that
	// transaction commits. This covers both idempotency-key and partial
	// content-hash conflicts without surfacing a uniqueness error to callers.
	row = s.q.QueryRowContext(ctx, `
		SELECT id
		FROM outbox_messages
		WHERE org_id = $1
		  AND (
			idempotency_key = $2
			OR (inbox_id = $3 AND content_hash = $4 AND status IN ('queued', 'sending'))
		  )
		ORDER BY
		  CASE WHEN idempotency_key = $2 THEN 0 ELSE 1 END,
		  CASE WHEN status IN ('queued', 'sending') THEN 0 ELSE 1 END,
		  created_at DESC,
		  id DESC
		LIMIT 1
	`, msg.OrgID, msg.IdempotencyKey, msg.InboxID, hash)
	if err := row.Scan(&outID); err != nil {
		return "", err
	}
	return outID, nil
}

func (s *Store) ClaimOutboxMessages(ctx context.Context, limit int, workerID string, now time.Time, staleLockAfter time.Duration) ([]OutboxMessage, error) {
	if limit <= 0 {
		limit = 10
	}
	if workerID == "" {
		workerID = "worker"
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
			SELECT id
			FROM outbox_messages
			WHERE (
				(status = 'queued' AND next_attempt_at <= $1)
				OR
				(status = 'sending' AND locked_at <= $4)
			)
			ORDER BY next_attempt_at ASC
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		UPDATE outbox_messages o
		SET status = 'sending',
		    locked_at = $1,
		    locked_by = $3,
		    attempt_count = o.attempt_count + 1,
		    last_attempt_at = $1
		FROM picked
		WHERE o.id = picked.id
		RETURNING o.id, o.org_id::text, o.inbox_id::text, o.provider, o.provider_message_id, o.idempotency_key,
		          o."to", o."from", o.subject, coalesce(o.text_body, ''), coalesce(o.html_body, ''),
		          o.status, o.attempt_count, o.next_attempt_at, o.last_attempt_at, o.last_error, o.locked_at, o.locked_by,
		          coalesce(o.in_reply_to_message_id, ''), coalesce(o."references", '')
	`, now, limit, workerID, staleCutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []OutboxMessage
	for rows.Next() {
		var msg OutboxMessage
		if err := rows.Scan(
			&msg.ID,
			&msg.OrgID,
			&msg.InboxID,
			&msg.Provider,
			&msg.ProviderMessageID,
			&msg.IdempotencyKey,
			&msg.To,
			&msg.From,
			&msg.Subject,
			&msg.TextBody,
			&msg.HTMLBody,
			&msg.Status,
			&msg.AttemptCount,
			&msg.NextAttemptAt,
			&msg.LastAttemptAt,
			&msg.LastError,
			&msg.LockedAt,
			&msg.LockedBy,
			&msg.InReplyToMessageID,
			&msg.References,
		); err != nil {
			return nil, err
		}
		out = append(out, msg)
	}
	return out, rows.Err()
}

// CountOutboxByState returns the number of outbox rows currently in the
// given state. Used by the queue-depth metric exporter; cheap because
// state is indexed.
func (s *Store) CountOutboxByState(ctx context.Context, state string) (int64, error) {
	var count int64
	row := s.q.QueryRowContext(ctx, `
		SELECT count(*) FROM outbox_messages WHERE status = $1
	`, state)
	if err := row.Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) MarkOutboxMessageSent(ctx context.Context, id string, providerMessageID string) error {
	if id == "" {
		return errors.New("missing id")
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE outbox_messages
		SET status = 'sent',
		    provider_message_id = nullif($2, ''),
		    last_error = null,
		    locked_at = null,
		    locked_by = null
		WHERE id = $1
	`, id, providerMessageID)
	return err
}

// MigrateOutboxProviderToResend switches unsent outbox messages from smtp to resend
// and resets them for immediate retry. Called at startup when Resend is the configured provider.
func (s *Store) MigrateOutboxProviderToResend(ctx context.Context) (int64, error) {
	result, err := s.q.ExecContext(ctx, `
		UPDATE outbox_messages
		SET provider = 'resend',
		    status = 'queued',
		    next_attempt_at = now(),
		    locked_at = null,
		    locked_by = null
		WHERE provider = 'smtp'
		  AND status IN ('queued', 'sending')
	`)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// MarkOutboxMessageFailed permanently marks a message as failed after exhausting retries.
func (s *Store) MarkOutboxMessageFailed(ctx context.Context, id string, lastError string) error {
	if id == "" {
		return errors.New("missing id")
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE outbox_messages
		SET status = 'failed',
		    last_error = nullif($2, ''),
		    locked_at = null,
		    locked_by = null
		WHERE id = $1
	`, id, lastError)
	return err
}

func (s *Store) RequeueOutboxMessage(ctx context.Context, id string, nextAttemptAt time.Time, lastError string) error {
	if id == "" {
		return errors.New("missing id")
	}
	if nextAttemptAt.IsZero() {
		nextAttemptAt = time.Now().UTC().Add(10 * time.Second)
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE outbox_messages
		SET status = 'queued',
		    next_attempt_at = $2,
		    last_error = nullif($3, ''),
		    locked_at = null,
		    locked_by = null
		WHERE id = $1
	`, id, nextAttemptAt, lastError)
	return err
}

// UpdateOutboxDeliveryStatus updates the current delivery status with monotonic enforcement.
// Delivery events can arrive out of order (e.g., "delivery_delayed" then "delivered").
// The delivery_status_severity() SQL function ensures status never regresses.
func (s *Store) UpdateOutboxDeliveryStatus(ctx context.Context, providerMessageID, status string) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE outbox_messages
		SET delivery_status = $1,
		    delivery_status_at = now()
		WHERE provider_message_id = $2
		  AND delivery_status_severity($1) > delivery_status_severity(delivery_status)
	`, status, providerMessageID)
	return err
}

// InsertOutboxEvent appends a delivery event to the append-only timeline.
// The outbox_events table is the full audit trail; delivery_status on
// outbox_messages is the derived "current state" for quick UI rendering.
//
// When OutboxMessageID is set, inserts directly using that as the link.
// Otherwise falls back to a join via provider_message_id (legacy webhook
// callback path that only knows the provider's ID).
func (s *Store) InsertOutboxEvent(ctx context.Context, evt OutboxEvent) error {
	_, err := s.InsertOutboxEventReturningID(ctx, evt)
	return err
}

// InsertOutboxEventReturningID is like InsertOutboxEvent but returns
// the persisted event id. Used by the webhook fan-out path so the
// dispatcher has a stable idempotency key per (webhook, event) pair.
// Returns empty string when the legacy join path finds no matching
// outbox_message row (e.g. event for a message from another org).
func (s *Store) InsertOutboxEventReturningID(ctx context.Context, evt OutboxEvent) (string, error) {
	if evt.OutboxMessageID == "" && evt.ProviderMessageID == "" {
		return "", errors.New("InsertOutboxEvent: must provide OutboxMessageID or ProviderMessageID")
	}
	if evt.OutboxMessageID != "" {
		var id string
		row := s.q.QueryRowContext(ctx, `
			INSERT INTO outbox_events (org_id, outbox_message_id, provider_message_id, event_type, raw_payload, reason)
			VALUES ($1, $2, nullif($3, ''), $4, $5, nullif($6, ''))
			RETURNING id::text
		`, evt.OrgID, evt.OutboxMessageID, evt.ProviderMessageID, evt.EventType, evt.RawPayload, evt.Reason)
		if err := row.Scan(&id); err != nil {
			return "", err
		}
		return id, nil
	}
	// Legacy join path: webhook callbacks only have the provider's ID.
	// QueryRow returns sql.ErrNoRows if the INSERT...SELECT inserted
	// zero rows (unknown provider_message_id); upstream callers treat
	// that as a benign "we don't have this message" signal.
	var id string
	row := s.q.QueryRowContext(ctx, `
		INSERT INTO outbox_events (org_id, outbox_message_id, provider_message_id, event_type, raw_payload, reason)
		SELECT $1, om.id, $2, $3, $4, $5
		FROM outbox_messages om WHERE om.provider_message_id = $2
		RETURNING id::text
	`, evt.OrgID, evt.ProviderMessageID, evt.EventType, evt.RawPayload, evt.Reason)
	if err := row.Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return id, nil
}

// IsSuppressed reports whether (orgID, email) is on the per-org suppression
// list. Email matching is case-insensitive. Returns the reason text when
// suppressed (e.g. "hard_bounce", "complaint", "manual:abuse").
func (s *Store) IsSuppressed(ctx context.Context, orgID, email string) (bool, string, error) {
	if orgID == "" || email == "" {
		return false, "", nil
	}
	var reason string
	row := s.q.QueryRowContext(ctx, `
		SELECT reason FROM suppressions
		WHERE org_id = $1 AND email_lower = lower($2)
		LIMIT 1
	`, orgID, email)
	if err := row.Scan(&reason); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, "", nil
		}
		return false, "", err
	}
	return true, reason, nil
}

// AddSuppression upserts a row into the suppression list. Used by webhook
// ingest (bounce/complaint) and the admin CRUD endpoint. Idempotent.
func (s *Store) AddSuppression(ctx context.Context, orgID, email, reason, source string) error {
	if orgID == "" || email == "" {
		return errors.New("missing org_id or email")
	}
	if source == "" {
		source = "manual"
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO suppressions (org_id, email_lower, reason, source)
		VALUES ($1, lower($2), $3, $4)
		ON CONFLICT (org_id, email_lower)
		DO UPDATE SET reason = EXCLUDED.reason, source = EXCLUDED.source
	`, orgID, email, reason, source)
	return err
}

// RemoveSuppression deletes a row from the suppression list, allowing
// future sends to that recipient. Returns true when a row was removed.
func (s *Store) RemoveSuppression(ctx context.Context, orgID, email string) (bool, error) {
	if orgID == "" || email == "" {
		return false, errors.New("missing org_id or email")
	}
	result, err := s.q.ExecContext(ctx, `
		DELETE FROM suppressions
		WHERE org_id = $1 AND email_lower = lower($2)
	`, orgID, email)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ListSuppressions returns all suppression entries for an org, newest first.
func (s *Store) ListSuppressions(ctx context.Context, orgID string, limit int) ([]Suppression, error) {
	if orgID == "" {
		return nil, errors.New("missing org_id")
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT org_id::text, email_lower, reason, source, created_at
		FROM suppressions
		WHERE org_id = $1
		ORDER BY created_at DESC
		LIMIT $2
	`, orgID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Suppression
	for rows.Next() {
		var s Suppression
		if err := rows.Scan(&s.OrgID, &s.Email, &s.Reason, &s.Source, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// checkSendableDomain verifies that the `From` address's domain is in a
// sendable state for the org. Returns ErrDomainNotVerified when the
// domain is explicitly claimed in org_domains but not yet active or
// verified_dns. Returns nil when the domain is sendable, not claimed at
// all (preserves legacy/dev paths), or when inputs are incomplete.
func (s *Store) checkSendableDomain(ctx context.Context, orgID, from string) error {
	if orgID == "" || from == "" {
		return nil
	}
	domain := extractDomainFromAddress(from)
	if domain == "" {
		return nil
	}
	// Internal dev/local paths aren't registered in org_domains. Skip
	// the check entirely rather than breaking local development.
	switch domain {
	case "local.neuralmail", "localhost":
		return nil
	}

	var status string
	row := s.q.QueryRowContext(ctx, `
		SELECT status FROM org_domains
		WHERE org_id = $1 AND lower(domain) = lower($2)
		ORDER BY
			CASE status
				WHEN 'active' THEN 0
				WHEN 'verified_dns' THEN 1
				WHEN 'provisioning' THEN 2
				WHEN 'pending' THEN 3
				WHEN 'failed' THEN 4
				ELSE 5
			END
		LIMIT 1
	`, orgID, domain)
	if err := row.Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Not claimed by this org — allow (legacy/dev path).
			return nil
		}
		return fmt.Errorf("check domain: %w", err)
	}
	switch status {
	case "active", "verified_dns":
		return nil
	default:
		return fmt.Errorf("%w: domain=%s status=%s", ErrDomainNotVerified, domain, status)
	}
}

// extractDomainFromAddress pulls the domain out of an RFC-5322 mailbox
// address. Handles both "user@host" and "Name <user@host>" forms.
// Returns "" when no domain can be found.
func extractDomainFromAddress(addr string) string {
	raw := strings.TrimSpace(addr)
	if raw == "" {
		return ""
	}
	if i := strings.LastIndex(raw, "<"); i >= 0 {
		if j := strings.Index(raw[i:], ">"); j > 0 {
			raw = raw[i+1 : i+j]
		}
	}
	at := strings.LastIndex(raw, "@")
	if at < 0 || at == len(raw)-1 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(raw[at+1:]))
}

// GetOutboxMessageByIDForOrg returns the outbox row for (orgID, id).
// Returns sql.ErrNoRows when not found or when the row belongs to a
// different org. Used by the DLQ admin endpoints and the B5 status
// lookup endpoint.
func (s *Store) GetOutboxMessageByIDForOrg(ctx context.Context, orgID, id string) (OutboxMessage, error) {
	var m OutboxMessage
	row := s.q.QueryRowContext(ctx, `
		SELECT id, org_id::text, inbox_id::text, provider, provider_message_id, idempotency_key,
		       "to", "from", subject, coalesce(text_body, ''), coalesce(html_body, ''),
		       status, delivery_status, delivery_status_at,
		       attempt_count, next_attempt_at, last_attempt_at, last_error,
		       locked_at, locked_by, created_at
		FROM outbox_messages
		WHERE org_id = $1 AND id = $2
	`, orgID, id)
	if err := row.Scan(
		&m.ID, &m.OrgID, &m.InboxID, &m.Provider, &m.ProviderMessageID, &m.IdempotencyKey,
		&m.To, &m.From, &m.Subject, &m.TextBody, &m.HTMLBody,
		&m.Status, &m.DeliveryStatus, &m.DeliveryStatusAt,
		&m.AttemptCount, &m.NextAttemptAt, &m.LastAttemptAt, &m.LastError,
		&m.LockedAt, &m.LockedBy, &m.CreatedAt,
	); err != nil {
		return OutboxMessage{}, err
	}
	return m, nil
}

// ListFailedOutboxForOrg returns up to limit permanently-failed outbox
// messages for an org, newest first. Used by the B3 DLQ admin list
// endpoint.
func (s *Store) ListFailedOutboxForOrg(ctx context.Context, orgID string, limit int) ([]OutboxMessage, error) {
	if orgID == "" {
		return nil, errors.New("missing org_id")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, org_id::text, inbox_id::text, provider, provider_message_id, idempotency_key,
		       "to", "from", subject, coalesce(text_body, ''), coalesce(html_body, ''),
		       status, delivery_status, delivery_status_at,
		       attempt_count, next_attempt_at, last_attempt_at, last_error,
		       locked_at, locked_by, created_at
		FROM outbox_messages
		WHERE org_id = $1 AND status = 'failed'
		ORDER BY created_at DESC, id DESC
		LIMIT $2
	`, orgID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxMessage
	for rows.Next() {
		var m OutboxMessage
		if err := rows.Scan(
			&m.ID, &m.OrgID, &m.InboxID, &m.Provider, &m.ProviderMessageID, &m.IdempotencyKey,
			&m.To, &m.From, &m.Subject, &m.TextBody, &m.HTMLBody,
			&m.Status, &m.DeliveryStatus, &m.DeliveryStatusAt,
			&m.AttemptCount, &m.NextAttemptAt, &m.LastAttemptAt, &m.LastError,
			&m.LockedAt, &m.LockedBy, &m.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListOutboxEventsForMessage returns the append-only event timeline for
// a specific outbox row, oldest first. Used by B3 (DLQ timeline view)
// and B5 (status lookup).
func (s *Store) ListOutboxEventsForMessage(ctx context.Context, orgID, outboxMessageID string) ([]OutboxEventRecord, error) {
	if orgID == "" || outboxMessageID == "" {
		return nil, errors.New("missing org_id or outbox_message_id")
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT id::text, org_id::text, outbox_message_id::text,
		       coalesce(provider_message_id, ''), event_type,
		       raw_payload, coalesce(reason, ''), created_at
		FROM outbox_events
		WHERE org_id = $1 AND outbox_message_id = $2
		ORDER BY created_at ASC
	`, orgID, outboxMessageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxEventRecord
	for rows.Next() {
		var r OutboxEventRecord
		if err := rows.Scan(&r.ID, &r.OrgID, &r.OutboxMessageID, &r.ProviderMessageID, &r.EventType, &r.RawPayload, &r.Reason, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReplayOutboxMessage resets a failed outbox row so the worker picks it
// up on the next claim cycle. Clears attempt_count, last_error, lock
// fields, and sets next_attempt_at to now. Only replays rows in the
// `failed` state; returns false when the row is not eligible.
func (s *Store) ReplayOutboxMessage(ctx context.Context, orgID, id string) (bool, error) {
	if orgID == "" || id == "" {
		return false, errors.New("missing org_id or id")
	}
	result, err := s.q.ExecContext(ctx, `
		UPDATE outbox_messages
		SET status = 'queued',
		    attempt_count = 0,
		    next_attempt_at = now(),
		    last_attempt_at = null,
		    last_error = null,
		    locked_at = null,
		    locked_by = null
		WHERE org_id = $1 AND id = $2 AND status = 'failed'
	`, orgID, id)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// enqueueSuppressedMessage inserts an outbox row directly in the
// failed/suppressed terminal state and appends a matching outbox_events
// row, without ever attempting provider delivery. The idempotency
// contract is preserved — if the same (org_id, idempotency_key) was
// previously enqueued, the existing row id is returned unchanged.
func (s *Store) enqueueSuppressedMessage(ctx context.Context, id string, msg OutboxMessage, suppressReason string) (string, error) {
	hash := contentHash(msg.To, msg.Subject, msg.TextBody, msg.HTMLBody)
	lastError := fmt.Sprintf("suppressed:%s", suppressReason)

	row := s.q.QueryRowContext(ctx, `
		WITH inserted AS (
			INSERT INTO outbox_messages (
				id, org_id, inbox_id, provider, idempotency_key,
				"to", "from", subject, text_body, html_body,
				content_hash, in_reply_to_message_id, "references",
				status, delivery_status, delivery_status_at, last_error, last_attempt_at
			)
			VALUES (
				$1, $2, $3, $4, $5,
				$6, $7, $8, nullif($9, ''), nullif($10, ''),
				$11, nullif($12, ''), nullif($13, ''),
				'failed', 'suppressed', now(), $14, now()
			)
			ON CONFLICT (org_id, idempotency_key)
			DO UPDATE SET idempotency_key = outbox_messages.idempotency_key
			RETURNING id, (xmax = 0) AS inserted
		)
		SELECT id, inserted FROM inserted
	`,
		id, msg.OrgID, msg.InboxID, msg.Provider, msg.IdempotencyKey,
		msg.To, msg.From, msg.Subject, msg.TextBody, msg.HTMLBody,
		hash, msg.InReplyToMessageID, msg.References,
		lastError,
	)
	var outID string
	var inserted bool
	if err := row.Scan(&outID, &inserted); err != nil {
		return "", fmt.Errorf("enqueue suppressed: %w", err)
	}

	// Append a suppression event to the audit timeline only on first
	// insert; on idempotency replay we don't want to keep stacking events.
	if inserted {
		payload, _ := json.Marshal(map[string]any{
			"event_type":      "suppressed_at_enqueue",
			"reason":          suppressReason,
			"recipient":       msg.To,
			"idempotency_key": msg.IdempotencyKey,
		})
		_ = s.InsertOutboxEvent(ctx, OutboxEvent{
			OrgID:           msg.OrgID,
			OutboxMessageID: outID,
			EventType:       "suppressed_at_enqueue",
			RawPayload:      payload,
			Reason:          suppressReason,
		})
	}
	return outID, nil
}
