package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

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

	Status        string
	AttemptCount  int
	NextAttemptAt time.Time
	LastAttemptAt sql.NullTime
	LastError     sql.NullString
	LockedAt      sql.NullTime
	LockedBy      sql.NullString
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

	row := s.q.QueryRowContext(ctx, `
		INSERT INTO outbox_messages (id, org_id, inbox_id, provider, idempotency_key, "to", "from", subject, text_body, html_body)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, nullif($9, ''), nullif($10, ''))
		ON CONFLICT (org_id, idempotency_key)
		DO UPDATE SET idempotency_key = outbox_messages.idempotency_key
		RETURNING id
	`, id, msg.OrgID, msg.InboxID, msg.Provider, msg.IdempotencyKey, msg.To, msg.From, msg.Subject, msg.TextBody, msg.HTMLBody)
	var outID string
	if err := row.Scan(&outID); err != nil {
		return "", err
	}
	return outID, nil
}

func (s *Store) ClaimOutboxMessages(ctx context.Context, limit int, workerID string, now time.Time) ([]OutboxMessage, error) {
	if limit <= 0 {
		limit = 10
	}
	if workerID == "" {
		workerID = "worker"
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	rows, err := s.q.QueryContext(ctx, `
		WITH picked AS (
			SELECT id
			FROM outbox_messages
			WHERE status = 'queued'
			  AND next_attempt_at <= $1
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
		          o.status, o.attempt_count, o.next_attempt_at, o.last_attempt_at, o.last_error, o.locked_at, o.locked_by
	`, now, limit, workerID)
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
		); err != nil {
			return nil, err
		}
		out = append(out, msg)
	}
	return out, rows.Err()
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
