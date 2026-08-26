package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
)

func seedCore28Outbox(t *testing.T, ctx context.Context, db *sql.DB) (string, string) {
	t.Helper()
	orgID := uuid.NewString()
	inboxID := uuid.NewString()
	if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'core28-org')`, orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO inboxes (id, org_id, address, status)
		VALUES ($1, $2, 'core28@local.neuralmail', 'active')
	`, inboxID, orgID); err != nil {
		t.Fatalf("insert inbox: %v", err)
	}
	return orgID, inboxID
}

// Artifact B spans Core [28,29] and must serve legacy outbound on Core 28,
// before the additive Core 0029 fence exists. Every statement on that path has
// to avoid the fence columns entirely: enqueue, claim, and terminal delivery.
func TestLegacyOutboxWorksOnCore28WithoutFenceColumns(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if err := MigrateUpToCore(ctx, db, 28); err != nil {
			t.Fatalf("migrate to core 28: %v", err)
		}
		st := &Store{db: db, q: db, fence: newEnabledFence()}
		if err := st.RefreshOutboundFenceCapability(ctx); err != nil {
			t.Fatalf("detect fence capability: %v", err)
		}
		if st.OutboundFenceEnabled() {
			t.Fatal("core 28 reported the 0029 fence as available")
		}

		orgID, inboxID := seedCore28Outbox(t, ctx, db)
		id, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID: orgID, InboxID: inboxID, Provider: "smtp", IdempotencyKey: "core28-legacy",
			To: "recipient@local.neuralmail", From: "core28@local.neuralmail",
			Subject: "core28", TextBody: "core28 body",
		})
		if err != nil {
			t.Fatalf("enqueue on core 28: %v", err)
		}

		claimed, err := st.ClaimOutboxMessages(ctx, 10, "core28-worker", time.Now().UTC(), 5*time.Minute)
		if err != nil {
			t.Fatalf("claim on core 28: %v", err)
		}
		if len(claimed) != 1 || claimed[0].ID != id {
			t.Fatalf("claim returned %+v, want exactly the enqueued row", claimed)
		}
		if claimed[0].AutonomousPolicyEpoch != 0 {
			t.Fatalf("pre-fence claim reported epoch %d", claimed[0].AutonomousPolicyEpoch)
		}

		if err := st.MarkOutboxMessageSent(ctx, id, "provider-core28"); err != nil {
			t.Fatalf("mark sent on core 28: %v", err)
		}
		var status string
		if err := db.QueryRowContext(ctx, `SELECT status FROM outbox_messages WHERE id = $1`, id).Scan(&status); err != nil {
			t.Fatalf("read status: %v", err)
		}
		if status != "sent" {
			t.Fatalf("status = %q, want sent", status)
		}

		// A second enqueue must requeue and fail cleanly too, exercising the
		// remaining pre-fence statements.
		retryID, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID: orgID, InboxID: inboxID, Provider: "smtp", IdempotencyKey: "core28-retry",
			To: "recipient@local.neuralmail", From: "core28@local.neuralmail",
			Subject: "core28 retry", TextBody: "retry body",
		})
		if err != nil {
			t.Fatalf("enqueue retry row: %v", err)
		}
		if _, err := st.ClaimOutboxMessages(ctx, 10, "core28-worker", time.Now().UTC(), 5*time.Minute); err != nil {
			t.Fatalf("claim retry row: %v", err)
		}
		if err := st.RequeueOutboxMessage(ctx, retryID, time.Now().UTC().Add(time.Minute), "transient"); err != nil {
			t.Fatalf("requeue on core 28: %v", err)
		}
		if err := st.MarkOutboxProviderFailure(ctx, retryID, "permanent"); err != nil {
			t.Fatalf("mark failure on core 28: %v", err)
		}
	})
}

// The autonomous provider fence cannot be reached before Core 0029. Those
// operations must refuse with a typed unsupported-schema error rather than
// emitting SQL against columns that do not exist.
func TestAutonomousFenceOperationsRefuseOnCore28(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if err := MigrateUpToCore(ctx, db, 28); err != nil {
			t.Fatalf("migrate to core 28: %v", err)
		}
		st := &Store{db: db, q: db, fence: newEnabledFence()}
		if err := st.RefreshOutboundFenceCapability(ctx); err != nil {
			t.Fatalf("detect fence capability: %v", err)
		}
		orgID, _ := seedCore28Outbox(t, ctx, db)

		if _, err := st.BeginOutboxProviderOperationState(ctx, OutboxMessage{ID: uuid.NewString()}); err == nil {
			t.Fatal("provider-start admitted on core 28")
		}
		if err := st.ResolveOutboxProviderAttempt(ctx, uuid.NewString(), "w", "op"); err == nil {
			t.Fatal("provider resolution admitted on core 28")
		}
		if err := st.RunInTx(ctx, func(scoped *Store) error {
			_, err := scoped.EnsureOutboundPolicyState(ctx, orgID)
			return err
		}); err == nil {
			t.Fatal("policy-state write admitted on core 28")
		}
	})
}

// After Core 0029 the same Store must re-detect the fence and enforce it.
func TestFenceBecomesActiveAfterCore29(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if err := MigrateUpToCore(ctx, db, 28); err != nil {
			t.Fatalf("migrate to core 28: %v", err)
		}
		st := &Store{db: db, q: db, fence: newEnabledFence()}
		if err := st.RefreshOutboundFenceCapability(ctx); err != nil {
			t.Fatalf("detect fence at 28: %v", err)
		}
		if st.OutboundFenceEnabled() {
			t.Fatal("fence reported available on core 28")
		}
		if err := MigrateUpToCore(ctx, db, 29); err != nil {
			t.Fatalf("migrate to core 29: %v", err)
		}
		if err := st.RefreshOutboundFenceCapability(ctx); err != nil {
			t.Fatalf("detect fence at 29: %v", err)
		}
		if !st.OutboundFenceEnabled() {
			t.Fatal("fence reported unavailable on core 29")
		}

		orgID, inboxID := seedCore28Outbox(t, ctx, db)
		id, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID: orgID, InboxID: inboxID, Provider: "smtp", IdempotencyKey: "core29-legacy",
			To: "recipient@local.neuralmail", From: "core28@local.neuralmail",
			Subject: "core29", TextBody: "core29 body",
		})
		if err != nil {
			t.Fatalf("enqueue on core 29: %v", err)
		}
		claimed, err := st.ClaimOutboxMessages(ctx, 10, "core29-worker", time.Now().UTC(), 5*time.Minute)
		if err != nil {
			t.Fatalf("claim on core 29: %v", err)
		}
		if len(claimed) != 1 || claimed[0].ID != id {
			t.Fatalf("claim returned %+v, want the enqueued row", claimed)
		}
	})
}
