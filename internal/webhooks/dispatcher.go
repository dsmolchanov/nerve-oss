// Package webhooks implements the customer-facing outbound webhook
// dispatcher. It mirrors the outbox delivery loop shape: claim a batch
// of pending deliveries, POST each to the customer URL with an
// HMAC-SHA256 signature, and either mark delivered or requeue with
// exponential backoff.
package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"neuralmail/internal/store"
)

// Dispatcher delivers queued webhook rows to customer endpoints.
// Run it in a goroutine alongside the outbox worker.
type Dispatcher struct {
	Store *store.Store

	// HTTPClient overrides the default HTTP client. Tests inject a
	// fake; production code leaves it nil to get a sane default.
	HTTPClient *http.Client

	WorkerID       string
	ClaimLimit     int
	PollInterval   time.Duration
	BaseBackoff    time.Duration
	MaxBackoff     time.Duration
	StaleLockAfter time.Duration
	MaxAttempts    int
}

// NewDispatcher builds a Dispatcher with sensible defaults.
func NewDispatcher(st *store.Store, workerID string) *Dispatcher {
	if workerID == "" {
		workerID = "webhook-dispatcher"
	}
	return &Dispatcher{
		Store:          st,
		WorkerID:       workerID,
		ClaimLimit:     20,
		PollInterval:   5 * time.Second,
		BaseBackoff:    10 * time.Second,
		MaxBackoff:     1 * time.Hour,
		StaleLockAfter: 5 * time.Minute,
		MaxAttempts:    store.MaxWebhookRetries,
	}
}

// Run blocks until ctx is cancelled, claiming and delivering batches
// of webhook deliveries on the configured polling interval. Uses
// polling rather than LISTEN/NOTIFY because webhook traffic is much
// lower volume than outbound email and the simpler loop is easier to
// reason about under concurrent workers.
func (d *Dispatcher) Run(ctx context.Context) error {
	if d == nil || d.Store == nil {
		return errors.New("missing store")
	}
	if d.ClaimLimit <= 0 {
		d.ClaimLimit = 20
	}
	if d.PollInterval <= 0 {
		d.PollInterval = 5 * time.Second
	}
	if d.BaseBackoff <= 0 {
		d.BaseBackoff = 10 * time.Second
	}
	if d.MaxBackoff <= 0 {
		d.MaxBackoff = 1 * time.Hour
	}
	if d.StaleLockAfter <= 0 {
		d.StaleLockAfter = 5 * time.Minute
	}
	if d.MaxAttempts <= 0 {
		d.MaxAttempts = store.MaxWebhookRetries
	}
	if d.HTTPClient == nil {
		d.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}

	ticker := time.NewTicker(d.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			d.claimAndDeliver(ctx)
		}
	}
}

func (d *Dispatcher) claimAndDeliver(ctx context.Context) {
	now := time.Now().UTC()
	deliveries, err := d.Store.ClaimWebhookDeliveries(ctx, d.ClaimLimit, d.WorkerID, now, d.StaleLockAfter)
	if err != nil {
		slog.ErrorContext(ctx, "webhook dispatcher: claim error",
			slog.String("worker_id", d.WorkerID),
			slog.String("error", err.Error()),
		)
		return
	}
	for _, delivery := range deliveries {
		if err := d.deliverOne(ctx, delivery); err != nil {
			slog.ErrorContext(ctx, "webhook dispatcher: deliver error",
				slog.String("worker_id", d.WorkerID),
				slog.String("delivery_id", delivery.ID),
				slog.String("webhook_id", delivery.WebhookID),
				slog.String("event_type", delivery.EventType),
				slog.Int("attempt", delivery.AttemptCount),
				slog.String("error", err.Error()),
			)
		}
	}
}

func (d *Dispatcher) deliverOne(ctx context.Context, delivery store.WebhookDelivery) error {
	url, secret, err := d.Store.LookupWebhookSubscription(ctx, delivery.WebhookID)
	if err != nil {
		// Subscription was deleted or disabled between claim and
		// dispatch. Drop the delivery row — nothing to do.
		return d.Store.MarkWebhookDeliveryFailed(ctx, delivery.ID, 0, "subscription removed or disabled")
	}

	statusCode, err := d.postSigned(ctx, url, secret, delivery.Payload)
	if err == nil && statusCode >= 200 && statusCode < 300 {
		return d.Store.MarkWebhookDeliveryDelivered(ctx, delivery.ID, statusCode)
	}

	lastErr := ""
	if err != nil {
		lastErr = err.Error()
	} else {
		lastErr = fmt.Sprintf("non-2xx response: %d", statusCode)
	}

	// Permanent: 4xx responses (except 408/429) mean the customer has
	// rejected the payload shape or auth; retrying won't help.
	permanent := statusCode >= 400 && statusCode < 500 && statusCode != http.StatusRequestTimeout && statusCode != http.StatusTooManyRequests
	if permanent {
		slog.WarnContext(ctx, "webhook dispatcher: permanent 4xx, terminating",
			slog.String("delivery_id", delivery.ID),
			slog.String("webhook_id", delivery.WebhookID),
			slog.Int("status_code", statusCode),
			slog.String("error", lastErr),
		)
		return d.Store.MarkWebhookDeliveryFailed(ctx, delivery.ID, statusCode, lastErr)
	}

	if delivery.AttemptCount >= d.MaxAttempts {
		slog.WarnContext(ctx, "webhook dispatcher: retry budget exhausted",
			slog.String("delivery_id", delivery.ID),
			slog.String("webhook_id", delivery.WebhookID),
			slog.Int("attempt", delivery.AttemptCount),
			slog.String("error", lastErr),
		)
		return d.Store.MarkWebhookDeliveryFailed(ctx, delivery.ID, statusCode, lastErr)
	}

	backoff := d.backoffForAttempt(delivery.AttemptCount)
	next := time.Now().UTC().Add(backoff)
	return d.Store.RequeueWebhookDelivery(ctx, delivery.ID, next, statusCode, lastErr)
}

// postSigned POSTs the payload to the customer URL with X-Nerve-Signature
// and X-Nerve-Timestamp headers. Returns the HTTP status code and any
// transport error. The signature format is:
//
//	X-Nerve-Signature: t=<unix-ts>,v1=<hex-hmac>
//	X-Nerve-Timestamp: <unix-ts>
//
// The HMAC is computed over `<ts>.<body>` keyed by the webhook secret.
// Customers verify by rebuilding the string and comparing with
// crypto.subtle.timingSafeEqual.
func (d *Dispatcher) postSigned(ctx context.Context, url, secret string, payload []byte) (int, error) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	signature := SignPayload(secret, ts, payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Nerve-Timestamp", ts)
	req.Header.Set("X-Nerve-Signature", "t="+ts+",v1="+signature)
	req.Header.Set("User-Agent", "nerve-webhook-dispatcher/1")

	resp, err := d.HTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	// Drain the body so the connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// backoffForAttempt returns an exponential backoff with a hard ceiling.
// Matches the outbox_worker behaviour.
func (d *Dispatcher) backoffForAttempt(attempt int) time.Duration {
	if attempt <= 0 {
		return d.BaseBackoff
	}
	shift := attempt - 1
	if shift > 16 {
		shift = 16
	}
	delay := d.BaseBackoff * time.Duration(1<<shift)
	if delay > d.MaxBackoff {
		return d.MaxBackoff
	}
	return delay
}

// SignPayload computes the HMAC-SHA256 signature for a webhook delivery.
// Exported so tests and customer verification reference docs can share
// the exact algorithm.
func SignPayload(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
