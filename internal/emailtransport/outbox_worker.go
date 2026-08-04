package emailtransport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"time"

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

	WorkerID       string
	ClaimLimit     int
	PollInterval   time.Duration
	BaseBackoff    time.Duration
	MaxBackoff     time.Duration
	StaleLockAfter time.Duration
}

// MetricsSink is the dependency-injected interface the outbox worker uses
// to emit metrics. Defining it locally avoids a hard dep on the
// observability package and keeps tests cheap to write.
type MetricsSink interface {
	IncDeliver(provider, outcome string)
	ObserveDeliveryDuration(provider string, seconds float64)
	IncDLQ(provider, reason string)
	SetQueueDepth(state string, count float64)
}

func NewOutboxWorker(st *store.Store, reg *Registry, workerID string) *OutboxWorker {
	if workerID == "" {
		workerID = "outbox-worker"
	}
	return &OutboxWorker{
		Store:          st,
		Registry:       reg,
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
	adapter, ok := w.Registry.Outbound(msg.Provider)
	if !ok {
		next := time.Now().UTC().Add(30 * time.Second)
		_ = w.Store.RequeueOutboxMessage(ctx, msg.ID, next, fmt.Sprintf("unknown provider: %s", msg.Provider))
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

	textBody := msg.TextBody
	htmlBody := msg.HTMLBody

	// Auto-detect HTML mistakenly placed in the text body field.
	if htmlBody == "" && textBody != "" && looksLikeHTML(textBody) {
		htmlBody = textBody
		textBody = ""
	}

	out := OutboundMessage{
		From:     msg.From,
		To:       []string{msg.To},
		Subject:  msg.Subject,
		TextBody: textBody,
		HTMLBody: htmlBody,
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

	sendStart := time.Now()
	providerMessageID, err := adapter.SendMessage(ctx, out, msg.IdempotencyKey)
	w.observeDelivery(msg.Provider, time.Since(sendStart).Seconds())
	if err == nil {
		w.incDeliver(msg.Provider, "ok")
		return w.Store.MarkOutboxMessageSent(ctx, msg.ID, providerMessageID)
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
		_ = w.Store.MarkOutboxMessageFailed(ctx, msg.ID, fmt.Sprintf("permanent:%s: %s", classified.Reason, err.Error()))
		return err
	}

	if msg.AttemptCount >= store.MaxOutboxRetries {
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
		_ = w.Store.MarkOutboxMessageFailed(ctx, msg.ID, err.Error())
		return err
	}

	w.incDeliver(msg.Provider, "transient")
	backoff := w.backoffForAttempt(msg.AttemptCount)
	next := time.Now().UTC().Add(backoff)
	_ = w.Store.RequeueOutboxMessage(ctx, msg.ID, next, err.Error())
	return err
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
