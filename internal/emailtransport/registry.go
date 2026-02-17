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
