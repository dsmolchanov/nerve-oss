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
	q  queryer
}

type queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type CloudAPIKey struct {
	ID        string
	OrgID     string
	Scopes    []string
	RevokedAt sql.NullTime
}

type ServiceToken struct {
	ID        string
	OrgID     string
	Actor     string
	Scopes    []string
	ExpiresAt time.Time
	RevokedAt sql.NullTime
}

type OrgEntitlement struct {
	OrgID              string
	PlanCode           string
	SubscriptionStatus string
	MCPRPM             int
	MonthlyUnits       int64
	MaxInboxes         int
	UsagePeriodStart   time.Time
	UsagePeriodEnd     time.Time
	GraceUntil         sql.NullTime
	UpdatedAt          time.Time
}

type PlanEntitlement struct {
	PlanCode     string
	MCPRPM       int
	MonthlyUnits int64
	MaxInboxes   int
}

type SubscriptionRecord struct {
	OrgID                  string
	Provider               string
	ExternalCustomerID     string
	ExternalSubscriptionID string
	Status                 string
	CurrentPeriodStart     sql.NullTime
	CurrentPeriodEnd       sql.NullTime
	CancelAtPeriodEnd      bool
}

type SubscriptionSummary struct {
	OrgID                  string
	PlanCode               string
	SubscriptionStatus     string
	ExternalCustomerID     string
	ExternalSubscriptionID string
	CurrentPeriodStart     sql.NullTime
	CurrentPeriodEnd       sql.NullTime
	CancelAtPeriodEnd      bool
	GraceUntil             sql.NullTime
}

