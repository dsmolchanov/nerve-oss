package store

import (
	"context"
	"time"
)

func (s *Store) GetOrgEntitlement(ctx context.Context, orgID string) (OrgEntitlement, error) {
	var ent OrgEntitlement
	row := s.q.QueryRowContext(ctx, `
		SELECT org_id, plan_code, subscription_status, mcp_rpm, monthly_units, max_inboxes, max_domains, features,
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
		&ent.MaxDomains,
		&ent.Features,
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
