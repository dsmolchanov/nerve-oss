package emailtransport

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"neuralmail/internal/memguard"
	"neuralmail/internal/store"
)

func TestOutboxPersistsKnownAcceptanceAfterDeliveryContextExpires(t *testing.T) {
	withOutboxWorkerDatabase(t, func(ctx context.Context, db *sql.DB, st *store.Store) {
		org, inbox := uuid.NewString(), uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs(id,name) VALUES($1,'drain')`, org); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO inboxes(id,org_id,address,status) VALUES($1,$2,'drain@local.nerve.email','active')`, inbox, org); err != nil {
			t.Fatal(err)
		}
		id, err := st.EnqueueOutboxMessage(ctx, store.OutboxMessage{OrgID: org, InboxID: inbox, Provider: "capture", IdempotencyKey: "accepted", To: "recipient@example.test", From: "drain@local.nerve.email", Subject: "test", TextBody: "synthetic"})
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := st.ClaimOutboxMessages(ctx, 1, "drain", time.Now().UTC(), 5*time.Minute)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim: %v %v", claimed, err)
		}
		deliveryCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		adapter := &captureOutboundAdapter{onSend: cancel}
		registry := NewRegistry()
		_ = registry.RegisterOutbound(adapter)
		budget, err := memguard.New(64 << 20)
		if err != nil {
			t.Fatal(err)
		}
		worker := NewOutboxWorker(st, registry, "drain", budget)
		if err := worker.deliverOne(deliveryCtx, claimed[0]); err != nil {
			t.Fatalf("lost confirmed acceptance: %v", err)
		}
		var status string
		if err := db.QueryRowContext(ctx, `SELECT status FROM outbox_messages WHERE id=$1`, id).Scan(&status); err != nil || status != "sent" {
			t.Fatalf("status=%s err=%v", status, err)
		}
	})
}

func TestTenClaimedRowsShareOneDrainDeadlineWhenRequeueBlocks(t *testing.T) {
	withOutboxWorkerDatabase(t, func(ctx context.Context, db *sql.DB, st *store.Store) {
		org, inbox := uuid.NewString(), uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs(id,name) VALUES($1,'batch-drain')`, org); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO inboxes(id,org_id,address,status) VALUES($1,$2,'batch@local.nerve.email','active')`, inbox, org); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 10; i++ {
			if _, err := st.EnqueueOutboxMessage(ctx, store.OutboxMessage{OrgID: org, InboxID: inbox, Provider: "capture", IdempotencyKey: fmt.Sprintf("batch-%d", i), To: "recipient@example.test", From: "batch@local.nerve.email", Subject: "synthetic", TextBody: fmt.Sprintf("test-%d", i)}); err != nil {
				t.Fatal(err)
			}
		}
		claimed, err := st.ClaimOutboxMessages(ctx, 10, "drain-test", time.Now().UTC(), time.Minute)
		if err != nil || len(claimed) != 10 {
			t.Fatalf("claim: %d %v", len(claimed), err)
		}
		lock, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Rollback()
		if _, err := lock.ExecContext(ctx, `UPDATE outbox_messages SET last_error='hold for test' WHERE inbox_id=$1`, inbox); err != nil {
			t.Fatal(err)
		}
		stopping, cancel := context.WithCancel(ctx)
		cancel()
		budget, _ := memguard.New(64 << 20)
		worker := NewOutboxWorker(st, NewRegistry(), "drain-test", budget)
		start := time.Now()
		err = worker.deliverClaimedBatch(stopping, claimed, 150*time.Millisecond)
		if err == nil {
			t.Fatal("blocked requeue failure was swallowed")
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("per-row drain multiplied budget: %s", elapsed)
		}
		// Every unstarted claim has a reported requeue failure, and no provider is
		// needed: shutdown must never attempt a new delivery.
		if strings.Count(err.Error(), "requeue unstarted outbox") != 10 {
			t.Fatalf("incomplete requeue errors: %v", err)
		}
	})
}

// A manually fired deadline lets real store work reach the chosen provider call
// before time expires. No scheduler/DB latency determines which branch is tested.
type controlledDeadline struct {
	context.Context
	done     chan struct{}
	once     sync.Once
	deadline time.Time
}

func newControlledDeadline(parent context.Context) *controlledDeadline {
	return &controlledDeadline{Context: context.WithoutCancel(parent), done: make(chan struct{}), deadline: time.Now().Add(time.Hour)}
}
func (c *controlledDeadline) Deadline() (time.Time, bool) { return c.deadline, true }
func (c *controlledDeadline) Done() <-chan struct{}       { return c.done }
func (c *controlledDeadline) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}
func (c *controlledDeadline) expire() { c.once.Do(func() { close(c.done) }) }

type deadlineOutcomeAdapter struct {
	result       error
	calls        int
	acceptBefore int
	expire       func()
}

