package entitlements

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"neuralmail/internal/auth"
	"neuralmail/internal/config"
	"neuralmail/internal/observability"
	"neuralmail/internal/store"
)

const meterMCPUnits = "mcp_units"

var ErrQuotaExceeded = errors.New("quota exceeded")

type RateLimitError struct {
	RetryAfterSeconds int
}

func (e *RateLimitError) Error() string {
	return "rate limited"
}

type Reservation struct {
	OrgID        string
	MeterName    string
	PeriodStart  time.Time
	PeriodEnd    time.Time
	Quantity     int64
	MonthlyUnits int64
	UsedAfter    int64
	Subscription string
}

type Service struct {
	Config config.Config
	Store  *store.Store

	RateLimiter *RateLimiter
	Observer    *observability.EntitlementObserver
	Now         func() time.Time

	defaultCost int64
	toolCosts   map[string]int64
}

func NewService(cfg config.Config, st *store.Store, observer *observability.EntitlementObserver) *Service {
	defaultCost, toolCosts := loadToolCosts(cfg.Metering.ToolCostPath)
	return &Service{
		Config:      cfg,
		Store:       st,
		RateLimiter: NewRateLimiter(),
		Observer:    observer,
		Now:         func() time.Time { return time.Now().UTC() },
		defaultCost: defaultCost,
		toolCosts:   toolCosts,
	}
}

func (s *Service) PreAuthorizeTool(ctx context.Context, principal auth.Principal, toolName string, replayID string) (*Reservation, error) {
	if s == nil || s.Store == nil {
		return nil, ErrSubscriptionInactive
	}
	if principal.OrgID == "" {
		return nil, ErrSubscriptionInactive
	}

	cost := s.toolCost(toolName)
	now := s.Now()
	var reservation *Reservation

	err := s.Store.RunAsOrg(ctx, principal.OrgID, func(scoped *store.Store) error {
		ent, err := scoped.GetOrgEntitlement(ctx, principal.OrgID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				s.Observer.RecordDeny(principal.OrgID, "missing_entitlement")
				return ErrSubscriptionInactive
			}
			return err
		}

		if now.After(ent.UsagePeriodEnd) {
			nextStart, nextEnd := rolloverWindow(ent.UsagePeriodStart, ent.UsagePeriodEnd, now)
			if err := scoped.UpdateOrgEntitlementUsagePeriod(ctx, principal.OrgID, nextStart, nextEnd); err != nil {
				return err
			}
			ent.UsagePeriodStart = nextStart
			ent.UsagePeriodEnd = nextEnd
		}

		if err := ValidateSubscriptionAccess(now, ent); err != nil {
			s.Observer.RecordDeny(principal.OrgID, "subscription_"+ent.SubscriptionStatus)
			return err
		}

		allowed, retryAfter := s.RateLimiter.Allow(principal.OrgID, ent.MCPRPM)
		if !allowed {
			s.Observer.RecordDeny(principal.OrgID, "rate_limited")
			return &RateLimitError{RetryAfterSeconds: retryAfter}
		}

		if err := scoped.EnsureOrgUsageCounter(ctx, principal.OrgID, meterMCPUnits, ent.UsagePeriodStart, ent.UsagePeriodEnd); err != nil {
			return err
		}
		reserved, usedAfter, err := scoped.ReserveOrgUsageUnits(ctx, principal.OrgID, meterMCPUnits, ent.UsagePeriodStart, cost, ent.MonthlyUnits)
		if err != nil {
			return err
		}
		if !reserved {
			s.Observer.RecordDeny(principal.OrgID, "quota_exceeded")
			return ErrQuotaExceeded
		}

		s.Observer.RecordAllow(principal.OrgID, "authorized", usedAfter, ent.MonthlyUnits)
		reservation = &Reservation{
			OrgID:        principal.OrgID,
			MeterName:    meterMCPUnits,
			PeriodStart:  ent.UsagePeriodStart,
			PeriodEnd:    ent.UsagePeriodEnd,
			Quantity:     cost,
			MonthlyUnits: ent.MonthlyUnits,
			UsedAfter:    usedAfter,
			Subscription: ent.SubscriptionStatus,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return reservation, nil
}

func (s *Service) FinalizeToolExecution(ctx context.Context, reservation Reservation, toolName string, replayID string, auditID string, status string) error {
	if s == nil || s.Store == nil {
		return nil
	}
	if reservation.OrgID == "" {
		return nil
	}

	normalizedStatus := "success"
	if status != "success" {
		normalizedStatus = "failed"
	}

	return s.Store.RunAsOrg(ctx, reservation.OrgID, func(scoped *store.Store) error {
		if normalizedStatus != "success" {
			if err := scoped.ReleaseOrgUsageUnits(ctx, reservation.OrgID, reservation.MeterName, reservation.PeriodStart, reservation.Quantity); err != nil {
				return err
			}
			s.Observer.RecordDeny(reservation.OrgID, "tool_execution_failed")
		}
		return scoped.RecordUsageEvent(ctx, reservation.OrgID, reservation.MeterName, reservation.Quantity, toolName, replayID, auditID, normalizedStatus)
	})
}

func (s *Service) toolCost(toolName string) int64 {
	if cost, ok := s.toolCosts[toolName]; ok && cost > 0 {
		return cost
	}
	if s.defaultCost <= 0 {
		return 1
	}
	return s.defaultCost
}

func rolloverWindow(periodStart, periodEnd, now time.Time) (time.Time, time.Time) {
	window := periodEnd.Sub(periodStart)
	if window <= 0 {
		window = 30 * 24 * time.Hour
	}
	start := periodStart
	end := periodEnd
	for now.After(end) {
		start = start.Add(window)
		end = end.Add(window)
	}
	return start, end
}

type toolCostConfig struct {
	DefaultUnitCost int64            `yaml:"default_unit_cost"`
	Tools           map[string]int64 `yaml:"tools"`
}

func loadToolCosts(path string) (int64, map[string]int64) {
	defaultCost := int64(1)
	costs := map[string]int64{}

	if path == "" {
		return defaultCost, costs
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return defaultCost, costs
	}
	var cfg toolCostConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return defaultCost, costs
	}
	if cfg.DefaultUnitCost > 0 {
		defaultCost = cfg.DefaultUnitCost
	}
	for tool, value := range cfg.Tools {
		if value > 0 {
			costs[tool] = value
		}
	}
	return defaultCost, costs
}
