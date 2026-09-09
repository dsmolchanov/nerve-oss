package emailtransport

import (
	"context"
	"database/sql"
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
