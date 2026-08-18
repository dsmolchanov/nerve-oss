package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

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

func (s *Store) RecordUsageEvent(ctx context.Context, orgID string, meterName string, quantity int64, toolName string, replayID string, auditID string, status string) error {
	return s.RecordUsageEventAt(ctx, orgID, meterName, quantity, toolName, replayID, auditID, status, time.Now().UTC())
}

func (s *Store) RecordUsageEventAt(ctx context.Context, orgID string, meterName string, quantity int64, toolName string, replayID string, auditID string, status string, createdAt time.Time) error {
	var audit sql.NullString
	if auditID != "" {
		audit = sql.NullString{String: auditID, Valid: true}
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO usage_events (id, org_id, meter_name, quantity, tool_name, replay_id, audit_id, status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, nullif($7, '')::uuid, $8, $9)
	`, uuid.NewString(), orgID, meterName, quantity, toolName, replayID, audit.String, status, createdAt)
	return err
}

// ReconcileOrgUsageCounter takes the same counter-row lock used implicitly by
// reservations and holds it across SUM+SET, so reconciliation cannot erase a
// reservation committed concurrently on another runtime replica.
func (s *Store) ReconcileOrgUsageCounter(ctx context.Context, counter UsageCounter) (int64, bool, error) {
	var expected int64
	changed := false
	err := s.RunAsOrg(ctx, counter.OrgID, func(scoped *Store) error {
		var current int64
		if err := scoped.q.QueryRowContext(ctx, `
			SELECT used
			FROM org_usage_counters
			WHERE org_id = $1 AND meter_name = $2 AND period_start = $3
			FOR UPDATE
		`, counter.OrgID, counter.MeterName, counter.PeriodStart).Scan(&current); err != nil {
			return err
		}
		var err error
		expected, err = scoped.SumUsageEvents(ctx, counter.OrgID, counter.MeterName, counter.PeriodStart, counter.PeriodEnd)
		if err != nil {
			return err
		}
		if expected == current {
			return nil
		}
		if err := scoped.SetOrgUsageCounterUsed(ctx, counter.OrgID, counter.MeterName, counter.PeriodStart, expected); err != nil {
			return err
		}
		changed = true
		return nil
	})
	return expected, changed, err
}
