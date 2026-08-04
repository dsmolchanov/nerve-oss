package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// ThreadSummary extends Thread with aggregated data for list views.
type ThreadSummary struct {
	Thread
	MessageCount  int       `json:"message_count"`
	LatestSnippet string    `json:"latest_snippet"`
	LastMessageAt time.Time `json:"last_message_at"`
}

// ListThreadsForInbox returns threads for an inbox with message counts and snippets.
func (s *Store) ListThreadsForInbox(ctx context.Context, inboxID string, status string, limit, offset int) ([]ThreadSummary, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	query := `
		SELECT t.id, t.inbox_id, coalesce(t.subject,''), t.status, t.participants, t.updated_at,
		       t.sentiment_score, t.priority_level, coalesce(t.provider_thread_id, ''),
		       count(m.id) AS message_count,
		       coalesce(max(m.created_at), t.updated_at) AS last_message_at,
		       coalesce(substring(
		           (SELECT mm.text FROM messages mm WHERE mm.thread_id = t.id ORDER BY mm.created_at DESC LIMIT 1)
		           FROM 1 FOR 200
		       ), '') AS latest_snippet
		FROM threads t
		LEFT JOIN messages m ON m.thread_id = t.id
		WHERE t.inbox_id = $1`

	args := []any{inboxID}
	if status != "" {
		args = append(args, status)
		query += fmt.Sprintf(" AND t.status = $%d", len(args))
	}

	query += ` GROUP BY t.id`
	query += ` ORDER BY last_message_at DESC`
	args = append(args, limit)
	query += fmt.Sprintf(" LIMIT $%d", len(args))
	args = append(args, offset)
	query += fmt.Sprintf(" OFFSET $%d", len(args))

	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ThreadSummary
	for rows.Next() {
		var ts ThreadSummary
		var participantsJSON []byte
		if err := rows.Scan(
			&ts.ID, &ts.InboxID, &ts.Subject, &ts.Status, &participantsJSON, &ts.UpdatedAt,
			&ts.SentimentScore, &ts.PriorityLevel, &ts.ProviderThreadID,
			&ts.MessageCount, &ts.LastMessageAt, &ts.LatestSnippet,
		); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(participantsJSON, &ts.Participants)
		out = append(out, ts)
	}
	return out, rows.Err()
}

// ListOutboxForInbox returns outbox messages for an inbox with pagination.
func (s *Store) ListOutboxForInbox(ctx context.Context, inboxID string, limit, offset int) ([]OutboxMessage, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	rows, err := s.q.QueryContext(ctx, `
		SELECT id, org_id::text, inbox_id::text, provider, provider_message_id, idempotency_key,
		       "to", "from", subject, coalesce(text_body, ''), coalesce(html_body, ''),
		       status, delivery_status, delivery_status_at,
		       attempt_count, next_attempt_at, last_attempt_at, last_error,
		       locked_at, locked_by, created_at
		FROM outbox_messages
		WHERE inbox_id = $1
		ORDER BY created_at DESC, id DESC
		LIMIT $2 OFFSET $3
	`, inboxID, limit, offset)
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
