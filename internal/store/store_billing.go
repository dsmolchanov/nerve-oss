package store

import (
	"context"
	"encoding/json"
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

func (s *Store) ListExpiredOrgEntitlements(ctx context.Context, now time.Time) ([]OrgEntitlement, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT org_id, plan_code, subscription_status, mcp_rpm, monthly_units, max_inboxes, max_domains,
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
			&ent.MaxDomains,
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

func (s *Store) UpsertOrgEntitlement(ctx context.Context, ent OrgEntitlement) error {
	var grace any
	if ent.GraceUntil.Valid {
		grace = ent.GraceUntil.Time
	}
	features := ent.Features
	if len(features) == 0 {
		features = json.RawMessage("{}")
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO org_entitlements (
			org_id, plan_code, subscription_status, mcp_rpm, monthly_units, max_inboxes, max_domains, features,
			usage_period_start, usage_period_end, grace_until
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (org_id) DO UPDATE SET
			plan_code = EXCLUDED.plan_code,
			subscription_status = EXCLUDED.subscription_status,
			mcp_rpm = EXCLUDED.mcp_rpm,
			monthly_units = EXCLUDED.monthly_units,
			max_inboxes = EXCLUDED.max_inboxes,
			max_domains = EXCLUDED.max_domains,
			features = EXCLUDED.features,
			usage_period_start = EXCLUDED.usage_period_start,
			usage_period_end = EXCLUDED.usage_period_end,
			grace_until = EXCLUDED.grace_until,
			updated_at = now()
	`, ent.OrgID, ent.PlanCode, ent.SubscriptionStatus, ent.MCPRPM, ent.MonthlyUnits, ent.MaxInboxes, ent.MaxDomains, features, ent.UsagePeriodStart, ent.UsagePeriodEnd, grace)
	return err
}

func (s *Store) GetPlanEntitlement(ctx context.Context, planCode string) (PlanEntitlement, error) {
	var plan PlanEntitlement
	row := s.q.QueryRowContext(ctx, `
		SELECT plan_code, mcp_rpm, monthly_units, max_inboxes, max_domains, features
		FROM plan_entitlements
		WHERE plan_code = $1
	`, planCode)
	if err := row.Scan(&plan.PlanCode, &plan.MCPRPM, &plan.MonthlyUnits, &plan.MaxInboxes, &plan.MaxDomains, &plan.Features); err != nil {
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
