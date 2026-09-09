package emailtransport

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
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
