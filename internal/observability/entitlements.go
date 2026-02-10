package observability

import (
	"log"
	"sync"
)

type EntitlementObserver struct {
	logger *log.Logger

	mu         sync.Mutex
	denyCounts map[string]int64
	warned80   map[string]bool
}

func NewEntitlementObserver(logger *log.Logger) *EntitlementObserver {
	if logger == nil {
		logger = log.Default()
	}
	return &EntitlementObserver{
		logger:     logger,
		denyCounts: make(map[string]int64),
		warned80:   make(map[string]bool),
	}
}

func (o *EntitlementObserver) RecordAllow(orgID string, reason string, used int64, limit int64) {
	if o == nil {
		return
	}
	utilization := 0.0
	if limit > 0 {
		utilization = float64(used) / float64(limit)
	}
	o.logger.Printf("entitlements allow org_id=%s reason=%s used=%d limit=%d utilization=%.4f", orgID, reason, used, limit, utilization)

	if utilization >= 0.8 {
		o.mu.Lock()
		alreadyWarned := o.warned80[orgID]
		if !alreadyWarned {
			o.warned80[orgID] = true
		}
		o.mu.Unlock()
		if !alreadyWarned {
			o.logger.Printf("entitlements warning org_id=%s threshold=0.80 used=%d limit=%d", orgID, used, limit)
		}
	}
}

func (o *EntitlementObserver) RecordDeny(orgID string, reason string) {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.denyCounts[orgID]++
	count := o.denyCounts[orgID]
	o.mu.Unlock()

	o.logger.Printf("entitlements deny org_id=%s reason=%s count=%d", orgID, reason, count)

	// Basic alert hook for repeated spikes in deny events.
	if count%10 == 0 {
		o.logger.Printf("entitlements alert org_id=%s reason=%s repeated_deny_count=%d", orgID, reason, count)
	}
}
