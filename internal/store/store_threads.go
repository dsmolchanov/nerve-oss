package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

func (s *Store) ListThreads(ctx context.Context, inboxID string, status string, limit int) ([]Thread, error) {
	if limit <= 0 {
		limit = 50
	}
	query := `SELECT id, inbox_id, coalesce(subject,''), status, participants, updated_at, sentiment_score, priority_level, coalesce(provider_thread_id,'')
		FROM threads WHERE inbox_id = $1`
	args := []any{inboxID}
	if status != "" {
		query += " AND status = $2"
		args = append(args, status)
	}
	query += fmt.Sprintf(" ORDER BY updated_at DESC LIMIT $%d", len(args)+1)
	args = append(args, limit)

	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var threads []Thread
	for rows.Next() {
		var t Thread
		var participantsJSON []byte
		if err := rows.Scan(&t.ID, &t.InboxID, &t.Subject, &t.Status, &participantsJSON, &t.UpdatedAt, &t.SentimentScore, &t.PriorityLevel, &t.ProviderThreadID); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(participantsJSON, &t.Participants)
		threads = append(threads, t)
	}
	return threads, rows.Err()
}

func (s *Store) GetThread(ctx context.Context, threadID string) (Thread, []Message, error) {
	var t Thread
	var participantsJSON []byte
	row := s.q.QueryRowContext(ctx, `SELECT id, inbox_id, coalesce(subject,''), status, participants, updated_at, sentiment_score, priority_level, coalesce(provider_thread_id,'') FROM threads WHERE id = $1`, threadID)
	if err := row.Scan(&t.ID, &t.InboxID, &t.Subject, &t.Status, &participantsJSON, &t.UpdatedAt, &t.SentimentScore, &t.PriorityLevel, &t.ProviderThreadID); err != nil {
		return t, nil, err
	}
	_ = json.Unmarshal(participantsJSON, &t.Participants)

	rows, err := s.q.QueryContext(ctx, `SELECT id, inbox_id, thread_id, direction, coalesce(subject,''), coalesce(text,''), coalesce(html,''), created_at, coalesce(provider_message_id,''), coalesce(internet_message_id,''), coalesce(from_json,'{}'), coalesce(to_json,'[]'), coalesce(cc_json,'[]') FROM messages WHERE thread_id = $1 ORDER BY created_at ASC`, threadID)
	if err != nil {
		return t, nil, err
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var m Message
		var fromJSON, toJSON, ccJSON []byte
		if err := rows.Scan(&m.ID, &m.InboxID, &m.ThreadID, &m.Direction, &m.Subject, &m.Text, &m.HTML, &m.CreatedAt, &m.ProviderMessageID, &m.InternetMessageID, &fromJSON, &toJSON, &ccJSON); err != nil {
			return t, nil, err
		}
		_ = json.Unmarshal(fromJSON, &m.From)
		_ = json.Unmarshal(toJSON, &m.To)
		_ = json.Unmarshal(ccJSON, &m.CC)
		messages = append(messages, m)
	}
	return t, messages, rows.Err()
}

func (s *Store) GetThreadInboxID(ctx context.Context, threadID string) (string, error) {
	row := s.q.QueryRowContext(ctx, `SELECT inbox_id FROM threads WHERE id = $1`, threadID)
	var inboxID string
	if err := row.Scan(&inboxID); err != nil {
		return "", err
	}
	return inboxID, nil
}

func (s *Store) GetMessage(ctx context.Context, messageID string) (Message, error) {
	var m Message
	var fromJSON, toJSON, ccJSON []byte
	row := s.q.QueryRowContext(ctx, `SELECT id, inbox_id, thread_id, direction, subject, text, html, created_at, provider_message_id, internet_message_id, from_json, to_json, cc_json, coalesce(received_email_id, '') FROM messages WHERE id = $1`, messageID)
	if err := row.Scan(&m.ID, &m.InboxID, &m.ThreadID, &m.Direction, &m.Subject, &m.Text, &m.HTML, &m.CreatedAt, &m.ProviderMessageID, &m.InternetMessageID, &fromJSON, &toJSON, &ccJSON, &m.ReceivedEmailID); err != nil {
		return m, err
	}
	_ = json.Unmarshal(fromJSON, &m.From)
	_ = json.Unmarshal(toJSON, &m.To)
	_ = json.Unmarshal(ccJSON, &m.CC)
	return m, nil
}