type UsageCounter struct {
	OrgID       string
	MeterName   string
	PeriodStart time.Time
	PeriodEnd   time.Time
	Used        int64
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
	return &Store{db: db, q: db}, nil
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

func (s *Store) RunAsOrg(ctx context.Context, orgID string, fn func(scoped *Store) error) error {
	if orgID == "" {
		return errors.New("missing org id")
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.cloud_mode', 'true', true)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_org_id', $1, true)`, orgID); err != nil {
		return err
	}

	scoped := &Store{db: s.db, q: tx}
	if err := fn(scoped); err != nil {
		return err
	}
	return tx.Commit()
}

type Thread struct {
	ID               string
	InboxID          string
	Subject          string
	Status           string
	Participants     []Participant
	UpdatedAt        time.Time
	SentimentScore   *float64
	PriorityLevel    string
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
	ProviderThreadID  string
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

var ErrOwnershipMismatch = errors.New("resource does not belong to org")

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
	row := s.q.QueryRowContext(ctx, `SELECT id, inbox_id, subject, status, participants, updated_at, sentiment_score, priority_level, provider_thread_id FROM threads WHERE id = $1`, threadID)
	if err := row.Scan(&t.ID, &t.InboxID, &t.Subject, &t.Status, &participantsJSON, &t.UpdatedAt, &t.SentimentScore, &t.PriorityLevel, &t.ProviderThreadID); err != nil {
		return t, nil, err
	}
	_ = json.Unmarshal(participantsJSON, &t.Participants)

	rows, err := s.q.QueryContext(ctx, `SELECT id, inbox_id, thread_id, direction, subject, text, html, created_at, provider_message_id, internet_message_id, from_json, to_json, cc_json FROM messages WHERE thread_id = $1 ORDER BY created_at ASC`, threadID)
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
	row := s.q.QueryRowContext(ctx, `SELECT id, inbox_id, thread_id, direction, subject, text, html, created_at, provider_message_id, internet_message_id, from_json, to_json, cc_json FROM messages WHERE id = $1`, messageID)
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
	row := s.q.QueryRowContext(ctx, `INSERT INTO messages (id, inbox_id, org_id, thread_id, direction, subject, text, html, created_at, provider_message_id, internet_message_id, from_json, to_json, cc_json)
		VALUES ($1,$2,(SELECT org_id FROM inboxes WHERE id = $2),$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (inbox_id, provider_message_id) DO UPDATE SET thread_id = EXCLUDED.thread_id
		RETURNING id`,
		msg.ID, msg.InboxID, msg.ThreadID, msg.Direction, msg.Subject, msg.Text, msg.HTML, msg.CreatedAt, msg.ProviderMessageID, msg.InternetMessageID, fromJSON, toJSON, ccJSON)
	var id string
	if err := row.Scan(&id); err != nil {
		return "", err
	}
	return id, nil
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

func (s *Store) EnsureInbox(ctx context.Context, address string) (string, error) {
	orgID, err := s.EnsureDefaultOrg(ctx)
	if err != nil {
		return "", err
	}
	var id string
	row := s.q.QueryRowContext(ctx, `SELECT id FROM inboxes WHERE address = $1`, address)
	switch err := row.Scan(&id); err {
	case nil:
		_, _ = s.q.ExecContext(ctx, `UPDATE inboxes SET org_id = COALESCE(org_id, $2) WHERE id = $1`, id, orgID)
		return id, nil
	case sql.ErrNoRows:
		id = uuid.NewString()
		_, err = s.q.ExecContext(ctx, `INSERT INTO inboxes (id, org_id, address, status) VALUES ($1,$2,$3,'active')`, id, orgID, address)
		return id, err
	default:
		return "", err
	}
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

func (s *Store) UpdateCheckpoint(ctx context.Context, inboxID string, provider string, lastState string) error {
	_, err := s.q.ExecContext(ctx, `INSERT INTO inbox_checkpoints (inbox_id, provider, last_state, updated_at)
		VALUES ($1,$2,$3,now())
		ON CONFLICT (inbox_id, provider) DO UPDATE SET last_state = EXCLUDED.last_state, updated_at = now()`, inboxID, provider, lastState)
	return err
}

func (s *Store) GetCheckpoint(ctx context.Context, inboxID string, provider string) (string, error) {
	row := s.q.QueryRowContext(ctx, `SELECT last_state FROM inbox_checkpoints WHERE inbox_id = $1 AND provider = $2`, inboxID, provider)
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
	rows, err := s.q.QueryContext(ctx, `SELECT id FROM inboxes ORDER BY created_at ASC`)
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

func (s *Store) ListInboxesByOrg(ctx context.Context, orgID string) ([]string, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT id FROM inboxes WHERE org_id = $1 ORDER BY created_at ASC`, orgID)
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
	row := s.q.QueryRowContext(ctx, `SELECT last_state FROM inbox_checkpoints WHERE inbox_id = $1`, inboxID)
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
	_, err := s.q.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1,$2)`, id, "default")
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) EnsureDefaultOrg(ctx context.Context) (string, error) {
	row := s.q.QueryRowContext(ctx, `SELECT id FROM orgs ORDER BY created_at ASC LIMIT 1`)
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
	row := s.q.QueryRowContext(ctx, `SELECT id FROM inboxes WHERE address = $1`, address)
	var id string
	if err := row.Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			id = uuid.NewString()
			_, err = s.q.ExecContext(ctx, `INSERT INTO inboxes (id, org_id, address, status) VALUES ($1,$2,$3,'active')`, id, orgID, address)
			return id, err
		}
		return "", err
	}
	_, _ = s.q.ExecContext(ctx, `UPDATE inboxes SET org_id = COALESCE(org_id, $2) WHERE id = $1`, id, orgID)
	return id, nil
}

func (s *Store) EnsureThread(ctx context.Context, inboxID string, providerThreadID string, subject string, participants []Participant) (string, error) {
	if providerThreadID != "" {
		row := s.q.QueryRowContext(ctx, `SELECT id FROM threads WHERE inbox_id = $1 AND provider_thread_id = $2`, inboxID, providerThreadID)
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

func (s *Store) EnsureDefaults(ctx context.Context, inboxAddress string) (string, error) {
	if inboxAddress == "" {
		return "", fmt.Errorf("missing inbox address")
	}
	return s.EnsureDefaultInbox(ctx, inboxAddress)
}

func (s *Store) LookupCloudAPIKey(ctx context.Context, keyHash string) (CloudAPIKey, error) {
	var key CloudAPIKey
	if keyHash == "" {
		return key, sql.ErrNoRows
	}
	row := s.q.QueryRowContext(ctx, `SELECT id, org_id, scopes, revoked_at FROM cloud_api_keys WHERE key_hash = $1`, keyHash)
	if err := row.Scan(&key.ID, &key.OrgID, &key.Scopes, &key.RevokedAt); err != nil {
		return key, err
	}
	return key, nil
}

func (s *Store) EnsureInboxBelongsToOrg(ctx context.Context, inboxID string, orgID string) error {
	return s.ensureBelongsToOrg(ctx, `SELECT EXISTS(SELECT 1 FROM inboxes WHERE id = $1 AND org_id = $2)`, inboxID, orgID)
}

func (s *Store) EnsureThreadBelongsToOrg(ctx context.Context, threadID string, orgID string) error {
	return s.ensureBelongsToOrg(ctx, `SELECT EXISTS(SELECT 1 FROM threads WHERE id = $1 AND org_id = $2)`, threadID, orgID)
}

func (s *Store) EnsureMessageBelongsToOrg(ctx context.Context, messageID string, orgID string) error {
	return s.ensureBelongsToOrg(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE id = $1 AND org_id = $2)`, messageID, orgID)
}

