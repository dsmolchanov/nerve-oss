package entitlements

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"neuralmail/internal/auth"
	"neuralmail/internal/config"
	"neuralmail/internal/observability"
	"neuralmail/internal/store"
)

const meterMCPUnits = "mcp_units"
const meterMCPRequestsPerMinute = "mcp_requests_per_minute"

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
	Features     json.RawMessage
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

func (s *Service) PreAuthorizeTool(ctx context.Context, principal auth.Principal, toolName string, replayID string, idempotencyKey string) (*Reservation, error) {
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

		if toolName == "send_reply" || toolName == "compose_email" {
			// Default allow to preserve existing behavior until plans are explicitly feature-scoped.
			if !FeatureBool(ent.Features, "email_send_enabled", true) {
				s.Observer.RecordDeny(principal.OrgID, "send_disabled")
				return errors.New("send disabled by plan")
			}

			if idempotencyKey != "" {
				acquire, err := scoped.AcquireToolIdempotency(ctx, principal.OrgID, toolName, idempotencyKey, now, 15*time.Minute)
				if err != nil {
					return err
				}
				switch acquire.State {
				case store.ToolIdempotencyReplay:
					var cached any
					if err := json.Unmarshal(acquire.CachedResponse, &cached); err != nil {
						return err
					}
					return &IdempotencyReplayError{Response: cached}
				case store.ToolIdempotencyInProgress:
					return &IdempotencyInProgressError{RetryAfterSeconds: 2}
				case store.ToolIdempotencyAcquired:
					// proceed
				default:
					return &IdempotencyInProgressError{RetryAfterSeconds: 2}
				}
			}
		}

		allowed, retryAfter, err := s.reserveRateLimit(ctx, scoped, principal, toolName, replayID, now, ent.MCPRPM)
		if err != nil {
			return err
		}
		if !allowed {
			s.Observer.RecordDeny(principal.OrgID, "rate_limited")
			if idempotencyKey != "" && (toolName == "send_reply" || toolName == "compose_email") {
				_ = scoped.MarkToolIdempotencyFailed(ctx, principal.OrgID, toolName, idempotencyKey, now)
			}
			return &RateLimitError{RetryAfterSeconds: retryAfter}
		}

		if err := scoped.EnsureOrgUsageCounter(ctx, principal.OrgID, meterMCPUnits, ent.UsagePeriodStart, ent.UsagePeriodEnd); err != nil {
			if idempotencyKey != "" && (toolName == "send_reply" || toolName == "compose_email") {
				_ = scoped.MarkToolIdempotencyFailed(ctx, principal.OrgID, toolName, idempotencyKey, now)
			}
			return err
		}
		reserved, usedAfter, err := scoped.ReserveOrgUsageUnits(ctx, principal.OrgID, meterMCPUnits, ent.UsagePeriodStart, cost, ent.MonthlyUnits)
		if err != nil {
			if idempotencyKey != "" && (toolName == "send_reply" || toolName == "compose_email") {
				_ = scoped.MarkToolIdempotencyFailed(ctx, principal.OrgID, toolName, idempotencyKey, now)
			}
			return err
		}
		if !reserved {
			s.Observer.RecordDeny(principal.OrgID, "quota_exceeded")
			if idempotencyKey != "" && (toolName == "send_reply" || toolName == "compose_email") {
				_ = scoped.MarkToolIdempotencyFailed(ctx, principal.OrgID, toolName, idempotencyKey, now)
			}
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
			Features:     ent.Features,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return reservation, nil
}

// reserveRateLimit keeps the legacy process-local limiter for existing
// principals, while autonomous M2M orgs reserve from PostgreSQL so adding a
// runtime replica cannot multiply their configured RPM allowance.
func (s *Service) reserveRateLimit(
	ctx context.Context, scoped *store.Store, principal auth.Principal,
	toolName, replayID string, now time.Time, rpm int,
) (bool, int, error) {
	if principal.Kind != auth.PrincipalM2MOrg {
		allowed, retryAfter := s.RateLimiter.Allow(principal.OrgID, rpm)
		return allowed, retryAfter, nil
	}
	if rpm <= 0 {
		return false, 60, nil
	}
	periodStart := now.UTC().Truncate(time.Minute)
	periodEnd := periodStart.Add(time.Minute)
	if err := scoped.EnsureOrgUsageCounter(ctx, principal.OrgID, meterMCPRequestsPerMinute, periodStart, periodEnd); err != nil {
		return false, 0, err
	}
	reserved, _, err := scoped.ReserveOrgUsageUnits(
		ctx, principal.OrgID, meterMCPRequestsPerMinute, periodStart, 1, int64(rpm),
	)
	if err != nil {
		return false, 0, err
	}
	if !reserved {
		remaining := periodEnd.Sub(now)
		retryAfter := int((remaining + time.Second - 1) / time.Second)
		if retryAfter < 1 {
			retryAfter = 1
		}
		return false, retryAfter, nil
	}
	if replayID == "" {
		replayID = uuid.NewString()
	}
	rateReplayID := durableUsageReplayID(
		principal.OrgID, toolName, replayID, meterMCPRequestsPerMinute, "",
	)
	if err := scoped.RecordUsageEventAt(
		ctx, principal.OrgID, meterMCPRequestsPerMinute, 1, toolName,
		rateReplayID, "", "success", now,
	); err != nil {
		return false, 0, err
	}
	return true, 0, nil
}

func durableUsageReplayID(orgID, toolName, replayID, meter, dimension string) string {
	hash := sha256.New()
	for _, part := range []string{"mcp-usage-v1", orgID, toolName, replayID, meter, dimension} {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(part)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(part))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// FeatureBool extracts a boolean feature flag from a JSON entitlement blob.
// If the key is absent or the blob is invalid, defaultValue is returned.
func FeatureBool(raw json.RawMessage, key string, defaultValue bool) bool {
	if len(raw) == 0 {
		return defaultValue
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return defaultValue
	}
	v, ok := m[key]
	if !ok {
		return defaultValue
	}
	b, ok := v.(bool)
	if ok {
		return b
	}
	return defaultValue
}

func (s *Service) FinalizeToolExecution(ctx context.Context, reservation Reservation, toolName string, replayID string, auditID string, status string, idempotencyKey string, result any) error {
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
		now := s.Now()
		if normalizedStatus != "success" {
			if err := scoped.ReleaseOrgUsageUnits(ctx, reservation.OrgID, reservation.MeterName, reservation.PeriodStart, reservation.Quantity); err != nil {
				return err
			}
			s.Observer.RecordDeny(reservation.OrgID, "tool_execution_failed")
		}

		if idempotencyKey != "" && (toolName == "send_reply" || toolName == "compose_email") {
			if normalizedStatus == "success" {
				cached := result
				if m, ok := result.(map[string]any); ok {
					copyMap := make(map[string]any, len(m))
					for k, v := range m {
						copyMap[k] = v
					}
					delete(copyMap, "replay_id")
					delete(copyMap, "audit_id")
					cached = copyMap
				}
				cachedBytes, err := json.Marshal(cached)
				if err != nil {
					return err
				}
				if err := scoped.MarkToolIdempotencySucceeded(ctx, reservation.OrgID, toolName, idempotencyKey, cachedBytes, now); err != nil {
					return err
				}
			} else {
				if err := scoped.MarkToolIdempotencyFailed(ctx, reservation.OrgID, toolName, idempotencyKey, now); err != nil {
					return err
				}
			}
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
