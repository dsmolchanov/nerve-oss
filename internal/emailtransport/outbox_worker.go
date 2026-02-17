package emailtransport

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"neuralmail/internal/store"
)

type OutboxWorker struct {
	Store    *store.Store
	Registry *Registry

	WorkerID       string
	ClaimLimit     int
	PollInterval   time.Duration
	BaseBackoff    time.Duration
	MaxBackoff     time.Duration
	StaleLockAfter time.Duration
}

func NewOutboxWorker(st *store.Store, reg *Registry, workerID string) *OutboxWorker {
	if workerID == "" {
		workerID = "outbox-worker"
	}
	return &OutboxWorker{
		Store:        st,
		Registry:     reg,
		WorkerID:     workerID,
		ClaimLimit:   10,
		PollInterval: 500 * time.Millisecond,
		BaseBackoff:  5 * time.Second,
		MaxBackoff:   15 * time.Minute,
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
	if w.PollInterval <= 0 {
		w.PollInterval = 500 * time.Millisecond
	}
	if w.BaseBackoff <= 0 {
		w.BaseBackoff = 5 * time.Second
	}
	if w.MaxBackoff <= 0 {
		w.MaxBackoff = 15 * time.Minute
	}

	ticker := time.NewTicker(w.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			now := time.Now().UTC()
			msgs, err := w.Store.ClaimOutboxMessages(ctx, w.ClaimLimit, w.WorkerID, now)
			if err != nil {
				log.Printf("outbox claim error: %v", err)
				continue
			}
			for _, msg := range msgs {
				if err := w.deliverOne(ctx, msg); err != nil {
					log.Printf("outbox deliver error id=%s provider=%s attempt=%d err=%v", msg.ID, msg.Provider, msg.AttemptCount, err)
				}
			}
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

	providerMessageID, err := adapter.SendMessage(ctx, OutboundMessage{
		From:     msg.From,
		To:       []string{msg.To},
		Subject:  msg.Subject,
		TextBody: msg.TextBody,
		HTMLBody: msg.HTMLBody,
	}, msg.IdempotencyKey)
	if err == nil {
		return w.Store.MarkOutboxMessageSent(ctx, msg.ID, providerMessageID)
	}

	backoff := w.backoffForAttempt(msg.AttemptCount)
	next := time.Now().UTC().Add(backoff)
	_ = w.Store.RequeueOutboxMessage(ctx, msg.ID, next, err.Error())
	return err
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
