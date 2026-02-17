package emailtransport

import (
	"context"
	"errors"

	"neuralmail/internal/store"
)

// InboundAdapter ingests messages from a provider into the Nerve store.
//
// In MVP we keep the seam minimal and provider-driven (adapter writes to store),
// matching the current JMAP ingest flow. Future providers can evolve this to a
// "fetch raw + normalize in core" model without changing MCP tools.
type InboundAdapter interface {
	Name() string
	Ingest(ctx context.Context, st *store.Store, inboxID string, cursor string) (newCursor string, messageIDs []string, err error)
}

type DeliveryStatus string

const (
	DeliveryStatusUnknown DeliveryStatus = "unknown"
	DeliveryStatusSent    DeliveryStatus = "sent"
	DeliveryStatusFailed  DeliveryStatus = "failed"
)

type OutboundMessage struct {
	From     string
	To       []string
	Subject  string
	TextBody string
	HTMLBody string
	Headers  map[string]string
}

type OutboundAdapter interface {
	Name() string
	SendMessage(ctx context.Context, msg OutboundMessage, idempotencyKey string) (providerMessageID string, err error)
	GetDeliveryStatus(ctx context.Context, providerMessageID string) (DeliveryStatus, error) // optional in MVP
}

type DNSRecord struct {
	Type  string
	Host  string
	Value string
	TTL   int
}

type DomainAdapter interface {
	Name() string
	DNSInstructions(domain string, verificationToken string) []DNSRecord
	VerifyDNS(ctx context.Context, domain string, verificationToken string) (verified bool, details string, err error)
	CreateInboxRoute(ctx context.Context, address string) (providerRouteID string, err error) // optional in MVP
}

var ErrNotSupported = errors.New("not supported")
