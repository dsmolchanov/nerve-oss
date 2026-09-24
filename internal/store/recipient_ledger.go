package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrRecipientLedgerConflict           = errors.New("recipient ledger replay conflicts with stored contract")
	ErrRecipientLimit                    = errors.New("recipient allowance exhausted or period closed")
	ErrRecipientReplayRequiresNewMessage = errors.New("enrolled recipient meter requires a new outbox message for replay")
)

// RecipientPeriod is supplied by an authoritative billing projection. A null
// Limit means unlimited; zero means no allowance. Periods never roll locally.
type RecipientPeriod struct {
	OrgID    string
	PeriodID string
	StartsAt time.Time
	EndsAt   time.Time
	Limit    sql.NullInt64
}

func recipientIDs(org, id string) error {
	for _, value := range []string{org, id} {
		parsed, err := uuid.Parse(value)
		if err != nil || parsed == uuid.Nil || parsed.String() != value {
			return errors.New("recipient ledger requires canonical nonzero UUIDs")
		}
	}
	return nil
}

// recipientLedgerAvailable is checked through the caller's transaction. Core
// 28/29 continue serving legacy outbox rows, while an applied Core 30 must use
// the ledger table. A missing migration history is an error, never evidence
// that an enrolled organization can use the legacy meter.
func (s *Store) recipientLedgerAvailable(ctx context.Context) (bool, error) {
	if err := s.requireTx(); err != nil {
		return false, err
	}
	var applied bool
	err := s.q.QueryRowContext(ctx, `SELECT is_applied FROM schema_migrations_core
  WHERE version_id=30 ORDER BY id DESC LIMIT 1`).Scan(&applied)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return applied, err
}

func (s *Store) recipientOutboxFenceAvailable(ctx context.Context) (bool, error) {
	if err := s.requireTx(); err != nil {
		return false, err
	}
	var applied bool
	err := s.q.QueryRowContext(ctx, `SELECT is_applied FROM schema_migrations_core
  WHERE version_id=31 ORDER BY id DESC LIMIT 1`).Scan(&applied)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return applied, err
}

// RecipientMeterEnrolled persists across a closed or expired period. It lets
// runtime authorization keep legacy tool units isolated from the v2 contract
// even when the v2 send allowance is currently zero.
func (s *Store) RecipientMeterEnrolled(ctx context.Context, org string) (bool, error) {
	available, err := s.recipientLedgerAvailable(ctx)
	if err != nil || !available {
		return false, err
	}
	if err := recipientIDs(org, org); err != nil {
		return false, err
	}
	var enrolled bool
	err = s.q.QueryRowContext(ctx, `SELECT EXISTS(
  SELECT 1 FROM org_recipient_periods WHERE org_id=$1::uuid
)`, org).Scan(&enrolled)
	return enrolled, err
}

// InstallRecipientPeriod must share its caller's projection transaction. Exact
// replay is harmless; changing the allowance or boundaries requires a separate
// future projection transition, and cannot silently reset consumed counters.
func (s *Store) InstallRecipientPeriod(ctx context.Context, p RecipientPeriod) error {
	if err := s.requireTx(); err != nil {
		return err
	}
	if err := recipientIDs(p.OrgID, p.PeriodID); err != nil {
		return err
	}
	if p.StartsAt.IsZero() || !p.StartsAt.Before(p.EndsAt) || (p.Limit.Valid && p.Limit.Int64 < 0) {
		return errors.New("invalid recipient period")
	}
	_, err := s.q.ExecContext(ctx, `INSERT INTO org_recipient_periods
  (org_id,period_id,starts_at,ends_at,recipient_limit) VALUES ($1,$2,$3,$4,$5)
  ON CONFLICT (org_id,period_id) DO NOTHING`, p.OrgID, p.PeriodID, p.StartsAt, p.EndsAt, p.Limit)
	if err != nil {
		return err
	}
	var matches bool
	err = s.q.QueryRowContext(ctx, `SELECT starts_at=$3 AND ends_at=$4 AND recipient_limit IS NOT DISTINCT FROM $5::bigint
  FROM org_recipient_periods WHERE org_id=$1 AND period_id=$2 FOR UPDATE`, p.OrgID, p.PeriodID, p.StartsAt, p.EndsAt, p.Limit).Scan(&matches)
	if err != nil {
		return err
	}
	if !matches {
		return ErrRecipientLedgerConflict
	}
	return nil
}