func (s *Store) SearchInboxFTS(ctx context.Context, inboxID string, query string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.q.QueryContext(ctx, `SELECT m.id, m.thread_id, ts_rank_cd(to_tsvector('simple', coalesce(m.text,'')), plainto_tsquery('simple', $2)) AS score,
		substring(m.text from 1 for 200) AS snippet
		FROM messages m
		JOIN threads t ON t.id = m.thread_id
		WHERE t.inbox_id = $1 AND to_tsvector('simple', coalesce(m.text,'')) @@ plainto_tsquery('simple', $2)
		ORDER BY score DESC
		LIMIT $3`, inboxID, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.MessageID, &r.ThreadID, &r.Score, &r.Snippet); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

func (s *Store) UpsertThread(ctx context.Context, thread Thread) (string, error) {
	if thread.ID == "" {
		thread.ID = uuid.NewString()
	}
	participantsJSON, _ := json.Marshal(thread.Participants)
	_, err := s.q.ExecContext(ctx, `INSERT INTO threads (id, inbox_id, org_id, subject, status, participants, updated_at, sentiment_score, priority_level, provider_thread_id)
		VALUES ($1,$2,(SELECT org_id FROM inboxes WHERE id = $2),$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (id) DO UPDATE SET
			org_id = EXCLUDED.org_id,
			subject = EXCLUDED.subject,
			status = EXCLUDED.status,
			participants = EXCLUDED.participants,
			updated_at = EXCLUDED.updated_at,
			sentiment_score = EXCLUDED.sentiment_score,
			priority_level = EXCLUDED.priority_level,
			provider_thread_id = EXCLUDED.provider_thread_id`,
		thread.ID, thread.InboxID, thread.Subject, thread.Status, participantsJSON, thread.UpdatedAt, thread.SentimentScore, thread.PriorityLevel, thread.ProviderThreadID)
	if err != nil {
		return "", err
	}
	return thread.ID, nil
}

func (s *Store) InsertMessage(ctx context.Context, msg Message) (string, error) {
	if msg.ID == "" {
		msg.ID = uuid.NewString()
	}
	if msg.ProviderMessageID == "" {
		msg.ProviderMessageID = msg.ID
	}
	fromJSON, _ := json.Marshal(msg.From)
	toJSON, _ := json.Marshal(msg.To)
	ccJSON, _ := json.Marshal(msg.CC)

	var refs any
	if len(msg.References) > 0 {
		refs = msg.References
	}

	row := s.q.QueryRowContext(ctx, `INSERT INTO messages (id, inbox_id, org_id, thread_id, direction, subject, text, html, created_at, provider_message_id, internet_message_id, from_json, to_json, cc_json, in_reply_to, "references", received_email_id)
		VALUES ($1,$2,(SELECT org_id FROM inboxes WHERE id = $2),$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,nullif($14,''),$15,nullif($16,''))
		ON CONFLICT (inbox_id, provider_message_id) DO UPDATE SET thread_id = EXCLUDED.thread_id
		RETURNING id`,
		msg.ID, msg.InboxID, msg.ThreadID, msg.Direction, msg.Subject, msg.Text, msg.HTML, msg.CreatedAt, msg.ProviderMessageID, msg.InternetMessageID, fromJSON, toJSON, ccJSON, msg.InReplyTo, refs, msg.ReceivedEmailID)
	var id string
	if err := row.Scan(&id); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) EnsureThread(ctx context.Context, inboxID string, providerThreadID string, subject string, participants []Participant) (string, error) {
	if providerThreadID != "" {
		row := s.q.QueryRowContext(ctx, `UPDATE threads SET updated_at = $3 WHERE inbox_id = $1 AND provider_thread_id = $2 RETURNING id`, inboxID, providerThreadID, time.Now().UTC())
		var id string
		if err := row.Scan(&id); err == nil {
			return id, nil
		}
	}
	thread := Thread{
		ID:               uuid.NewString(),
		InboxID:          inboxID,
		Subject:          subject,
		Status:           "open",
		UpdatedAt:        time.Now().UTC(),
		Participants:     participants,
		ProviderThreadID: providerThreadID,
	}
	participantsJSON, _ := json.Marshal(thread.Participants)
	row := s.q.QueryRowContext(ctx, `INSERT INTO threads (id, inbox_id, org_id, subject, status, participants, updated_at, provider_thread_id)
		VALUES ($1,$2,(SELECT org_id FROM inboxes WHERE id = $2),$3,$4,$5,$6,$7)
		ON CONFLICT (inbox_id, provider_thread_id) DO UPDATE SET subject = EXCLUDED.subject, updated_at = EXCLUDED.updated_at
		RETURNING id`,
		thread.ID, thread.InboxID, thread.Subject, thread.Status, participantsJSON, thread.UpdatedAt, thread.ProviderThreadID)
	var id string
	if err := row.Scan(&id); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) UpdateThreadSignals(ctx context.Context, threadID string, sentiment *float64, priority string) error {
	_, err := s.q.ExecContext(ctx, `UPDATE threads SET sentiment_score = $2, priority_level = $3 WHERE id = $1`, threadID, sentiment, priority)
	return err
}

func (s *Store) InsertMessageWithThread(ctx context.Context, inboxID string, providerThreadID string, msg Message) (string, string, error) {
	threadID, err := s.EnsureThread(ctx, inboxID, providerThreadID, msg.Subject, append([]Participant{msg.From}, msg.To...))
	if err != nil {
		return "", "", err
	}
	msg.ThreadID = threadID
	msg.InboxID = inboxID
	msgID, err := s.InsertMessage(ctx, msg)
	return threadID, msgID, err
}

func (s *Store) MessageCount(ctx context.Context) (int, error) {
	row := s.q.QueryRowContext(ctx, `SELECT count(*) FROM messages`)
	var count int
	if err := row.Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) RecordToolCall(ctx context.Context, toolName string, idempotencyKey string, modelName string, promptVersion string, latencyMS int) (string, error) {
	id := uuid.NewString()
	_, err := s.q.ExecContext(ctx, `INSERT INTO tool_calls (id, tool_name, idempotency_key, model_name, prompt_version, latency_ms) VALUES ($1,$2,$3,$4,$5,$6)`,
		id, toolName, idempotencyKey, modelName, promptVersion, latencyMS)
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) RecordAudit(ctx context.Context, toolCallID string, actor string, inputsHash string, outputsHash string, replayID string) error {
	_, err := s.q.ExecContext(ctx, `INSERT INTO audit_log (tool_call_id, actor, inputs_hash, outputs_hash, replay_id) VALUES ($1,$2,$3,$4,$5)`,
		toolCallID, actor, inputsHash, outputsHash, replayID)
	return err
}

func (s *Store) ListAudit(ctx context.Context, limit int) ([]map[string]any, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.q.QueryContext(ctx, `SELECT a.id, a.replay_id, a.created_at, t.tool_name, t.latency_ms
		FROM audit_log a
		LEFT JOIN tool_calls t ON t.id = a.tool_call_id
		ORDER BY a.created_at DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []map[string]any
	for rows.Next() {
		var id, replayID, toolName sql.NullString
		var createdAt time.Time
		var latency sql.NullInt64
		if err := rows.Scan(&id, &replayID, &createdAt, &toolName, &latency); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"id":         id.String,
			"replay_id":  replayID.String,
			"created_at": createdAt,
			"tool_name":  toolName.String,
			"latency_ms": latency.Int64,
		})
	}
	return out, rows.Err()
}