func (s *Store) ensureBelongsToOrg(ctx context.Context, query string, resourceID string, orgID string) error {
	if resourceID == "" || orgID == "" {
		return ErrOwnershipMismatch
	}
	var ok bool
	if err := s.q.QueryRowContext(ctx, query, resourceID, orgID).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrOwnershipMismatch
	}
	return nil
}

func (s *Store) CountInboxesByOrg(ctx context.Context, orgID string) (int, error) {
	row := s.q.QueryRowContext(ctx, `SELECT count(*) FROM inboxes WHERE org_id = $1`, orgID)
	var count int
	if err := row.Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) GetOrgEntitlement(ctx context.Context, orgID string) (OrgEntitlement, error) {
	var ent OrgEntitlement
	row := s.q.QueryRowContext(ctx, `
		SELECT org_id, plan_code, subscription_status, mcp_rpm, monthly_units, max_inboxes,
		       usage_period_start, usage_period_end, grace_until, updated_at
		FROM org_entitlements
		WHERE org_id = $1
	`, orgID)
	if err := row.Scan(
		&ent.OrgID,
		&ent.PlanCode,
		&ent.SubscriptionStatus,
		&ent.MCPRPM,
		&ent.MonthlyUnits,
		&ent.MaxInboxes,
		&ent.UsagePeriodStart,
		&ent.UsagePeriodEnd,
		&ent.GraceUntil,
		&ent.UpdatedAt,
	); err != nil {
		return ent, err
	}
	return ent, nil
}

func (s *Store) UpdateOrgEntitlementUsagePeriod(ctx context.Context, orgID string, usagePeriodStart, usagePeriodEnd time.Time) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE org_entitlements
		SET usage_period_start = $2, usage_period_end = $3, updated_at = now()
		WHERE org_id = $1
	`, orgID, usagePeriodStart, usagePeriodEnd)
	return err
}

func (s *Store) EnsureOrgUsageCounter(ctx context.Context, orgID string, meterName string, periodStart, periodEnd time.Time) error {
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO org_usage_counters (org_id, meter_name, period_start, period_end, used)
		VALUES ($1, $2, $3, $4, 0)
		ON CONFLICT (org_id, meter_name, period_start)
		DO UPDATE SET period_end = EXCLUDED.period_end
	`, orgID, meterName, periodStart, periodEnd)
	return err
}

func (s *Store) ReserveOrgUsageUnits(ctx context.Context, orgID string, meterName string, periodStart time.Time, quantity int64, monthlyUnits int64) (bool, int64, error) {
	var used int64
	row := s.q.QueryRowContext(ctx, `
		UPDATE org_usage_counters
		SET used = used + $4, updated_at = now()
		WHERE org_id = $1
		  AND meter_name = $2
		  AND period_start = $3
		  AND used + $4 <= $5
		RETURNING used
	`, orgID, meterName, periodStart, quantity, monthlyUnits)
	if err := row.Scan(&used); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, 0, nil
		}
		return false, 0, err
	}
	return true, used, nil
}

