package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type Store struct {
	db *sql.DB
}

func Open(dsn string) (*Store, error) {
	if dsn == "" {
		return nil, errors.New("missing database dsn")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	return &Store{db: db}, nil
}

func (s *Store) DB() *sql.DB {
	return s.db
}

func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

type Thread struct {
	ID              string
	InboxID         string
	Subject         string
	Status          string
	Participants    []Participant
	UpdatedAt       time.Time
	SentimentScore  *float64
	PriorityLevel   string
	ProviderThreadID string
}

type Message struct {
	ID                string
	InboxID           string
	ThreadID          string
	Direction         string
	Subject           string
	Text              string
	HTML              string
	CreatedAt         time.Time
	ProviderMessageID string
	InternetMessageID string
	From              Participant
	To                []Participant
	CC                []Participant
}

type Participant struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type SearchResult struct {
	MessageID string
	ThreadID  string
	Score     float64
	Snippet   string
}

func (s *Store) ListThreads(ctx context.Context, inboxID string, status string, limit int) ([]Thread, error) {
	if limit <= 0 {
		limit = 50
	}
	query := `SELECT id, inbox_id, subject, status, participants, updated_at, sentiment_score, priority_level, provider_thread_id
		FROM threads WHERE inbox_id = $1`
	args := []any{inboxID}
	if status != "" {
		query += " AND status = $2"
		args = append(args, status)
	}
	query += fmt.Sprintf(" ORDER BY updated_at DESC LIMIT $%d", len(args)+1)
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
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
	row := s.db.QueryRowContext(ctx, `SELECT id, inbox_id, subject, status, participants, updated_at, sentiment_score, priority_level, provider_thread_id FROM threads WHERE id = $1`, threadID)
	if err := row.Scan(&t.ID, &t.InboxID, &t.Subject, &t.Status, &participantsJSON, &t.UpdatedAt, &t.SentimentScore, &t.PriorityLevel, &t.ProviderThreadID); err != nil {
		return t, nil, err
	}
	_ = json.Unmarshal(participantsJSON, &t.Participants)

	rows, err := s.db.QueryContext(ctx, `SELECT id, inbox_id, thread_id, direction, subject, text, html, created_at, provider_message_id, internet_message_id, from_json, to_json, cc_json FROM messages WHERE thread_id = $1 ORDER BY created_at ASC`, threadID)
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
	row := s.db.QueryRowContext(ctx, `SELECT inbox_id FROM threads WHERE id = $1`, threadID)
	var inboxID string
	if err := row.Scan(&inboxID); err != nil {
		return "", err
	}
	return inboxID, nil
}

func (s *Store) GetMessage(ctx context.Context, messageID string) (Message, error) {
	var m Message
	var fromJSON, toJSON, ccJSON []byte
	row := s.db.QueryRowContext(ctx, `SELECT id, inbox_id, thread_id, direction, subject, text, html, created_at, provider_message_id, internet_message_id, from_json, to_json, cc_json FROM messages WHERE id = $1`, messageID)
	if err := row.Scan(&m.ID, &m.InboxID, &m.ThreadID, &m.Direction, &m.Subject, &m.Text, &m.HTML, &m.CreatedAt, &m.ProviderMessageID, &m.InternetMessageID, &fromJSON, &toJSON, &ccJSON); err != nil {
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
	rows, err := s.db.QueryContext(ctx, `SELECT m.id, m.thread_id, ts_rank_cd(to_tsvector('simple', coalesce(m.text,'')), plainto_tsquery('simple', $2)) AS score,
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO threads (id, inbox_id, subject, status, participants, updated_at, sentiment_score, priority_level, provider_thread_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (id) DO UPDATE SET subject = EXCLUDED.subject, status = EXCLUDED.status, participants = EXCLUDED.participants, updated_at = EXCLUDED.updated_at,
		sentiment_score = EXCLUDED.sentiment_score, priority_level = EXCLUDED.priority_level, provider_thread_id = EXCLUDED.provider_thread_id`,
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
	fromJSON, _ := json.Marshal(msg.From)
	toJSON, _ := json.Marshal(msg.To)
	ccJSON, _ := json.Marshal(msg.CC)
	_, err := s.db.ExecContext(ctx, `INSERT INTO messages (id, inbox_id, thread_id, direction, subject, text, html, created_at, provider_message_id, internet_message_id, from_json, to_json, cc_json)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		msg.ID, msg.InboxID, msg.ThreadID, msg.Direction, msg.Subject, msg.Text, msg.HTML, msg.CreatedAt, msg.ProviderMessageID, msg.InternetMessageID, fromJSON, toJSON, ccJSON)
	if err != nil {
		return "", err
	}
	return msg.ID, nil
}

func (s *Store) RecordToolCall(ctx context.Context, toolName string, idempotencyKey string, modelName string, promptVersion string, latencyMS int) (string, error) {
	id := uuid.NewString()
	_, err := s.db.ExecContext(ctx, `INSERT INTO tool_calls (id, tool_name, idempotency_key, model_name, prompt_version, latency_ms) VALUES ($1,$2,$3,$4,$5,$6)`,
		id, toolName, idempotencyKey, modelName, promptVersion, latencyMS)
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) RecordAudit(ctx context.Context, toolCallID string, actor string, inputsHash string, outputsHash string, replayID string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit_log (tool_call_id, actor, inputs_hash, outputs_hash, replay_id) VALUES ($1,$2,$3,$4,$5)`,
		toolCallID, actor, inputsHash, outputsHash, replayID)
	return err
}

func (s *Store) EnsureInbox(ctx context.Context, address string) (string, error) {
	var id string
	row := s.db.QueryRowContext(ctx, `SELECT id FROM inboxes WHERE address = $1`, address)
	switch err := row.Scan(&id); err {
	case nil:
		return id, nil
	case sql.ErrNoRows:
		id = uuid.NewString()
		_, err = s.db.ExecContext(ctx, `INSERT INTO inboxes (id, address, status) VALUES ($1,$2,'active')`, id, address)
		return id, err
	default:
		return "", err
	}
}

func (s *Store) ListAudit(ctx context.Context, limit int) ([]map[string]any, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `SELECT a.id, a.replay_id, a.created_at, t.tool_name, t.latency_ms
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

func (s *Store) UpdateCheckpoint(ctx context.Context, inboxID string, provider string, lastState string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO inbox_checkpoints (inbox_id, provider, last_state, updated_at)
		VALUES ($1,$2,$3,now())
		ON CONFLICT (inbox_id, provider) DO UPDATE SET last_state = EXCLUDED.last_state, updated_at = now()`, inboxID, provider, lastState)
	return err
}

func (s *Store) GetCheckpoint(ctx context.Context, inboxID string, provider string) (string, error) {
	row := s.db.QueryRowContext(ctx, `SELECT last_state FROM inbox_checkpoints WHERE inbox_id = $1 AND provider = $2`, inboxID, provider)
	var state sql.NullString
	if err := row.Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return state.String, nil
}

func (s *Store) ListInboxes(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM inboxes ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) LastInboxState(ctx context.Context, inboxID string) (string, error) {
	row := s.db.QueryRowContext(ctx, `SELECT last_state FROM inbox_checkpoints WHERE inbox_id = $1`, inboxID)
	var state sql.NullString
	if err := row.Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return state.String, nil
}

func (s *Store) HealthSummary(ctx context.Context) (map[string]string, error) {
	if err := s.db.PingContext(ctx); err != nil {
		return nil, err
	}
	return map[string]string{"database": "ok"}, nil
}

func (s *Store) CreateDefaultOrg(ctx context.Context) (string, error) {
	id := uuid.NewString()
	_, err := s.db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1,$2)`, id, "default")
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) EnsureDefaultOrg(ctx context.Context) (string, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id FROM orgs ORDER BY created_at ASC LIMIT 1`)
	var id string
	if err := row.Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return s.CreateDefaultOrg(ctx)
		}
		return "", err
	}
	return id, nil
}

