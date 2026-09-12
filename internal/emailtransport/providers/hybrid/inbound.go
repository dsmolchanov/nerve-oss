package hybrid

import (
	"context"
	"errors"
	"fmt"
	"time"

	"neuralmail/internal/emailtransport"
	"neuralmail/internal/store"
)

// maxIngestBatch bounds one Ingest call. Cloud leases one delivery per poll,
// so this is the number of round trips a single tick may spend before other
// work gets the loop back.
const maxIngestBatch = 50

// InboundAdapter pulls deliveries Cloud is holding and writes them into the
// local store.
//
// The runtime pulls rather than being pushed to: a self-hosted host usually
// has no inbound route from the internet, and pulling means Cloud never needs
// a credential for this machine.
type InboundAdapter struct {
	Client *Client
	// AuthorityID namespaces the local deduplication key. Two Cloud
	// deployments can hand out the same delivery UUID; without the namespace
	// the second one's message would be silently dropped as a replay of the
	// first.
	AuthorityID string
	Now         func() time.Time
}

func (a *InboundAdapter) Name() string { return ProviderName }

func (a *InboundAdapter) now() time.Time {
	if a.Now != nil {
		return a.Now().UTC()
	}
	return time.Now().UTC()
}

// LocalDeliveryID is the durable deduplication key for one Cloud delivery.
//
// It is derived only from identifiers that outlive a restart, so a runtime
// that crashed between storing a message and acknowledging it recognises the
// redelivery instead of storing a second copy. Nothing about the current
// process, lease or attempt may enter this key.
func LocalDeliveryID(authorityID string, delivery Delivery) string {
	return fmt.Sprintf("hybrid:%s:%s:%s:%s", authorityID, delivery.OrgID, delivery.InstallationID, delivery.ID)
}

// Ingest drains up to one batch of deliveries.
//
// Ordering is deliberate: store first, acknowledge second. Acknowledging first
// would let a crash in between lose the message outright, because Cloud is
// then free to purge its copy. Storing first can only ever cost a duplicate
// delivery, and the store's own (inbox, provider_message_id) key absorbs that.
//
// The cursor stays empty: Cloud holds the queue and the lease, so there is no
// local position to resume from, and a stale cursor would be a way to skip
// mail.
func (a *InboundAdapter) Ingest(ctx context.Context, st *store.Store, inboxID string, _ string) (string, []string, error) {
	if a.Client == nil {
		return "", nil, errors.New("hybrid inbound adapter has no cloud client")
	}
	var stored []string
	for range maxIngestBatch {
		if err := ctx.Err(); err != nil {
			return "", stored, err
		}
		delivery, err := a.Client.Poll(ctx)
		if errors.Is(err, ErrMissing) {
			return "", stored, nil
		}
		if err != nil {
			return "", stored, err
		}
		messageID, err := a.store(ctx, st, inboxID, delivery)
		if err != nil {
			// Leave it unacknowledged. The lease expires and Cloud offers the
			// delivery again; losing it here would be unrecoverable.
			return "", stored, err
		}
		if err := a.Client.Ack(ctx, delivery.ID, delivery.LeaseToken); err != nil {
			// The message is already durable locally. A failed ack costs one
			// redelivery, which deduplicates, so this is not a loss.
			return "", append(stored, messageID), err
		}
		stored = append(stored, messageID)
	}
	return "", stored, nil
}

func (a *InboundAdapter) store(ctx context.Context, st *store.Store, inboxID string, delivery Delivery) (string, error) {
	_, messageID, err := st.InsertMessageWithThread(ctx, inboxID, "", store.Message{
		Direction: "inbound",
		Subject:   delivery.Subject,
		Text:      delivery.Body,
		CreatedAt: a.now(),
		// The local key is ours and stable; the provider's own identifier is
		// kept separately so an operator can still trace a message back to
		// Cloud and the mail provider.
		ProviderMessageID: LocalDeliveryID(a.AuthorityID, delivery),
		InternetMessageID: delivery.ProviderMessageID,
		From:              store.Participant{Email: delivery.Sender},
		To:                []store.Participant{{Email: delivery.Recipient}},
	})
	if err != nil {
		return "", err
	}
	return messageID, nil
}

var _ emailtransport.InboundAdapter = (*InboundAdapter)(nil)