func (s *Store) ReleaseOrgUsageUnits(ctx context.Context, orgID string, meterName string, periodStart time.Time, quantity int64) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE org_usage_counters
		SET used = CASE WHEN used >= $4 THEN used - $4 ELSE 0 END, updated_at = now()
		WHERE org_id = $1
		  AND meter_name = $2
		  AND period_start = $3
	`, orgID, meterName, periodStart, quantity)
	return err
}

func (s *Store) GetOrgUsageCounterUsed(ctx context.Context, orgID string, meterName string, periodStart time.Time) (int64, error) {
	row := s.q.QueryRowContext(ctx, `
		SELECT used
		FROM org_usage_counters
		WHERE org_id = $1 AND meter_name = $2 AND period_start = $3
	`, orgID, meterName, periodStart)
	var used int64
	if err := row.Scan(&used); err != nil {
		return 0, err
	}
	return used, nil
}

func (s *Store) ListOrgUsageCounters(ctx context.Context) ([]UsageCounter, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT org_id, meter_name, period_start, period_end, used
		FROM org_usage_counters
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var counters []UsageCounter
	for rows.Next() {
		var item UsageCounter
		if err := rows.Scan(&item.OrgID, &item.MeterName, &item.PeriodStart, &item.PeriodEnd, &item.Used); err != nil {
			return nil, err
		}
		counters = append(counters, item)
	}
	return counters, rows.Err()
}

func (s *Store) SumUsageEvents(ctx context.Context, orgID string, meterName string, periodStart, periodEnd time.Time) (int64, error) {
	row := s.q.QueryRowContext(ctx, `
		SELECT coalesce(sum(quantity), 0)
		FROM usage_events
		WHERE org_id = $1
		  AND meter_name = $2
		  AND status = 'success'
		  AND created_at >= $3
		  AND created_at < $4
	`, orgID, meterName, periodStart, periodEnd)
	var total int64
	if err := row.Scan(&total); err != nil {
		return 0, err
	}
	return total, nil
}

func (s *Store) SetOrgUsageCounterUsed(ctx context.Context, orgID string, meterName string, periodStart time.Time, used int64) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE org_usage_counters
		SET used = $4, updated_at = now()
		WHERE org_id = $1
		  AND meter_name = $2
		  AND period_start = $3
	`, orgID, meterName, periodStart, used)
	return err
}

func (s *Store) ListExpiredOrgEntitlements(ctx context.Context, now time.Time) ([]OrgEntitlement, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT org_id, plan_code, subscription_status, mcp_rpm, monthly_units, max_inboxes,
		       usage_period_start, usage_period_end, grace_until, updated_at
		FROM org_entitlements
		WHERE usage_period_end < $1
	`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []OrgEntitlement
	for rows.Next() {
		var ent OrgEntitlement
		if err := rows.Scan(
			&ent.OrgID,
			&ent.PlanCode,
			&ent.SubscriptionStatus,
			&ent.MCPRPM,
			&ent.MonthlyUnits,
			&ent.MaxInboxes,
			&ent.UsagePeriodStart,
			&ent.UsagePeriodEnd,
			&ent.GraceUntil,
			&ent.UpdatedAt,
		); err != nil {
			return nil, err
		}
		items = append(items, ent)
	}
	return items, rows.Err()
}

func (s *Store) RecordUsageEvent(ctx context.Context, orgID string, meterName string, quantity int64, toolName string, replayID string, auditID string, status string) error {
	var audit sql.NullString
	if auditID != "" {
		audit = sql.NullString{String: auditID, Valid: true}
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO usage_events (id, org_id, meter_name, quantity, tool_name, replay_id, audit_id, status)
		VALUES ($1, $2, $3, $4, $5, $6, nullif($7, '')::uuid, $8)
	`, uuid.NewString(), orgID, meterName, quantity, toolName, replayID, audit.String, status)
	return err
}

func (s *Store) GetPlanEntitlement(ctx context.Context, planCode string) (PlanEntitlement, error) {
	var plan PlanEntitlement
	row := s.q.QueryRowContext(ctx, `
		SELECT plan_code, mcp_rpm, monthly_units, max_inboxes
		FROM plan_entitlements
		WHERE plan_code = $1
	`, planCode)
	if err := row.Scan(&plan.PlanCode, &plan.MCPRPM, &plan.MonthlyUnits, &plan.MaxInboxes); err != nil {
		return plan, err
	}
	return plan, nil
}

