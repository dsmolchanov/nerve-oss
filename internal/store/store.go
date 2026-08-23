package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type Store struct {
	db   *sql.DB
	q    queryer
	inTx bool
}

type queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Types: credentials

type CloudAPIKey struct {
	ID          string
	OrgID       string
	ExternalRef sql.NullString
	KeyPrefix   string
	Label       string
	Scopes      []string
	CreatedAt   time.Time
	RevokedAt   sql.NullTime
}

type ServiceToken struct {
	ID        string
	OrgID     string
	Actor     string
	Scopes    []string
	ExpiresAt time.Time
	RevokedAt sql.NullTime
}

// Types: billing & entitlements

type OrgEntitlement struct {
	OrgID              string
	PlanCode           string
	SubscriptionStatus string
	MCPRPM             int
	MonthlyUnits       int64
	MaxInboxes         int
	MaxDomains         int
	Features           json.RawMessage
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
	MaxDomains   int
	Features     json.RawMessage
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
	OrgID                  string     `json:"org_id"`
	PlanCode               string     `json:"plan_code"`
	SubscriptionStatus     string     `json:"subscription_status"`
	ExternalCustomerID     string     `json:"external_customer_id"`
	ExternalSubscriptionID string     `json:"external_subscription_id"`
	CurrentPeriodStart     *time.Time `json:"current_period_start"`
	CurrentPeriodEnd       *time.Time `json:"current_period_end"`
	CancelAtPeriodEnd      bool       `json:"cancel_at_period_end"`
	GraceUntil             *time.Time `json:"grace_until"`
	MCPRPM                 int        `json:"mcp_rpm"`
	MonthlyUnits           int64      `json:"monthly_units"`
	MaxInboxes             int        `json:"max_inboxes"`
	MaxDomains             int        `json:"max_domains"`
}

type UsageCounter struct {
	OrgID       string
	MeterName   string
	PeriodStart time.Time
	PeriodEnd   time.Time
	Used        int64
}

type UsageBreakdown struct {
	ToolName  string `json:"tool_name"`
	Category  string `json:"category"`
	CallCount int64  `json:"call_count"`
	UnitsUsed int64  `json:"units_used"`
}

// Types: threads & messages

type Thread struct {
	ID               string
	InboxID          string
	Subject          string
	Status           string
	Participants     []Participant
	UpdatedAt        time.Time
	SentimentScore   *float64
	PriorityLevel    *string
	ProviderThreadID string
}

type Message struct {
	ID                string
	InboxID           string
	ThreadID          string
	Direction         string
	AttachmentsState  string
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

	InReplyTo       string
	References      []string
	ReceivedEmailID string
	Attachments     []MessageAttachment
}

type Participant struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type SearchResult struct {
	MessageID string  `json:"message_id"`
	ThreadID  string  `json:"thread_id"`
	Score     float64 `json:"score"`
	Snippet   string  `json:"snippet"`
}

var ErrOwnershipMismatch = errors.New("resource does not belong to org")

// Core store lifecycle

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

	scoped := &Store{db: s.db, q: tx, inTx: true}
	if err := fn(scoped); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) withTx(ctx context.Context, fn func(*Store) error) error {
	if fn == nil {
		return errors.New("transaction callback is required")
	}
	if s.inTx {
		return fn(s)
	}
	if s.db == nil {
		return errors.New("store database is not configured")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	scoped := &Store{db: s.db, q: tx, inTx: true}
	if err := fn(scoped); err != nil {
		return err
	}
	return tx.Commit()
}

// RunInTx exposes the store's nested-transaction-safe transaction boundary to
// orchestration packages. Store methods that coordinate OAuth/onboarding state
// require the scoped Store passed to fn so callers cannot accidentally split a
// lifecycle transition across autocommit statements.
func (s *Store) RunInTx(ctx context.Context, fn func(*Store) error) error {
	return s.withTx(ctx, fn)
}

func (s *Store) requireTx() error {
	if !s.inTx {
		return errors.New("store operation requires an explicit transaction")
	}
	return nil
}

func (s *Store) HealthSummary(ctx context.Context) (map[string]string, error) {
	if err := s.db.PingContext(ctx); err != nil {
		return nil, err
	}
	return map[string]string{"database": "ok"}, nil
}

// Shared inbox helpers used by multiple files

func (s *Store) EnsureInbox(ctx context.Context, address string) (string, error) {
	return s.ensureInbox(ctx, address)
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

func (s *Store) CountInboxesByOrg(ctx context.Context, orgID string) (int, error) {
	row := s.q.QueryRowContext(ctx, `SELECT count(*) FROM inboxes WHERE org_id = $1`, orgID)
	var count int
	if err := row.Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// Ownership enforcement

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

// Shared helpers

func nullTimePtr(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	ts := value.Time.UTC()
	return &ts
}
