package emailtransport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"time"

	"neuralmail/internal/memguard"
	"neuralmail/internal/store"
)

// SMTPAdapterFactory creates temporary SMTP outbound adapters for per-inbox configs.
// Injected from main.go to avoid circular imports with providers/smtp.
type SMTPAdapterFactory func(host string, port int, username, password string, requireSTARTTLS bool) OutboundAdapter

type OutboxWorker struct {
	Store    *store.Store
	Registry *Registry

	// DataEncryptionKey is used to decrypt per-inbox SMTP passwords.
	// If nil, custom SMTP configs are not supported.
	DataEncryptionKey []byte

	// SMTPFactory creates ephemeral SMTP adapters for per-inbox custom configs.
	// Must be set together with DataEncryptionKey.
	SMTPFactory SMTPAdapterFactory

	// Listener enables LISTEN/NOTIFY-driven wake instead of fixed-interval polling.
	// When set, the worker sleeps until Postgres sends a notification on "outbox_new",
	// with PollInterval as a fallback safety-net poll.
	Listener *store.OutboxListener

	// Metrics is an optional sink for delivery counters and histograms.
	// When set, the worker emits outbox_deliver_total{provider,outcome},
	// outbox_delivery_duration_seconds{provider}, outbox_dlq_total
	// {provider,reason}, and refreshes the outbox_queue_depth gauge.
	// All operations no-op when nil so tests don't need to wire it.
	Metrics MetricsSink
	// MemoryBudget is shared with the process's other attachment consumers.
	// It is acquired from metadata before blob.content is selected and held
	// until the provider call completes.
	MemoryBudget *memguard.Budget

	WorkerID       string
	ClaimLimit     int
	PollInterval   time.Duration
	BaseBackoff    time.Duration
	MaxBackoff     time.Duration
	StaleLockAfter time.Duration
}

const providerReplayExpirySafety = time.Minute

// MetricsSink is the dependency-injected interface the outbox worker uses
// to emit metrics. Defining it locally avoids a hard dep on the
// observability package and keeps tests cheap to write.
type MetricsSink interface {
	IncDeliver(provider, outcome string)
	ObserveDeliveryDuration(provider string, seconds float64)
	IncDLQ(provider, reason string)
	SetQueueDepth(state string, count float64)
}

func NewOutboxWorker(st *store.Store, reg *Registry, workerID string, memoryBudget *memguard.Budget) *OutboxWorker {
	if workerID == "" {
		workerID = "outbox-worker"
	}
	return &OutboxWorker{
		Store:          st,
		Registry:       reg,
		MemoryBudget:   memoryBudget,
		WorkerID:       workerID,
		ClaimLimit:     10,
		PollInterval:   500 * time.Millisecond,
		BaseBackoff:    5 * time.Second,
		MaxBackoff:     15 * time.Minute,
		StaleLockAfter: 5 * time.Minute,
	}
}

func (w *OutboxWorker) Run(ctx context.Context) error {
	if w == nil || w.Store == nil {
		return errors.New("missing store")
	}
	if w.Registry == nil {
		return errors.New("missing transport registry")
	}
	if w.MemoryBudget == nil {
		return errors.New("missing memory budget")
	}
	if w.ClaimLimit <= 0 {
		w.ClaimLimit = 10
	}
	if w.BaseBackoff <= 0 {
		w.BaseBackoff = 5 * time.Second
	}
	if w.MaxBackoff <= 0 {
		w.MaxBackoff = 15 * time.Minute
	}
	if w.StaleLockAfter <= 0 {
		w.StaleLockAfter = 5 * time.Minute
	}

	if w.Listener != nil {
		if w.PollInterval <= 0 {
			w.PollInterval = 30 * time.Second
		}
		return w.runWithNotify(ctx)
	}

	if w.PollInterval <= 0 {
		w.PollInterval = 500 * time.Millisecond
	}
	return w.runWithPoll(ctx)
}

