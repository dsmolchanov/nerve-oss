package jmap

import (
	"context"

	"neuralmail/internal/emailtransport"
	jmapapi "neuralmail/internal/jmap"
	"neuralmail/internal/store"
)

type InboundAdapter struct {
	client jmapapi.Client
}

func NewInboundAdapter(client jmapapi.Client) *InboundAdapter {
	return &InboundAdapter{client: client}
}

func (a *InboundAdapter) Name() string { return "jmap" }

func (a *InboundAdapter) Ingest(ctx context.Context, st *store.Store, inboxID string, cursor string) (string, []string, error) {
	return jmapapi.Ingest(ctx, a.client, st, inboxID, cursor)
}

var _ emailtransport.InboundAdapter = (*InboundAdapter)(nil)
