package emailtransport

import (
	"fmt"
	"sync"
)

type Registry struct {
	mu sync.RWMutex

	inbound  map[string]InboundAdapter
	outbound map[string]OutboundAdapter
	domain   map[string]DomainAdapter
}

func NewRegistry() *Registry {
	return &Registry{
		inbound:  make(map[string]InboundAdapter),
		outbound: make(map[string]OutboundAdapter),
		domain:   make(map[string]DomainAdapter),
	}
}

func (r *Registry) RegisterInbound(adapter InboundAdapter) error {
	if adapter == nil || adapter.Name() == "" {
		return fmt.Errorf("invalid inbound adapter")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inbound[adapter.Name()] = adapter
	return nil
}

func (r *Registry) RegisterOutbound(adapter OutboundAdapter) error {
	if adapter == nil || adapter.Name() == "" {
		return fmt.Errorf("invalid outbound adapter")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outbound[adapter.Name()] = adapter
	return nil
}

func (r *Registry) RegisterDomain(adapter DomainAdapter) error {
	if adapter == nil || adapter.Name() == "" {
		return fmt.Errorf("invalid domain adapter")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.domain[adapter.Name()] = adapter
	return nil
}

func (r *Registry) Inbound(name string) (InboundAdapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	adapter, ok := r.inbound[name]
	return adapter, ok
}

func (r *Registry) Outbound(name string) (OutboundAdapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	adapter, ok := r.outbound[name]
	return adapter, ok
}

func (r *Registry) Domain(name string) (DomainAdapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	adapter, ok := r.domain[name]
	return adapter, ok
}

// OutboundCount returns the number of registered outbound adapters.
// Used by the readiness check to assert "at least one adapter is
// registered" without naming a specific provider — so SMTP-only or
// Resend-only deployments are both valid.
func (r *Registry) OutboundCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.outbound)
}