// runWithPoll is the original fixed-interval polling loop.
func (w *OutboxWorker) runWithPoll(ctx context.Context) error {
	ticker := time.NewTicker(w.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			w.claimAndDeliver(ctx)
		}
	}
}

// runWithNotify uses LISTEN/NOTIFY to wake on new messages, with PollInterval
// as a fallback safety-net in case a notification is missed.
func (w *OutboxWorker) runWithNotify(ctx context.Context) error {
	for {
		w.claimAndDeliver(ctx)

		listenCtx, cancel := context.WithTimeout(ctx, w.PollInterval)
		_, err := w.Listener.Listen(listenCtx)
		cancel()

		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			jitter := time.Duration(rand.Int63n(int64(2 * time.Second)))
			backoff := w.BaseBackoff + jitter
			slog.ErrorContext(ctx, "outbox listen error",
				slog.String("worker_id", w.WorkerID),
				slog.String("error", err.Error()),
				slog.Duration("retry_in", backoff),
			)
			time.Sleep(backoff)
		}
	}
}

func (w *OutboxWorker) claimAndDeliver(ctx context.Context) {
	w.refreshQueueDepth(ctx)
	now := time.Now().UTC()
	msgs, err := w.Store.ClaimOutboxMessages(ctx, w.ClaimLimit, w.WorkerID, now, w.StaleLockAfter)
	if err != nil {
		slog.ErrorContext(ctx, "outbox claim error",
			slog.String("worker_id", w.WorkerID),
			slog.String("error", err.Error()),
		)
		return
	}
	for _, msg := range msgs {
		if err := w.deliverOne(ctx, msg); err != nil {
			slog.ErrorContext(ctx, "outbox deliver error",
				slog.String("worker_id", w.WorkerID),
				slog.String("outbox_id", msg.ID),
				slog.String("org_id", msg.OrgID),
				slog.String("provider", msg.Provider),
				slog.Int("attempt", msg.AttemptCount),
				slog.String("error", err.Error()),
			)
		}
	}
}

