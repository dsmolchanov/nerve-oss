package reconcile

import (
	"context"
	"time"

	"neuralmail/internal/store"
)

type Service struct {
	Store *store.Store
	Now   func() time.Time
}

type Report struct {
	CountersRepaired   int
	PeriodsRolled      int
	OrgEventsFannedOut int
}

func NewService(st *store.Store) *Service {
	return &Service{
		Store: st,
		Now:   func() time.Time { return time.Now().UTC() },
	}
}

func (s *Service) Run(ctx context.Context) (Report, error) {
	var report Report
	if s == nil || s.Store == nil {
		return report, nil
	}

	counters, err := s.Store.ListOrgUsageCounters(ctx)
	if err != nil {
		return report, err
	}
	for _, counter := range counters {
		expected, err := s.Store.SumUsageEvents(ctx, counter.OrgID, counter.MeterName, counter.PeriodStart, counter.PeriodEnd)
		if err != nil {
			return report, err
		}
		if expected != counter.Used {
			if err := s.Store.SetOrgUsageCounterUsed(ctx, counter.OrgID, counter.MeterName, counter.PeriodStart, expected); err != nil {
				return report, err
			}
			report.CountersRepaired++
		}
	}

	now := s.Now()
	expired, err := s.Store.ListExpiredOrgEntitlements(ctx, now)
	if err != nil {
		return report, err
	}
	for _, ent := range expired {
		start, end := rolloverWindow(ent.UsagePeriodStart, ent.UsagePeriodEnd, now)
		if err := s.Store.UpdateOrgEntitlementUsagePeriod(ctx, ent.OrgID, start, end); err != nil {
			return report, err
		}
		if err := s.Store.EnsureOrgUsageCounter(ctx, ent.OrgID, "mcp_units", start, end); err != nil {
			return report, err
		}
		report.PeriodsRolled++
	}

	journalAvailable, err := s.Store.OrgEventJournalAvailable(ctx)
	if err != nil {
		return report, err
	}
	if journalAvailable {
		for {
			pending, err := s.Store.ListPendingOrgEvents(ctx, now.Add(-5*time.Minute), 100)
			if err != nil {
				return report, err
			}
			for _, event := range pending {
				if _, err := s.Store.ReFanOutOrgEvent(ctx, event.ID); err != nil {
					return report, err
				}
				report.OrgEventsFannedOut++
			}
			if len(pending) < 100 {
				break
			}
		}
	}

	return report, nil
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