func (s *Store) UpsertSubscription(ctx context.Context, sub SubscriptionRecord) error {
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO subscriptions (
			org_id, provider, external_customer_id, external_subscription_id, status,
			current_period_start, current_period_end, cancel_at_period_end
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (external_subscription_id) DO UPDATE SET
			org_id = EXCLUDED.org_id,
			provider = EXCLUDED.provider,
			external_customer_id = EXCLUDED.external_customer_id,
			status = EXCLUDED.status,
			current_period_start = EXCLUDED.current_period_start,
			current_period_end = EXCLUDED.current_period_end,
			cancel_at_period_end = EXCLUDED.cancel_at_period_end,
			updated_at = now()
	`, sub.OrgID, sub.Provider, sub.ExternalCustomerID, sub.ExternalSubscriptionID, sub.Status, sub.CurrentPeriodStart, sub.CurrentPeriodEnd, sub.CancelAtPeriodEnd)
	return err
}

func (s *Store) UpdateSubscriptionStatusByExternalSubscriptionID(ctx context.Context, externalSubscriptionID string, status string) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE subscriptions
		SET status = $2, updated_at = now()
		WHERE external_subscription_id = $1
	`, externalSubscriptionID, status)
	return err
}

func (s *Store) UpdateSubscriptionStatusByExternalCustomerID(ctx context.Context, externalCustomerID string, status string) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE subscriptions
		SET status = $2, updated_at = now()
		WHERE external_customer_id = $1
	`, externalCustomerID, status)
	return err
}

func (s *Store) FindOrgByExternalCustomerID(ctx context.Context, externalCustomerID string) (string, error) {
	row := s.q.QueryRowContext(ctx, `
		SELECT org_id
		FROM subscriptions
		WHERE external_customer_id = $1
		ORDER BY updated_at DESC
		LIMIT 1
	`, externalCustomerID)
	var orgID string
	if err := row.Scan(&orgID); err != nil {
		return "", err
	}
	return orgID, nil
}

func (s *Store) FindOrgByExternalSubscriptionID(ctx context.Context, externalSubscriptionID string) (string, error) {
	row := s.q.QueryRowContext(ctx, `
		SELECT org_id
		FROM subscriptions
		WHERE external_subscription_id = $1
		ORDER BY updated_at DESC
		LIMIT 1
	`, externalSubscriptionID)
	var orgID string
	if err := row.Scan(&orgID); err != nil {
		return "", err
	}
	return orgID, nil
}

func (s *Store) FindStripeCustomerByOrg(ctx context.Context, orgID string) (string, error) {
	row := s.q.QueryRowContext(ctx, `
		SELECT external_customer_id
		FROM subscriptions
		WHERE org_id = $1 AND external_customer_id != ''
		ORDER BY updated_at DESC
		LIMIT 1
	`, orgID)
	var customerID string
	if err := row.Scan(&customerID); err != nil {
		return "", err
	}
	return customerID, nil
}

func (s *Store) UpsertOrgEntitlement(ctx context.Context, ent OrgEntitlement) error {
	var grace any
	if ent.GraceUntil.Valid {
		grace = ent.GraceUntil.Time
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO org_entitlements (
			org_id, plan_code, subscription_status, mcp_rpm, monthly_units, max_inboxes,
			usage_period_start, usage_period_end, grace_until
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (org_id) DO UPDATE SET
			plan_code = EXCLUDED.plan_code,
			subscription_status = EXCLUDED.subscription_status,
			mcp_rpm = EXCLUDED.mcp_rpm,
			monthly_units = EXCLUDED.monthly_units,
			max_inboxes = EXCLUDED.max_inboxes,
			usage_period_start = EXCLUDED.usage_period_start,
			usage_period_end = EXCLUDED.usage_period_end,
			grace_until = EXCLUDED.grace_until,
			updated_at = now()
	`, ent.OrgID, ent.PlanCode, ent.SubscriptionStatus, ent.MCPRPM, ent.MonthlyUnits, ent.MaxInboxes, ent.UsagePeriodStart, ent.UsagePeriodEnd, grace)
	return err
}