func (w *OutboxWorker) deliverOne(ctx context.Context, msg store.OutboxMessage) error {
	if !msg.LockedBy.Valid || msg.LockedBy.String == "" {
		return errors.New("outbox message is missing claim lease")
	}
	claimLeaseID := msg.LockedBy.String
	adapter, ok := w.Registry.Outbound(msg.Provider)
	if !ok {
		next := time.Now().UTC().Add(30 * time.Second)
		_ = w.Store.RequeueClaimedOutboxMessage(ctx, msg.ID, claimLeaseID, next, fmt.Sprintf("unknown provider: %s", msg.Provider))
		return fmt.Errorf("unknown provider: %s", msg.Provider)
	}

	// Per-inbox SMTP config: if provider is "smtp" and inbox has a config ref, use custom SMTP.
	if msg.Provider == "smtp" && w.DataEncryptionKey != nil && w.SMTPFactory != nil {
		customAdapter, err := w.resolvePerInboxSMTP(ctx, msg.InboxID)
		if err != nil {
			slog.WarnContext(ctx, "outbox: failed to resolve per-inbox SMTP, falling back to global",
				slog.String("worker_id", w.WorkerID),
				slog.String("inbox_id", msg.InboxID),
				slog.String("error", err.Error()),
			)
		} else if customAdapter != nil {
			adapter = customAdapter
		}
	}

	if msg.AutonomousPolicyEpoch > 0 && msg.ProviderStartedAt.Valid && !msg.ProviderResolvedAt.Valid {
		// A stale claim proves a prior worker may already have completed the
		// provider call. Replay is safe only inside the adapter's bounded
		// idempotency window. Quarantine before loading attachments or touching
		// the provider when that guarantee is absent or expired.
		operationID := msg.ProviderOperationID.String
		if !msg.ProviderOperationID.Valid || operationID == "" {
			return errors.New("unresolved provider start is missing operation identity")
		}
		if replayDeadline, ok := adapterReplayDeadline(adapter, msg.ProviderStartedAt.Time); !ok || !time.Now().UTC().Before(replayDeadline) {
			reason := "provider_unknown_non_idempotent: unresolved provider operation cannot be replayed"
			if ok {
				reason = "provider_unknown_replay_window_expired: unresolved provider operation cannot be replayed safely"
			}
			w.incDeliver(msg.Provider, "provider_unknown_quarantined")
			if err := w.Store.QuarantineClaimedOutboxUnknown(ctx, msg.ID, claimLeaseID, operationID, reason); err != nil {
				return fmt.Errorf("quarantine unreplayable provider operation: %w", err)
			}
			return errors.New("unresolved provider operation quarantined without replay")
		}
	}

	textBody := msg.TextBody
	htmlBody := msg.HTMLBody

	// Auto-detect HTML mistakenly placed in the text body field.
	if htmlBody == "" && textBody != "" && looksLikeHTML(textBody) {
		htmlBody = textBody
		textBody = ""
	}

	// Reserve from metadata before selecting blob.content. Hold the reservation
	// through the provider call because adapters may base64-encode or otherwise
	// copy the content while sending. Released bytes are a permanent failure;
	// transient metadata/store/budget errors put the row back on the queue.
	attachmentBytes, err := w.Store.OutboxMessageAttachmentBytes(ctx, msg.OrgID, msg.ID)
	if err != nil {
		if errors.Is(err, store.ErrAttachmentsReleased) {
			w.incDeliver(msg.Provider, "permanent")
			w.incDLQ(msg.Provider, "attachments_released")
			if finishErr := w.Store.MarkClaimedOutboxMessageFailed(ctx, msg.ID, claimLeaseID, err.Error()); finishErr != nil {
				return fmt.Errorf("record released attachment failure: %w", finishErr)
			}
			return err
		}
		next := time.Now().UTC().Add(w.BaseBackoff)
		_ = w.Store.RequeueClaimedOutboxMessage(ctx, msg.ID, claimLeaseID, next, fmt.Sprintf("load attachment metadata: %v", err))
		return fmt.Errorf("load outbox attachment metadata: %w", err)
	}
	if attachmentBytes > w.MemoryBudget.Limit() {
		err := fmt.Errorf("attachment bytes exceed configured memory budget: requested=%d limit=%d", attachmentBytes, w.MemoryBudget.Limit())
		w.incDeliver(msg.Provider, "permanent")
		w.incDLQ(msg.Provider, "attachment_memory_limit")
		if finishErr := w.Store.MarkClaimedOutboxMessageFailed(ctx, msg.ID, claimLeaseID, err.Error()); finishErr != nil {
			return fmt.Errorf("record attachment memory failure: %w", finishErr)
		}
		return err
	}
	releaseMemory, err := w.MemoryBudget.Acquire(ctx, attachmentBytes)
	if err != nil {
		next := time.Now().UTC().Add(w.BaseBackoff)
		_ = w.Store.RequeueClaimedOutboxMessage(ctx, msg.ID, claimLeaseID, next, fmt.Sprintf("reserve attachment memory: %v", err))
		return fmt.Errorf("reserve outbox attachment memory: %w", err)
	}
	defer releaseMemory()

	attachments, err := w.Store.LoadOutboxMessageAttachments(ctx, msg.OrgID, msg.ID)
	if err != nil {
		if errors.Is(err, store.ErrAttachmentsReleased) {
			w.incDeliver(msg.Provider, "permanent")
			w.incDLQ(msg.Provider, "attachments_released")
			if finishErr := w.Store.MarkClaimedOutboxMessageFailed(ctx, msg.ID, claimLeaseID, err.Error()); finishErr != nil {
				return fmt.Errorf("record released attachment failure: %w", finishErr)
			}
			return err
		}
		next := time.Now().UTC().Add(w.BaseBackoff)
		_ = w.Store.RequeueClaimedOutboxMessage(ctx, msg.ID, claimLeaseID, next, fmt.Sprintf("load attachments: %v", err))
		return fmt.Errorf("load outbox attachments: %w", err)
	}

	out := OutboundMessage{
		From:        msg.From,
		To:          []string{msg.To},
		Subject:     msg.Subject,
		TextBody:    textBody,
		HTMLBody:    htmlBody,
		Attachments: attachments,
		Tags: map[string]string{
			"org_id":    msg.OrgID,
			"inbox_id":  msg.InboxID,
			"outbox_id": msg.ID,
		},
	}

	// Set In-Reply-To and References headers for reply-chain threading.
	if msg.InReplyToMessageID != "" {
		out.Headers = map[string]string{
			"In-Reply-To": msg.InReplyToMessageID,
		}
		if msg.References != "" {
			out.Headers["References"] = msg.References + " " + msg.InReplyToMessageID
		} else {
			out.Headers["References"] = msg.InReplyToMessageID
		}
	}

	// For forwarded messages (idempotency key starts with "fwd:"), add forwarding headers.
	if strings.HasPrefix(msg.IdempotencyKey, "fwd:") {
		if out.Headers == nil {
			out.Headers = make(map[string]string)
		}
		// X-Nerve-Loop prevents infinite forwarding loops
		out.Headers["X-Nerve-Loop"] = msg.InboxID
		// Reply-To lets the external recipient reply directly to the original sender
		// (the original sender info isn't stored separately, but the outbox "to" field
		// is the forward target; the inbox address is the "from")
	}

	operation, err := w.Store.BeginOutboxProviderOperationState(ctx, msg)
	if err != nil {
		if errors.Is(err, store.ErrOutboxPolicyRevoked) {
			w.incDeliver(msg.Provider, "policy_revoked")
			return err
		}
		if errors.Is(err, store.ErrOutboxClaimLost) {
			return err
		}
		next := time.Now().UTC().Add(w.BaseBackoff)
		_ = w.Store.RequeueClaimedOutboxMessage(ctx, msg.ID, claimLeaseID, next, fmt.Sprintf("provider-start fence: %v", err))
		return fmt.Errorf("begin outbox provider operation: %w", err)
	}
	operationID := operation.ID
	providerStartedAt := operation.StartedAt
	providerOperationKey := msg.IdempotencyKey
	if operationID != "" {
		providerOperationKey = operationID
	}

	sendStart := time.Now()
	providerMessageID, err := adapter.SendMessage(ctx, out, providerOperationKey)
	w.observeDelivery(msg.Provider, time.Since(sendStart).Seconds())
	if err == nil {
		w.incDeliver(msg.Provider, "ok")
		return w.Store.MarkClaimedOutboxMessageSent(ctx, msg.ID, claimLeaseID, operationID, providerMessageID)
	}

	// Classify the provider error. Permanent errors terminate the message
	// immediately without consuming retry budget — an invalid recipient or
	// bad authentication will not resolve on retry.
	classified := ClassifyProviderError(err)
	if classified != nil && classified.Permanent {
		slog.WarnContext(ctx, "outbox: permanent provider error, terminating",
			slog.String("worker_id", w.WorkerID),
			slog.String("outbox_id", msg.ID),
			slog.String("org_id", msg.OrgID),
			slog.String("provider", msg.Provider),
			slog.String("reason", classified.Reason),
			slog.Int("status_code", classified.StatusCode),
			slog.Int("attempt", msg.AttemptCount),
			slog.String("error", err.Error()),
		)
		w.incDeliver(msg.Provider, "permanent")
		w.incDLQ(msg.Provider, classified.Reason)
		if finishErr := w.Store.MarkClaimedOutboxProviderFailure(ctx, msg.ID, claimLeaseID, operationID, fmt.Sprintf("permanent:%s: %s", classified.Reason, err.Error())); finishErr != nil {
			return fmt.Errorf("record permanent provider failure: %w", finishErr)
		}
		return err
	}

	// A provider-confirmed 4xx rejection proves this attempt was not accepted.
	// A 5xx response remains ambiguous: the provider may have accepted the
	// logical operation before failing the request, so only idempotent
	// replay/readback may resolve it. Transport errors are ambiguous too.
	knownOutcome := classified != nil && classified.StatusCode >= 400 && classified.StatusCode < 500
	if msg.AutonomousPolicyEpoch > 0 && !knownOutcome && !adapterSupportsIdempotentReplay(adapter) {
		// Replaying an ambiguous operation through SMTP or another adapter that
		// cannot honor the stable operation identity can duplicate a delivery.
		// Quarantine immediately; the unresolved fence keeps lifecycle cleanup
		// blocked until an operator/provider reconciliation establishes outcome.
		w.incDeliver(msg.Provider, "provider_unknown_quarantined")
		if quarantineErr := w.Store.QuarantineClaimedOutboxUnknown(ctx, msg.ID, claimLeaseID, operationID, "provider_unknown_non_idempotent: "+err.Error()); quarantineErr != nil {
			return fmt.Errorf("quarantine ambiguous provider outcome: %w", quarantineErr)
		}
		return err
	}

	if msg.AttemptCount >= store.MaxOutboxRetries {
		if msg.AutonomousPolicyEpoch > 0 && !knownOutcome && adapterSupportsIdempotentReplay(adapter) {
			// Unknown autonomous outcomes cannot be declared failed: retrying the
			// same provider idempotency identity is the recovery/readback path.
			next := time.Now().UTC().Add(w.MaxBackoff)
			if replayDeadline, ok := adapterReplayDeadline(adapter, providerStartedAt); !ok || !next.Before(replayDeadline) {
				w.incDeliver(msg.Provider, "provider_unknown_quarantined")
				if quarantineErr := w.Store.QuarantineClaimedOutboxUnknown(ctx, msg.ID, claimLeaseID, operationID, "provider_unknown_replay_window_expired: retry would exceed safe replay window"); quarantineErr != nil {
					return fmt.Errorf("quarantine expiring provider operation: %w", quarantineErr)
				}
				return err
			}
			w.incDeliver(msg.Provider, "provider_unknown")
			_ = w.Store.RequeueClaimedOutboxMessage(ctx, msg.ID, claimLeaseID, next, err.Error())
			return err
		}
		slog.WarnContext(ctx, "outbox: retry budget exhausted, terminating",
			slog.String("worker_id", w.WorkerID),
			slog.String("outbox_id", msg.ID),
			slog.String("org_id", msg.OrgID),
			slog.String("provider", msg.Provider),
			slog.Int("attempt", msg.AttemptCount),
			slog.String("error", err.Error()),
		)
		w.incDeliver(msg.Provider, "exhausted")
		reason := "exhausted"
		if classified != nil && classified.Reason != "" {
			reason = classified.Reason
		}
		w.incDLQ(msg.Provider, reason)
		if finishErr := w.Store.MarkClaimedOutboxProviderFailure(ctx, msg.ID, claimLeaseID, operationID, err.Error()); finishErr != nil {
			return fmt.Errorf("record exhausted provider failure: %w", finishErr)
		}
		return err
	}

	w.incDeliver(msg.Provider, "transient")
	backoff := w.backoffForAttempt(msg.AttemptCount)
	next := time.Now().UTC().Add(backoff)
	if knownOutcome && operationID != "" {
		if requeueErr := w.Store.RequeueClaimedOutboxKnownProviderFailure(ctx, msg.ID, claimLeaseID, operationID, next, err.Error()); requeueErr != nil {
			return fmt.Errorf("resolve and requeue provider failure: %w", requeueErr)
		}
		return err
	}
	_ = w.Store.RequeueClaimedOutboxMessage(ctx, msg.ID, claimLeaseID, next, err.Error())
	return err
}

