package reconcile

import (
	"context"
	"time"

	"neuralmail/internal/store"
)

type Service struct {
	Store                       *store.Store
	Now                         func() time.Time
	OutboundAttachmentRetention time.Duration
	AttachmentGCGrace           time.Duration
}

type Report struct {
	CountersRepaired           int
	PeriodsRolled              int
	OrgEventsFannedOut         int
	AttachmentUsageSeeded      int
	AttachmentUsageRepaired    int
	OutboxAttachmentsReleased  int
	AttachmentBlobsDeleted     int
	AttachmentBytesReleased    int64
	OutboundRateBucketsDeleted int
}

func NewService(st *store.Store) *Service {
	return &Service{
		Store:                       st,
		Now:                         func() time.Time { return time.Now().UTC() },
		OutboundAttachmentRetention: 90 * 24 * time.Hour,
		AttachmentGCGrace:           7 * 24 * time.Hour,
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
		_, changed, err := s.Store.ReconcileOrgUsageCounter(ctx, counter)
		if err != nil {
			return report, err
		}
		if changed {
			report.CountersRepaired++
		}
	}

	now := s.Now()
	deletedBuckets, err := s.Store.DeleteExpiredOutboundUsageCounters(ctx, now.Add(-24*time.Hour))
	if err != nil {
		return report, err
	}
	report.OutboundRateBucketsDeleted = deletedBuckets
	coreVersion, err := store.CurrentVersionCore(ctx, s.Store.DB())
	if err != nil {
		return report, err
	}
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

	seeded, err := s.Store.SeedMissingOrgAttachmentUsage(ctx)
	if err != nil {
		return report, err
	}
	report.AttachmentUsageSeeded = seeded

	if coreVersion >= 25 {
		retention := s.OutboundAttachmentRetention
		if retention <= 0 {
			retention = 90 * 24 * time.Hour
		}
		for {
			released, err := s.Store.ReleaseSentOutboxAttachments(ctx, now.Add(-retention), 100)
			if err != nil {
				return report, err
			}
			report.OutboxAttachmentsReleased += released
			if released == 0 {
				break
			}
		}

		gcGrace := s.AttachmentGCGrace
		if gcGrace <= 0 {
			gcGrace = 7 * 24 * time.Hour
		}
		for {
			deleted, bytesReleased, err := s.Store.DeleteUnreferencedAttachmentBlobs(ctx, now.Add(-gcGrace), 100)
			if err != nil {
				return report, err
			}
			report.AttachmentBlobsDeleted += deleted
			report.AttachmentBytesReleased += bytesReleased
			if deleted == 0 {
				break
			}
		}
	}
	if coreVersion >= 22 {
		repaired, err := s.Store.ReconcileOrgAttachmentUsage(ctx)
		if err != nil {
			return report, err
		}
		report.AttachmentUsageRepaired = repaired
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