func (s *Store) InsertWebhookEventIfAbsent(ctx context.Context, provider string, externalEventID string, eventType string, payloadHash string) (bool, string, error) {
	result, err := s.q.ExecContext(ctx, `
		INSERT INTO webhook_events (provider, external_event_id, event_type, payload_hash, status)
		VALUES ($1, $2, $3, $4, 'received')
		ON CONFLICT (provider, external_event_id) DO NOTHING
	`, provider, externalEventID, eventType, payloadHash)
	if err != nil {
		return false, "", err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, "", err
	}
	if rows > 0 {
		return true, "", nil
	}

	row := s.q.QueryRowContext(ctx, `
		SELECT status
		FROM webhook_events
		WHERE provider = $1 AND external_event_id = $2
	`, provider, externalEventID)
	var status string
	if err := row.Scan(&status); err != nil {
		return false, "", err
	}
	return false, status, nil
}

func (s *Store) UpdateWebhookEventStatus(ctx context.Context, provider string, externalEventID string, status string, errorMessage string) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE webhook_events
		SET status = $3,
		    error_message = nullif($4, ''),
		    processed_at = now()
		WHERE provider = $1
		  AND external_event_id = $2
	`, provider, externalEventID, status, errorMessage)
	return err
}

func (s *Store) CreateOrg(ctx context.Context, name string) (string, error) {
	id := uuid.NewString()
	if name == "" {
		name = "organization"
	}
	_, err := s.q.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, $2)`, id, name)
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) GetSubscriptionSummaryByOrg(ctx context.Context, orgID string) (SubscriptionSummary, error) {
	var summary SubscriptionSummary
	row := s.q.QueryRowContext(ctx, `
		SELECT
			e.org_id,
			e.plan_code,
			e.subscription_status,
			coalesce(s.external_customer_id, ''),
			coalesce(s.external_subscription_id, ''),
			s.current_period_start,
			s.current_period_end,
			coalesce(s.cancel_at_period_end, false),
			e.grace_until
		FROM org_entitlements e
		LEFT JOIN subscriptions s ON s.org_id = e.org_id
		WHERE e.org_id = $1
		ORDER BY s.updated_at DESC NULLS LAST
		LIMIT 1
	`, orgID)
	if err := row.Scan(
		&summary.OrgID,
		&summary.PlanCode,
		&summary.SubscriptionStatus,
		&summary.ExternalCustomerID,
		&summary.ExternalSubscriptionID,
		&summary.CurrentPeriodStart,
		&summary.CurrentPeriodEnd,
		&summary.CancelAtPeriodEnd,
		&summary.GraceUntil,
	); err != nil {
		return summary, err
	}
	return summary, nil
}

func (s *Store) CreateServiceToken(ctx context.Context, tokenID string, orgID string, actor string, scopes []string, expiresAt time.Time) error {
	if tokenID == "" {
		tokenID = uuid.NewString()
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO service_tokens (id, org_id, actor, scopes, expires_at)
		VALUES ($1, $2, $3, $4, $5)
	`, tokenID, orgID, actor, scopes, expiresAt)
	return err
}

func (s *Store) RevokeActiveServiceTokens(ctx context.Context, orgID string) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE service_tokens
		SET revoked_at = now()
		WHERE org_id = $1
		  AND revoked_at IS NULL
		  AND expires_at > now()
	`, orgID)
	return err
}

func (s *Store) GetServiceToken(ctx context.Context, tokenID string) (ServiceToken, error) {
	var token ServiceToken
	if tokenID == "" {
		return token, sql.ErrNoRows
	}
	row := s.q.QueryRowContext(ctx, `
		SELECT id, org_id, actor, scopes, expires_at, revoked_at
		FROM service_tokens
		WHERE id = $1
	`, tokenID)
	if err := row.Scan(&token.ID, &token.OrgID, &token.Actor, &token.Scopes, &token.ExpiresAt, &token.RevokedAt); err != nil {
		return token, err
	}
	return token, nil
}