func (a *deadlineOutcomeAdapter) Name() string { return "capture" }
func (a *deadlineOutcomeAdapter) SendMessage(ctx context.Context, _ OutboundMessage, _ string) (string, error) {
	a.calls++
	if a.calls <= a.acceptBefore {
		return "accepted", nil
	}
	a.expire()
	<-ctx.Done()
	return "", a.result
}
func (a *deadlineOutcomeAdapter) GetDeliveryStatus(context.Context, string) (DeliveryStatus, error) {
	return DeliveryStatusUnknown, ErrNotSupported
}

func TestOutboxTimeoutPersistsEveryProviderOutcome(t *testing.T) {
	for _, tc := range []struct {
		name     string
		epoch    int64
		result   error
		status   string
		resolved bool
	}{
		{"ordinary transient", 0, context.DeadlineExceeded, "queued", false},
		{"known rejection", 1, NewTransientError(429, "rate_limited", context.DeadlineExceeded), "queued", true},
		{"permanent rejection", 1, NewPermanentError(400, "bad_request", context.DeadlineExceeded), "failed", true},
		{"autonomous ambiguous", 1, context.DeadlineExceeded, "queued", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withOutboxWorkerDatabase(t, func(ctx context.Context, db *sql.DB, st *store.Store) {
				org, inbox := seedWorkerPolicyFence(t, ctx, db, st, "timeout")
				id := insertWorkerPolicyOutbox(t, ctx, db, org, inbox, 1)
				if tc.epoch == 0 {
					if _, err := db.ExecContext(ctx, `UPDATE outbox_messages SET autonomous_policy_epoch=NULL WHERE id=$1`, id); err != nil {
						t.Fatal(err)
					}
				}
				claimed := claimWorkerPolicyOutbox(t, ctx, st, id, "timeout")
				delivery := newControlledDeadline(ctx)
				defer delivery.expire()
				adapter := &deadlineOutcomeAdapter{result: tc.result, expire: delivery.expire}
				worker := policyFenceWorker(t, st, adapter, "timeout")
				if err := worker.deliverOne(delivery, claimed); err == nil {
					t.Fatal("expected provider failure")
				}
				var status string
				var lease sql.NullString
				var resolved sql.NullTime
				if err := db.QueryRowContext(ctx, `SELECT status, locked_by, provider_resolved_at FROM outbox_messages WHERE id=$1`, id).Scan(&status, &lease, &resolved); err != nil {
					t.Fatal(err)
				}
				if adapter.calls != 1 || status != tc.status || lease.Valid || resolved.Valid != tc.resolved {
					t.Fatalf("calls=%d status=%s lease=%v resolved=%v", adapter.calls, status, lease, resolved.Valid)
				}
			})
		})
	}
}

func TestOutboxBatchTimeoutRequeuesUnstartedClaimsAndCanContinue(t *testing.T) {
	withOutboxWorkerDatabase(t, func(ctx context.Context, db *sql.DB, st *store.Store) {
		org, inbox := seedWorkerPolicyFence(t, ctx, db, st, "batch-timeout")
		for i := 0; i < 4; i++ {
			insertWorkerPolicyOutbox(t, ctx, db, org, inbox, 1)
		}
		if _, err := db.ExecContext(ctx, `UPDATE outbox_messages SET autonomous_policy_epoch=NULL WHERE inbox_id=$1`, inbox); err != nil {
			t.Fatal(err)
		}
		claimed, err := st.ClaimOutboxMessages(ctx, 4, "batch-timeout", time.Now().UTC(), time.Minute)
		if err != nil || len(claimed) != 4 {
			t.Fatalf("claim %d %v", len(claimed), err)
		}
		work := newControlledDeadline(ctx)
		defer work.expire()
		drain, cancelDrain := context.WithTimeout(ctx, time.Minute)
		defer cancelDrain()
		adapter := &deadlineOutcomeAdapter{result: context.DeadlineExceeded, acceptBefore: 2, expire: work.expire}
		worker := policyFenceWorker(t, st, adapter, "batch-timeout")
		if err := worker.deliverClaimedBatchContexts(ctx, claimed, drain, work); err != nil {
			t.Fatalf("routine timeout kills worker: %v", err)
		}
		var queued int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM outbox_messages WHERE inbox_id=$1 AND status='queued' AND locked_by IS NULL`, inbox).Scan(&queued); err != nil {
			t.Fatal(err)
		}
		if queued != 2 || adapter.calls != 3 {
			t.Fatalf("queued=%d calls=%d", queued, adapter.calls)
		}
		// The poll loop continues after a successful batch cleanup. Run it again
		// with immediate deliveries and verify no claim waits for stale recovery.
		if _, err := db.ExecContext(ctx, `UPDATE outbox_messages SET next_attempt_at=now() WHERE inbox_id=$1`, inbox); err != nil {
			t.Fatal(err)
		}
		capture := &captureOutboundAdapter{}
		worker = policyFenceWorker(t, st, capture, "after-timeout")
		if err := worker.claimAndDeliver(ctx); err != nil {
			t.Fatal(err)
		}
		if len(capture.deliveries) != 2 {
			t.Fatalf("continued deliveries=%d", len(capture.deliveries))
		}
	})
}