func adapterSupportsIdempotentReplay(adapter OutboundAdapter) bool {
	capability, ok := adapter.(IdempotentReplayAdapter)
	return ok && capability.SupportsIdempotentReplay() && capability.IdempotentReplayWindow() > providerReplayExpirySafety
}

func adapterReplayDeadline(adapter OutboundAdapter, startedAt time.Time) (time.Time, bool) {
	capability, ok := adapter.(IdempotentReplayAdapter)
	if !ok || !capability.SupportsIdempotentReplay() || startedAt.IsZero() {
		return time.Time{}, false
	}
	window := capability.IdempotentReplayWindow()
	if window <= providerReplayExpirySafety {
		return time.Time{}, false
	}
	return startedAt.Add(window - providerReplayExpirySafety), true
}

// Metrics helpers — all no-op when w.Metrics is nil so the worker can
// run in tests without wiring an exporter.

func (w *OutboxWorker) incDeliver(provider, outcome string) {
	if w == nil || w.Metrics == nil {
		return
	}
	w.Metrics.IncDeliver(provider, outcome)
}

func (w *OutboxWorker) observeDelivery(provider string, seconds float64) {
	if w == nil || w.Metrics == nil {
		return
	}
	w.Metrics.ObserveDeliveryDuration(provider, seconds)
}

