package entitlements

import (
	"database/sql"
	"testing"
	"time"

	"neuralmail/internal/store"
)

func TestValidateSubscriptionAccess(t *testing.T) {
	now := time.Date(2026, 2, 7, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		status    string
		grace     sql.NullTime
		periodEnd time.Time
		wantErr   bool
	}{
		{name: "trialing allowed", status: "trialing", wantErr: false},
		{name: "active allowed", status: "active", wantErr: false},
		{name: "past_due in grace allowed", status: "past_due", grace: sql.NullTime{Time: now.Add(24 * time.Hour), Valid: true}, wantErr: false},
		{name: "past_due out of grace denied", status: "past_due", grace: sql.NullTime{Time: now.Add(-24 * time.Hour), Valid: true}, wantErr: true},
		{name: "canceled before period end allowed", status: "canceled", periodEnd: now.Add(24 * time.Hour), wantErr: false},
		{name: "canceled after period end denied", status: "canceled", periodEnd: now.Add(-24 * time.Hour), wantErr: true},
		{name: "unpaid denied", status: "unpaid", wantErr: true},
		{name: "unknown denied", status: "unknown", wantErr: true},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ent := store.OrgEntitlement{
				SubscriptionStatus: tc.status,
				GraceUntil:         tc.grace,
				UsagePeriodEnd:     tc.periodEnd,
			}
			err := ValidateSubscriptionAccess(now, ent)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for status %s", tc.status)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("did not expect error for status %s: %v", tc.status, err)
			}
		})
	}
}
