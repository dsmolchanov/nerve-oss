package entitlements

import (
	"errors"
	"strings"
	"time"

	"neuralmail/internal/store"
)

var ErrSubscriptionInactive = errors.New("subscription inactive")

func ValidateSubscriptionAccess(now time.Time, ent store.OrgEntitlement) error {
	switch strings.ToLower(strings.TrimSpace(ent.SubscriptionStatus)) {
	case "trialing", "active":
		return nil
	case "past_due":
		if ent.GraceUntil.Valid && !now.After(ent.GraceUntil.Time) {
			return nil
		}
		return ErrSubscriptionInactive
	case "canceled":
		if !now.After(ent.UsagePeriodEnd) {
			return nil
		}
		return ErrSubscriptionInactive
	case "unpaid":
		return ErrSubscriptionInactive
	default:
		return ErrSubscriptionInactive
	}
}
