package hybrid

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"neuralmail/internal/emailtransport"
)

// ProviderName is the registry name and the value an inbox carries in its
// inbound/outbound provider columns.
const ProviderName = "hybrid"

// ErrSendUncertain means Cloud accepted the message and then lost track of
// whether the mail provider took it. Resending would risk a duplicate on the
// recipient's side, so the operation stops here and waits for Cloud's
// reconciliation to settle the receipt.
var ErrSendUncertain = errors.New("hybrid send outcome uncertain")

// ErrSendPending means Cloud holds the message durably but has not finished
// with the provider yet. The worker retries the same operation key, which
// returns the settled result rather than sending twice.
var ErrSendPending = errors.New("hybrid send not yet delivered")

// OutboundAdapter hands outbound mail to the Cloud provider.
//
// It never sends mail itself: a self-hosted runtime has no deliverable sending
// domain, and routing through Cloud is what keeps DKIM, suppression and
// recipient policy in one place.
type OutboundAdapter struct {
	Client *Client
	// Kind is the Cloud scope this adapter sends under. Replies and new
	// conversations are separately scoped so a runtime that may answer mail
	// cannot start cold outreach.
	Kind string
}

func (a *OutboundAdapter) Name() string { return ProviderName }

// SendMessage submits one message and reports the provider's identifier.
//
// The worker's idempotency key is passed through as Cloud's operation key, so
// a retry after any failure — including one where this process never saw the
// response — returns the original outcome instead of sending a second copy.
func (a *OutboundAdapter) SendMessage(ctx context.Context, msg emailtransport.OutboundMessage, idempotencyKey string) (string, error) {
	if a.Client == nil {
		return "", errors.New("hybrid outbound adapter has no cloud client")
	}
	key := strings.TrimSpace(idempotencyKey)
	if key == "" || key != idempotencyKey || len(key) > 128 {
		return "", permanentSendRefusal("invalid_operation_key",
			errors.New("hybrid send requires a bounded operation key"))
	}
	if err := checkSendable(msg); err != nil {
		return "", err
	}
	kind := a.Kind
	if kind == "" {
		kind = "reply"
	}
	receipt, err := a.Client.Send(ctx, SendRequest{
		OperationKey: key, Kind: kind, To: msg.To, Subject: msg.Subject, Body: msg.TextBody,
	})
	if err != nil {
		return "", err
	}
	return resolveReceipt(receipt)
}

// checkSendable refuses a message the Cloud send contract cannot carry.
//
// That contract is one recipient, a subject and a plain-text body. Everything
// else an outbound message can hold — HTML, attachments, copies, reply
// headers — has nowhere to go. Dropping those silently is the dangerous
// option: Cloud would answer "sent", the outbox row would be finalized, and
// the attachment bytes released, so the loss is discovered by the recipient
// and can never be repaired. Refusing is permanent rather than retryable,
// because the same message will keep being unsendable.
func checkSendable(msg emailtransport.OutboundMessage) error {
	// Cloud accepts exactly one recipient per operation so that a partial
	// delivery can never hide inside one receipt.
	if len(msg.To) != 1 {
		return permanentSendRefusal("invalid_recipient",
			fmt.Errorf("hybrid send takes exactly one recipient, got %d", len(msg.To)))
	}
	unsupported := make([]string, 0, 6)
	if strings.TrimSpace(msg.HTMLBody) != "" {
		unsupported = append(unsupported, "an HTML body")
	}
	if len(msg.Attachments) != 0 {
		unsupported = append(unsupported, fmt.Sprintf("%d attachment(s)", len(msg.Attachments)))
	}
	if len(msg.CC) != 0 {
		unsupported = append(unsupported, "CC recipients")
	}
	if len(msg.BCC) != 0 {
		unsupported = append(unsupported, "BCC recipients")
	}
	if len(msg.ReplyTo) != 0 {
		unsupported = append(unsupported, "a Reply-To address")
	}
	if len(msg.Headers) != 0 {
		unsupported = append(unsupported, "custom headers")
	}
	if len(unsupported) != 0 {
		return permanentSendRefusal("unsupported_content",
			fmt.Errorf("hybrid transport cannot carry %s", strings.Join(unsupported, ", ")))
	}
	// A message with neither a subject nor a body would be delivered empty.
	if strings.TrimSpace(msg.Subject) == "" && strings.TrimSpace(msg.TextBody) == "" {
		return permanentSendRefusal("empty_message",
			errors.New("hybrid send needs a subject or a plain-text body"))
	}
	return nil
}

// permanentSendRefusal marks a refusal the outbox worker must not retry.
// Without the type the worker treats it as transient and burns the retry
// budget on a message that can never be sent.
func permanentSendRefusal(reason string, cause error) error {
	return emailtransport.NewPermanentError(0, reason, cause)
}

func resolveReceipt(receipt SendReceipt) (string, error) {
	switch receipt.Status {
	case "sent":
		if receipt.ProviderMessageID == "" {
			return "", emailtransport.NewTransientError(0, "server_error",
				fmt.Errorf("%w: cloud reported sent without a provider id", ErrUnavailable))
		}
		return receipt.ProviderMessageID, nil
	case "uncertain":
		// Retrying would risk a duplicate at the recipient. Wait for Cloud's
		// reconciliation instead of consuming retry budget.
		return "", emailtransport.NewTransientError(0, "outcome_unknown", ErrSendUncertain)
	case "accepted", "claimed":
		return "", emailtransport.NewTransientError(0, "provider_pending", ErrSendPending)
	default:
		return "", emailtransport.NewTransientError(0, "server_error",
			fmt.Errorf("%w: unknown send status %q", ErrUnavailable, receipt.Status))
	}
}

// GetDeliveryStatus reports what Cloud durably knows about one operation.
//
// The argument is the operation key, not the provider's message ID: the
// operation key is what this runtime chose and can still reproduce after a
// crash, whereas a provider ID only exists once the send has already settled.
func (a *OutboundAdapter) GetDeliveryStatus(ctx context.Context, operationKey string) (emailtransport.DeliveryStatus, error) {
	if a.Client == nil {
		return emailtransport.DeliveryStatusUnknown, errors.New("hybrid outbound adapter has no cloud client")
	}
	receipt, err := a.Client.Receipt(ctx, operationKey)
	if errors.Is(err, ErrMissing) {
		return emailtransport.DeliveryStatusUnknown, nil
	}
	if err != nil {
		return emailtransport.DeliveryStatusUnknown, err
	}
	switch receipt.Status {
	case "sent":
		return emailtransport.DeliveryStatusSent, nil
	case "accepted", "claimed":
		return emailtransport.DeliveryStatusQueued, nil
	default:
		// "uncertain" included: Cloud does not know, so neither does this.
		return emailtransport.DeliveryStatusUnknown, nil
	}
}

// SupportsIdempotentReplay reports that Cloud applies the supplied operation
// key to delivery, so the worker may safely replay an ambiguous send.
func (a *OutboundAdapter) SupportsIdempotentReplay() bool { return true }

// IdempotentReplayWindow is how long Cloud keeps an operation key bound to its
// first accepted payload. Cloud retains send receipts far longer than this;
// the bound here is the worker's, and staying well inside Cloud's retention
// means a replay always meets the original receipt rather than a fresh send.
func (a *OutboundAdapter) IdempotentReplayWindow() time.Duration { return 24 * time.Hour }

var (
	_ emailtransport.OutboundAdapter         = (*OutboundAdapter)(nil)
	_ emailtransport.IdempotentReplayAdapter = (*OutboundAdapter)(nil)
)