func (w *OutboxWorker) incDLQ(provider, reason string) {
	if w == nil || w.Metrics == nil {
		return
	}
	w.Metrics.IncDLQ(provider, reason)
}

// refreshQueueDepth samples the current outbox state distribution and
// updates the queue-depth gauge. Called once per claim cycle by the
// background loop. Errors are silenced because metrics must never
// affect delivery correctness.
func (w *OutboxWorker) refreshQueueDepth(ctx context.Context) {
	if w == nil || w.Metrics == nil || w.Store == nil {
		return
	}
	for _, state := range []string{"queued", "sending", "sent", "failed"} {
		if n, err := w.Store.CountOutboxByState(ctx, state); err == nil {
			w.Metrics.SetQueueDepth(state, float64(n))
		}
	}
}

// resolvePerInboxSMTP checks if the inbox has a custom SMTP config and returns
// a temporary OutboundAdapter for it. Returns nil if no custom config is set.
func (w *OutboxWorker) resolvePerInboxSMTP(ctx context.Context, inboxID string) (OutboundAdapter, error) {
	inbox, err := w.Store.GetInboxRecordByID(ctx, inboxID)
	if err != nil {
		return nil, err
	}

	if !inbox.OutboundProviderConfigRef.Valid || inbox.OutboundProviderConfigRef.String == "" {
		return nil, nil // no custom config, use global
	}

	smtpCfg, err := w.Store.GetInboxSMTPConfigByRef(ctx, inbox.OutboundProviderConfigRef.String)
	if err != nil {
		return nil, fmt.Errorf("load smtp config %s: %w", inbox.OutboundProviderConfigRef.String, err)
	}

	// Decrypt password
	password, err := decryptSMTPPassword(smtpCfg.PasswordEnc, w.DataEncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("decrypt smtp password: %w", err)
	}

	// Construct a temporary SMTP adapter with per-inbox credentials
	return w.SMTPFactory(smtpCfg.Host, smtpCfg.Port, smtpCfg.Username, password, smtpCfg.RequireSTARTTLS), nil
}

// looksLikeHTML returns true if body appears to be HTML rather than plain text.
// Used as a safety net to auto-promote HTML that was placed in the text_body field.
func looksLikeHTML(body string) bool {
	trimmed := strings.TrimSpace(body)
	if strings.HasPrefix(trimmed, "<!DOCTYPE") || strings.HasPrefix(trimmed, "<!doctype") {
		return true
	}
	lower := strings.ToLower(trimmed)
	// Check for common HTML block-level tags that wouldn't appear in plain text.
	for _, tag := range []string{"<html", "<div", "<table", "<head", "<body"} {
		if strings.Contains(lower, tag) {
			return true
		}
	}
	return false
}

func (w *OutboxWorker) backoffForAttempt(attempt int) time.Duration {
	if attempt <= 0 {
		return w.BaseBackoff
	}
	// attempt_count is incremented when claimed; first failure uses attempt=1.
	d := w.BaseBackoff
	shift := attempt - 1
	if shift > 16 {
		shift = 16
	}
	d = d * time.Duration(1<<shift)
	if d > w.MaxBackoff {
		return w.MaxBackoff
	}
	return d
}