func (s *Store) EnsureDefaultInbox(ctx context.Context, address string) (string, error) {
	orgID, err := s.EnsureDefaultOrg(ctx)
	if err != nil {
		return "", err
	}
	row := s.db.QueryRowContext(ctx, `SELECT id FROM inboxes WHERE address = $1`, address)
	var id string
	if err := row.Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			id = uuid.NewString()
			_, err = s.db.ExecContext(ctx, `INSERT INTO inboxes (id, org_id, address, status) VALUES ($1,$2,$3,'active')`, id, orgID, address)
			return id, err
		}
		return "", err
	}
	return id, nil
}

func (s *Store) UpsertThreadForMessage(ctx context.Context, inboxID string, subject string, participants []Participant) (string, error) {
	thread := Thread{
		ID:         uuid.NewString(),
		InboxID:    inboxID,
		Subject:    subject,
		Status:     "open",
		UpdatedAt:  time.Now().UTC(),
		Participants: participants,
	}
	return s.UpsertThread(ctx, thread)
}

func (s *Store) UpdateThreadSignals(ctx context.Context, threadID string, sentiment *float64, priority string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE threads SET sentiment_score = $2, priority_level = $3 WHERE id = $1`, threadID, sentiment, priority)
	return err
}

func (s *Store) InsertMessageWithThread(ctx context.Context, inboxID string, msg Message) (string, string, error) {
	threadID, err := s.UpsertThreadForMessage(ctx, inboxID, msg.Subject, append([]Participant{msg.From}, msg.To...))
	if err != nil {
		return "", "", err
	}
	msg.ThreadID = threadID
	msg.InboxID = inboxID
	msgID, err := s.InsertMessage(ctx, msg)
	return threadID, msgID, err
}

func (s *Store) MessageCount(ctx context.Context) (int, error) {
	row := s.db.QueryRowContext(ctx, `SELECT count(*) FROM messages`)
	var count int
	if err := row.Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) EnsureDefaults(ctx context.Context, inboxAddress string) (string, error) {
	if inboxAddress == "" {
		return "", fmt.Errorf("missing inbox address")
	}
	return s.EnsureDefaultInbox(ctx, inboxAddress)
}