// RecipientAdmissionPeriod identifies the single live period for an enrolled
// organization. An organization with no period rows still uses its legacy
// meter; once enrolled, an expired or closed period never falls back to it.
// Callers must hold their billing/lifecycle authority lock and reserve in the
// same transaction. ReserveRecipients rechecks the period after locking it.
func (s *Store) RecipientAdmissionPeriod(ctx context.Context, org string) (period string, enrolled bool, err error) {
	if err := s.requireTx(); err != nil {
		return "", false, err
	}
	if err := recipientIDs(org, org); err != nil {
		return "", false, err
	}
	rows, err := s.q.QueryContext(ctx, `SELECT period_id::text FROM org_recipient_periods
  WHERE org_id=$1 AND NOT admission_closed
    AND starts_at <= clock_timestamp() AND ends_at > clock_timestamp()
  LIMIT 2`, org)
	if err != nil {
		return "", false, err
	}
	var active []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return "", false, err
		}
		active = append(active, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", false, err
	}
	if err := rows.Close(); err != nil {
		return "", false, err
	}
	if len(active) > 1 {
		return "", true, ErrRecipientLedgerConflict
	}
	if len(active) == 1 {
		return active[0], true, nil
	}
	if err := s.q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM org_recipient_periods WHERE org_id=$1)`, org).Scan(&enrolled); err != nil {
		return "", false, err
	}
	if enrolled {
		return "", true, ErrRecipientLimit
	}
	return "", false, nil
}

// ReserveRecipients is an opt-in primitive. Its caller must establish outbox
// ownership and all live lifecycle/policy
// fences and insert the outbox row in this same transaction. The org-period row
// serializes accounting; callers must propagate errors to roll back the whole
// transaction. Replays retain their original terminal state and never re-charge.
func (s *Store) ReserveRecipients(ctx context.Context, org, period, outbox string, count int64) (string, error) {
	if err := s.requireTx(); err != nil {
		return "", err
	}
	if err := recipientIDs(org, period); err != nil {
		return "", err
	}
	if err := recipientIDs(org, outbox); err != nil {
		return "", err
	}
	if count <= 0 {
		return "", errors.New("recipient count must be positive")
	}
	var active bool
	err := s.q.QueryRowContext(ctx, `SELECT NOT admission_closed AND starts_at <= clock_timestamp() AND ends_at > clock_timestamp()
  FROM org_recipient_periods WHERE org_id=$1 AND period_id=$2 FOR UPDATE`, org, period).Scan(&active)
	if err != nil {
		return "", err
	}
	var storedPeriod, state string
	var storedCount int64
	err = s.q.QueryRowContext(ctx, `SELECT period_id,recipient_count,state FROM recipient_reservations WHERE org_id=$1 AND outbox_id=$2`, org, outbox).Scan(&storedPeriod, &storedCount, &state)
	if err == nil {
		if storedPeriod != period || storedCount != count {
			return "", ErrRecipientLedgerConflict
		}
		return state, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if !active {
		return "", ErrRecipientLimit
	}
	// Insert first: concurrent attempts to bind one outbox to different periods
	// cannot modify counters unless they won the unique reservation identity.
	result, err := s.q.ExecContext(ctx, `INSERT INTO recipient_reservations(org_id,outbox_id,period_id,recipient_count)
  VALUES ($1,$2,$3,$4) ON CONFLICT (org_id,outbox_id) DO NOTHING`, org, outbox, period, count)
	if err != nil {
		return "", err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return "", err
	}
	if n != 1 {
		return "", ErrRecipientLedgerConflict
	}
	// The initial SELECT may evaluate time before waiting on an unchanged row.
	// Recheck the live period at the actual admission UPDATE after acquiring it.
	result, err = s.q.ExecContext(ctx, `UPDATE org_recipient_periods SET reserved=reserved+$3
  WHERE org_id=$1 AND period_id=$2
  AND NOT admission_closed AND starts_at <= clock_timestamp() AND ends_at > clock_timestamp()
  AND $3 <= 9223372036854775807-committed-reserved
  AND (recipient_limit IS NULL OR $3 <= recipient_limit-committed-reserved)`, org, period, count)
	if err != nil {
		return "", err
	}
	n, err = result.RowsAffected()
	if err != nil {
		return "", err
	}
	if n != 1 {
		return "", ErrRecipientLimit
	}
	return "reserved", nil
}

// ResolveRecipients accepts only definitive accepted/rejected outcomes. Unknown
// outcomes do not call this method and retain their reservation after expiry or
// closure. A terminal replay is idempotent; a contradictory outcome is refused.
func (s *Store) ResolveRecipients(ctx context.Context, org, period, outbox, state string) error {
	if err := s.requireTx(); err != nil {
		return err
	}
	if err := recipientIDs(org, period); err != nil {
		return err
	}
	if err := recipientIDs(org, outbox); err != nil {
		return err
	}
	if state != "committed" && state != "released" {
		return errors.New("recipient resolution must be definitive")
	}
	var locked string
	if err := s.q.QueryRowContext(ctx, `SELECT period_id FROM org_recipient_periods WHERE org_id=$1 AND period_id=$2 FOR UPDATE`, org, period).Scan(&locked); err != nil {
		return err
	}
	var previous string
	var count int64
	if err := s.q.QueryRowContext(ctx, `SELECT state,recipient_count FROM recipient_reservations WHERE org_id=$1 AND period_id=$2 AND outbox_id=$3`, org, period, outbox).Scan(&previous, &count); err != nil {
		return err
	}
	if previous == state {
		return nil
	}
	if previous != "reserved" {
		return ErrRecipientLedgerConflict
	}
	committed := int64(0)
	if state == "committed" {
		committed = count
	}
	if _, err := s.q.ExecContext(ctx, `UPDATE org_recipient_periods SET reserved=reserved-$3,committed=committed+$4 WHERE org_id=$1 AND period_id=$2`, org, period, count, committed); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `UPDATE recipient_reservations SET state=$4,resolved_at=clock_timestamp() WHERE org_id=$1 AND period_id=$2 AND outbox_id=$3`, org, period, outbox, state)
	return err
}

// resolveOutboxRecipients must run in the same transaction as a definitive
// outbox transition. Outbox rows without a reservation predate enrollment and
// retain their legacy accounting. Unknown outcomes never call this helper.
func (s *Store) resolveOutboxRecipients(ctx context.Context, org, outbox, state string) error {
	available, err := s.recipientLedgerAvailable(ctx)
	if err != nil || !available {
		return err
	}
	var period string
	err = s.q.QueryRowContext(ctx, `SELECT period_id::text FROM recipient_reservations
  WHERE org_id=$1::uuid AND outbox_id=$2::uuid`, org, outbox).Scan(&period)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.ResolveRecipients(ctx, org, period, outbox, state)
}

func (s *Store) recipientOutboxPeriod(ctx context.Context, org, outbox string) (string, bool, error) {
	available, err := s.recipientLedgerAvailable(ctx)
	if err != nil || !available {
		return "", false, err
	}
	var period string
	var state string
	err = s.q.QueryRowContext(ctx, `SELECT period_id::text,state FROM recipient_reservations
  WHERE org_id=$1::uuid AND outbox_id=$2::uuid`, org, outbox).Scan(&period, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err == nil && state != "reserved" {
		return "", false, ErrOutboxClaimLost
	}
	if err != nil {
		return "", false, err
	}
	fenced, err := s.recipientOutboxFenceAvailable(ctx)
	if err != nil {
		return "", false, err
	}
	if !fenced {
		return "", false, &UnsupportedSchemaError{Operation: "recipient provider operation", RequiresCore: 31}
	}
	var meterVersion sql.NullInt64
	err = s.q.QueryRowContext(ctx, `SELECT recipient_meter_version FROM outbox_messages
  WHERE org_id=$1::uuid AND id=$2::uuid`, org, outbox).Scan(&meterVersion)
	if err != nil {
		return "", false, err
	}
	if !meterVersion.Valid || meterVersion.Int64 != 2 {
		return "", false, ErrRecipientLedgerConflict
	}
	return period, err == nil, err
}

func (s *Store) recipientPeriodAllowsDispatch(ctx context.Context, org, period string) (bool, error) {
	var allowed bool
	err := s.q.QueryRowContext(ctx, `SELECT NOT admission_closed
    AND starts_at <= clock_timestamp() AND ends_at > clock_timestamp()
  FROM org_recipient_periods WHERE org_id=$1::uuid AND period_id=$2::uuid FOR UPDATE`, org, period).Scan(&allowed)
	return allowed, err
}
