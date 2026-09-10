package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrRecipientLedgerConflict = errors.New("recipient ledger replay conflicts with stored contract")
	ErrRecipientLimit          = errors.New("recipient allowance exhausted or period closed")
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

// ReserveRecipients is an opt-in primitive, with no production callsites yet.
// Its caller must establish outbox ownership and all live lifecycle/policy
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
	result, err = s.q.ExecContext(ctx, `UPDATE org_recipient_periods SET reserved=reserved+$3
  WHERE org_id=$1 AND period_id=$2 AND $3 <= 9223372036854775807-committed-reserved
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
