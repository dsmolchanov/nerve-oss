package emailtransport

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"neuralmail/internal/memguard"
	"neuralmail/internal/store"
)

func TestOutboxWorkerPolicyTransitionBeforeProviderStartCallsNoProvider(t *testing.T) {
	withOutboxWorkerDatabase(t, func(ctx context.Context, db *sql.DB, st *store.Store) {
		orgID, inboxID := seedWorkerPolicyFence(t, ctx, db, st, "worker-policy-first")
		outboxID := insertWorkerPolicyOutbox(t, ctx, db, orgID, inboxID, 1)
		claimed := claimWorkerPolicyOutbox(t, ctx, st, outboxID, "policy-first-worker")

		if err := st.RunInTx(ctx, func(scoped *store.Store) error {
			_, _, err := scoped.AdvanceOutboundPolicyEpoch(ctx, orgID)
			return err
		}); err != nil {
			t.Fatal(err)
		}

		adapter := &captureOutboundAdapter{}
		worker := policyFenceWorker(t, st, adapter, "policy-first-worker")
		err := worker.deliverOne(ctx, claimed)
		if !errors.Is(err, store.ErrOutboxPolicyRevoked) {
			t.Fatalf("deliver error=%v, want policy revoked", err)
		}
		if len(adapter.deliveries) != 0 {
			t.Fatalf("provider called %d times", len(adapter.deliveries))
		}
		var status string
		var lastError sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT status, last_error FROM outbox_messages WHERE id = $1`, outboxID).Scan(&status, &lastError); err != nil {
			t.Fatal(err)
		}
		if status != "failed" || !lastError.Valid || lastError.String != "policy_revoked" {
			t.Fatalf("status=%q last_error=%q", status, lastError.String)
		}
	})
}

func TestOutboxWorkerProviderStartBeforePolicyTransitionDrainsSameOperation(t *testing.T) {
	withOutboxWorkerDatabase(t, func(ctx context.Context, db *sql.DB, st *store.Store) {
		orgID, inboxID := seedWorkerPolicyFence(t, ctx, db, st, "worker-provider-first")
		outboxID := insertWorkerPolicyOutbox(t, ctx, db, orgID, inboxID, 1)
		claimed := claimWorkerPolicyOutbox(t, ctx, st, outboxID, "provider-first-worker")
		operationID, err := st.BeginOutboxProviderOperation(ctx, claimed)
		if err != nil {
			t.Fatal(err)
		}

		if err := st.RunInTx(ctx, func(scoped *store.Store) error {
			_, terminalized, err := scoped.AdvanceOutboundPolicyEpoch(ctx, orgID)
			if terminalized != 0 {
				t.Fatalf("terminalized provider-started row=%d", terminalized)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}

		adapter := &captureOutboundAdapter{}
		worker := policyFenceWorker(t, st, adapter, "provider-first-worker")
		if err := worker.deliverOne(ctx, claimed); err != nil {
			t.Fatal(err)
		}
		if len(adapter.deliveries) != 1 || adapter.deliveries[0].idempotencyKey != operationID {
			t.Fatalf("deliveries=%+v", adapter.deliveries)
		}
		var status, storedOperationID string
		var resolvedAt sql.NullTime
		if err := db.QueryRowContext(ctx, `
			SELECT status, provider_operation_id, provider_resolved_at
			FROM outbox_messages WHERE id = $1
		`, outboxID).Scan(&status, &storedOperationID, &resolvedAt); err != nil {
			t.Fatal(err)
		}
		if status != "sent" || storedOperationID != operationID || !resolvedAt.Valid {
			t.Fatalf("status=%q operation=%q resolved=%v", status, storedOperationID, resolvedAt.Valid)
		}
	})
}

type ambiguousNonReplayAdapter struct{ calls int }

func (a *ambiguousNonReplayAdapter) Name() string { return "ambiguous" }
func (a *ambiguousNonReplayAdapter) SendMessage(context.Context, OutboundMessage, string) (string, error) {
	a.calls++
	return "", errors.New("ambiguous transport failure")
}
func (a *ambiguousNonReplayAdapter) GetDeliveryStatus(context.Context, string) (DeliveryStatus, error) {
	return DeliveryStatusUnknown, ErrNotSupported
}

func TestOutboxWorkerQuarantinesAmbiguousNonReplayProvider(t *testing.T) {
	withOutboxWorkerDatabase(t, func(ctx context.Context, db *sql.DB, st *store.Store) {
		orgID, inboxID := seedWorkerPolicyFence(t, ctx, db, st, "worker-non-replay")
		outboxID := insertWorkerPolicyOutbox(t, ctx, db, orgID, inboxID, 1)
		if _, err := db.ExecContext(ctx, `
			UPDATE outbox_messages SET provider = 'ambiguous' WHERE id = $1
		`, outboxID); err != nil {
			t.Fatal(err)
		}
		claimed := claimWorkerPolicyOutbox(t, ctx, st, outboxID, "non-replay-worker")
		adapter := &ambiguousNonReplayAdapter{}
		worker := policyFenceWorker(t, st, adapter, "non-replay-worker")
		if err := worker.deliverOne(ctx, claimed); err == nil {
			t.Fatal("ambiguous provider call unexpectedly succeeded")
		}
		var status, lastError string
		var startedAt, resolvedAt sql.NullTime
		if err := db.QueryRowContext(ctx, `
			SELECT status, last_error, provider_started_at, provider_resolved_at
			FROM outbox_messages WHERE id = $1
		`, outboxID).Scan(&status, &lastError, &startedAt, &resolvedAt); err != nil {
			t.Fatal(err)
		}
		if adapter.calls != 1 || status != "queued" || !startedAt.Valid || resolvedAt.Valid ||
			!strings.Contains(lastError, "provider_unknown_non_idempotent") {
			t.Fatalf("calls=%d status=%q error=%q started=%v resolved=%v", adapter.calls, status, lastError, startedAt.Valid, resolvedAt.Valid)
		}
	})
}

func TestOutboxWorkerQuarantinesAmbiguousMeteredProviderWithoutAutonomousEpoch(t *testing.T) {
	withOutboxWorkerDatabase(t, func(ctx context.Context, db *sql.DB, st *store.Store) {
		orgID, inboxID := uuid.NewString(), uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs(id,name) VALUES($1,'metered worker')`, orgID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO inboxes(id,org_id,address,status)
  VALUES($1,$2,'metered@local.neuralmail','active')`, inboxID, orgID); err != nil {
			t.Fatal(err)
		}
		period := store.RecipientPeriod{OrgID: orgID, PeriodID: uuid.NewString(),
			StartsAt: time.Now().UTC().Add(-time.Hour), EndsAt: time.Now().UTC().Add(time.Hour),
			Limit: sql.NullInt64{Int64: 1, Valid: true}}
		if err := st.RunInTx(ctx, func(tx *store.Store) error { return tx.InstallRecipientPeriod(ctx, period) }); err != nil {
			t.Fatal(err)
		}
		id, err := st.EnqueueOutboxMessage(ctx, store.OutboxMessage{OrgID: orgID, InboxID: inboxID,
			Provider: "ambiguous", IdempotencyKey: "metered-ambiguous", To: "recipient@example.test",
			From: "metered@local.neuralmail", Subject: "metered", TextBody: "body"})
		if err != nil {
			t.Fatal(err)
		}
		claimed := claimWorkerPolicyOutbox(t, ctx, st, id, "metered-worker")
		if claimed.AutonomousPolicyEpoch != 0 {
			t.Fatalf("unexpected autonomous epoch: %d", claimed.AutonomousPolicyEpoch)
		}
		adapter := &ambiguousNonReplayAdapter{}
		worker := policyFenceWorker(t, st, adapter, "metered-worker")
		if err := worker.deliverOne(ctx, claimed); err == nil {
			t.Fatal("ambiguous provider call unexpectedly succeeded")
		}
		var status string
		var startedAt, resolvedAt sql.NullTime
		var reserved, committed int64
		if err := db.QueryRowContext(ctx, `SELECT status,provider_started_at,provider_resolved_at
  FROM outbox_messages WHERE org_id=$1 AND id=$2`, orgID, id).Scan(&status, &startedAt, &resolvedAt); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRowContext(ctx, `SELECT reserved,committed FROM org_recipient_periods
  WHERE org_id=$1 AND period_id=$2`, orgID, period.PeriodID).Scan(&reserved, &committed); err != nil {
			t.Fatal(err)
		}
		if adapter.calls != 1 || status != "queued" || !startedAt.Valid || resolvedAt.Valid || reserved != 1 || committed != 0 {
			t.Fatalf("calls=%d status=%q started=%v resolved=%v reserved=%d committed=%d",
				adapter.calls, status, startedAt.Valid, resolvedAt.Valid, reserved, committed)
		}
		if _, err := db.ExecContext(ctx, `UPDATE outbox_messages
  SET next_attempt_at=now()-interval '1 second' WHERE org_id=$1 AND id=$2`, orgID, id); err != nil {
			t.Fatal(err)
		}
		reclaimed := claimWorkerPolicyOutbox(t, ctx, st, id, "metered-recovery-worker")
		if reclaimed.AutonomousPolicyEpoch != 0 || !reclaimed.ProviderStartedAt.Valid || reclaimed.ProviderResolvedAt.Valid {
			t.Fatalf("invalid unresolved metered claim: %+v", reclaimed)
		}
		worker.WorkerID = "metered-recovery-worker"
		if err := worker.deliverOne(ctx, reclaimed); err == nil {
			t.Fatal("unresolved non-replay metered operation unexpectedly succeeded")
		}
		if adapter.calls != 1 {
			t.Fatalf("ambiguous metered operation called provider twice: %d", adapter.calls)
		}
	})
}

func TestOutboxWorkerQuarantinesExpiredMeteredReplayBeforeProviderCall(t *testing.T) {
	withOutboxWorkerDatabase(t, func(ctx context.Context, db *sql.DB, st *store.Store) {
		orgID, inboxID := uuid.NewString(), uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs(id,name) VALUES($1,'metered replay')`, orgID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO inboxes(id,org_id,address,status)
  VALUES($1,$2,'metered-replay@local.neuralmail','active')`, inboxID, orgID); err != nil {
			t.Fatal(err)
		}
		period := store.RecipientPeriod{OrgID: orgID, PeriodID: uuid.NewString(),
			StartsAt: time.Now().UTC().Add(-time.Hour), EndsAt: time.Now().UTC().Add(time.Hour),
			Limit: sql.NullInt64{Int64: 1, Valid: true}}
		if err := st.RunInTx(ctx, func(tx *store.Store) error { return tx.InstallRecipientPeriod(ctx, period) }); err != nil {
			t.Fatal(err)
		}
		id, err := st.EnqueueOutboxMessage(ctx, store.OutboxMessage{OrgID: orgID, InboxID: inboxID,
			Provider: "server-replay", IdempotencyKey: "metered-expired", To: "recipient@example.test",
			From: "metered-replay@local.neuralmail", Subject: "metered", TextBody: "body"})
		if err != nil {
			t.Fatal(err)
		}
		claimed := claimWorkerPolicyOutbox(t, ctx, st, id, "metered-replay-worker")
		if claimed.AutonomousPolicyEpoch != 0 {
			t.Fatalf("unexpected autonomous epoch: %d", claimed.AutonomousPolicyEpoch)
		}
		adapter := &transientServerReplayAdapter{}
		worker := policyFenceWorker(t, st, adapter, "metered-replay-worker")
		if err := worker.deliverOne(ctx, claimed); err == nil || adapter.calls != 1 {
			t.Fatalf("initial ambiguous call err=%v calls=%d", err, adapter.calls)
		}
		if _, err := db.ExecContext(ctx, `UPDATE outbox_messages
  SET provider_started_at=now()-interval '25 hours',next_attempt_at=now()-interval '1 second'
  WHERE org_id=$1 AND id=$2`, orgID, id); err != nil {
			t.Fatal(err)
		}
		reclaimed := claimWorkerPolicyOutbox(t, ctx, st, id, "metered-replay-recovery")
		worker.WorkerID = "metered-replay-recovery"
		if err := worker.deliverOne(ctx, reclaimed); err == nil {
			t.Fatal("expired metered replay unexpectedly succeeded")
		}
		var status, lastError string
		var resolvedAt sql.NullTime
		if err := db.QueryRowContext(ctx, `SELECT status,last_error,provider_resolved_at
  FROM outbox_messages WHERE org_id=$1 AND id=$2`, orgID, id).Scan(&status, &lastError, &resolvedAt); err != nil {
			t.Fatal(err)
		}
		if adapter.calls != 1 || status != "queued" || resolvedAt.Valid ||
			!strings.Contains(lastError, "replay_window_expired") {
			t.Fatalf("expired metered replay calls=%d status=%q error=%q resolved=%v",
				adapter.calls, status, lastError, resolvedAt.Valid)
		}
	})
}

type transientServerReplayAdapter struct{ calls int }

func (a *transientServerReplayAdapter) Name() string                   { return "server-replay" }
func (a *transientServerReplayAdapter) SupportsIdempotentReplay() bool { return true }
func (a *transientServerReplayAdapter) IdempotentReplayWindow() time.Duration {
	return 24 * time.Hour
}
func (a *transientServerReplayAdapter) SendMessage(context.Context, OutboundMessage, string) (string, error) {
	a.calls++
	return "", NewTransientError(503, "server_error", errors.New("provider unavailable"))
}
func (a *transientServerReplayAdapter) GetDeliveryStatus(context.Context, string) (DeliveryStatus, error) {
	return DeliveryStatusUnknown, ErrNotSupported
}

func TestOutboxWorkerKeepsRetryableServerErrorUnresolved(t *testing.T) {
	withOutboxWorkerDatabase(t, func(ctx context.Context, db *sql.DB, st *store.Store) {
		orgID, inboxID := seedWorkerPolicyFence(t, ctx, db, st, "worker-server-replay")
		outboxID := insertWorkerPolicyOutbox(t, ctx, db, orgID, inboxID, 1)
		if _, err := db.ExecContext(ctx, `UPDATE outbox_messages SET provider = 'server-replay' WHERE id = $1`, outboxID); err != nil {
			t.Fatal(err)
		}
		claimed := claimWorkerPolicyOutbox(t, ctx, st, outboxID, "server-replay-worker")
		adapter := &transientServerReplayAdapter{}
		worker := policyFenceWorker(t, st, adapter, "server-replay-worker")
		if err := worker.deliverOne(ctx, claimed); err == nil {
			t.Fatal("retryable provider call unexpectedly succeeded")
		}
		var status string
		var operationID sql.NullString
		var startedAt, resolvedAt sql.NullTime
		if err := db.QueryRowContext(ctx, `
			SELECT status, provider_operation_id, provider_started_at, provider_resolved_at
			FROM outbox_messages WHERE id = $1
		`, outboxID).Scan(&status, &operationID, &startedAt, &resolvedAt); err != nil {
			t.Fatal(err)
		}
		if adapter.calls != 1 || status != "queued" || !operationID.Valid || !startedAt.Valid || resolvedAt.Valid {
			t.Fatalf("calls=%d status=%q operation=%q started=%v resolved=%v", adapter.calls, status, operationID.String, startedAt.Valid, resolvedAt.Valid)
		}
	})
}

func TestOutboxWorkerQuarantinesUnresolvedNonReplayOperationBeforeProviderCall(t *testing.T) {
	withOutboxWorkerDatabase(t, func(ctx context.Context, db *sql.DB, st *store.Store) {
		orgID, inboxID := seedWorkerPolicyFence(t, ctx, db, st, "worker-unreplayable-recovery")
		outboxID := insertWorkerPolicyOutbox(t, ctx, db, orgID, inboxID, 1)
		if _, err := db.ExecContext(ctx, `UPDATE outbox_messages SET provider = 'ambiguous' WHERE id = $1`, outboxID); err != nil {
			t.Fatal(err)
		}
		first := claimWorkerPolicyOutbox(t, ctx, st, outboxID, "crashed-worker")
		operationID, err := st.BeginOutboxProviderOperation(ctx, first)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE outbox_messages SET locked_at = now() - interval '10 minutes' WHERE id = $1`, outboxID); err != nil {
			t.Fatal(err)
		}
		recovered := claimWorkerPolicyOutbox(t, ctx, st, outboxID, "recovery-worker")
		adapter := &ambiguousNonReplayAdapter{}
		worker := policyFenceWorker(t, st, adapter, "recovery-worker")
		if err := worker.deliverOne(ctx, recovered); err == nil {
			t.Fatal("unresolved non-replay operation unexpectedly succeeded")
		}
		var status, storedOperationID, lastError string
		var resolvedAt sql.NullTime
		if err := db.QueryRowContext(ctx, `
			SELECT status, provider_operation_id, last_error, provider_resolved_at
			FROM outbox_messages WHERE id = $1
		`, outboxID).Scan(&status, &storedOperationID, &lastError, &resolvedAt); err != nil {
			t.Fatal(err)
		}
		if adapter.calls != 0 || status != "queued" || storedOperationID != operationID || resolvedAt.Valid ||
			!strings.Contains(lastError, "provider_unknown_non_idempotent") {
			t.Fatalf("calls=%d status=%q operation=%q error=%q resolved=%v", adapter.calls, status, storedOperationID, lastError, resolvedAt.Valid)
		}
	})
}

func TestOutboxWorkerQuarantinesExpiredReplayWindowBeforeProviderCall(t *testing.T) {
	withOutboxWorkerDatabase(t, func(ctx context.Context, db *sql.DB, st *store.Store) {
		orgID, inboxID := seedWorkerPolicyFence(t, ctx, db, st, "worker-expired-replay")
		outboxID := insertWorkerPolicyOutbox(t, ctx, db, orgID, inboxID, 1)
		if _, err := db.ExecContext(ctx, `UPDATE outbox_messages SET provider = 'server-replay' WHERE id = $1`, outboxID); err != nil {
			t.Fatal(err)
		}
		first := claimWorkerPolicyOutbox(t, ctx, st, outboxID, "expired-crashed-worker")
		operationID, err := st.BeginOutboxProviderOperation(ctx, first)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `
			UPDATE outbox_messages
			SET provider_started_at = now() - interval '25 hours',
			    locked_at = now() - interval '10 minutes'
			WHERE id = $1
		`, outboxID); err != nil {
			t.Fatal(err)
		}
		recovered := claimWorkerPolicyOutbox(t, ctx, st, outboxID, "expired-recovery-worker")
		adapter := &transientServerReplayAdapter{}
		worker := policyFenceWorker(t, st, adapter, "expired-recovery-worker")
		if err := worker.deliverOne(ctx, recovered); err == nil {
			t.Fatal("expired replay operation unexpectedly succeeded")
		}
		var status, storedOperationID, lastError string
		var resolvedAt sql.NullTime
		if err := db.QueryRowContext(ctx, `
			SELECT status, provider_operation_id, last_error, provider_resolved_at
			FROM outbox_messages WHERE id = $1
		`, outboxID).Scan(&status, &storedOperationID, &lastError, &resolvedAt); err != nil {
			t.Fatal(err)
		}
		if adapter.calls != 0 || status != "queued" || storedOperationID != operationID || resolvedAt.Valid ||
			!strings.Contains(lastError, "replay_window_expired") {
			t.Fatalf("calls=%d status=%q operation=%q error=%q resolved=%v", adapter.calls, status, storedOperationID, lastError, resolvedAt.Valid)
		}
	})
}

func seedWorkerPolicyFence(t *testing.T, ctx context.Context, db *sql.DB, st *store.Store, name string) (string, string) {
	t.Helper()
	orgID, inboxID := uuid.NewString(), uuid.NewString()
	if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, $2)`, orgID, name); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO inboxes (id, org_id, address, status)
		VALUES ($1, $2, $3, 'active')
	`, inboxID, orgID, name+"@local.neuralmail"); err != nil {
		t.Fatal(err)
	}
	if err := st.RunInTx(ctx, func(scoped *store.Store) error {
		if _, err := scoped.SetFeatureFlag(ctx, &orgID, "autonomous_outbound_policy", true, "test"); err != nil {
			return err
		}
		if _, err := scoped.SetFeatureFlag(ctx, &orgID, "email_outbound_suspended", false, "test"); err != nil {
			return err
		}
		_, err := scoped.EnsureOutboundPolicyState(ctx, orgID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return orgID, inboxID
}

func insertWorkerPolicyOutbox(t *testing.T, ctx context.Context, db *sql.DB, orgID, inboxID string, epoch int64) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO outbox_messages (
		  id, org_id, inbox_id, provider, idempotency_key, "to", "from", subject,
		  text_body, autonomous_policy_epoch
		) VALUES ($1, $2, $3, 'capture', $4, 'recipient@example.com', $5, 'subject', 'body', $6)
	`, id, orgID, inboxID, "policy:"+id, "sender@local.neuralmail", epoch); err != nil {
		t.Fatal(err)
	}
	return id
}

func claimWorkerPolicyOutbox(t *testing.T, ctx context.Context, st *store.Store, id, workerID string) store.OutboxMessage {
	t.Helper()
	claimed, err := st.ClaimOutboxMessages(ctx, 10, workerID, time.Now().UTC(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range claimed {
		if msg.ID == id {
			return msg
		}
	}
	t.Fatalf("outbox %s not claimed: %+v", id, claimed)
	return store.OutboxMessage{}
}

func policyFenceWorker(t *testing.T, st *store.Store, adapter OutboundAdapter, workerID string) *OutboxWorker {
	t.Helper()
	registry := NewRegistry()
	if err := registry.RegisterOutbound(adapter); err != nil {
		t.Fatal(err)
	}
	budget, err := memguard.New(64 << 20)
	if err != nil {
		t.Fatal(err)
	}
	return NewOutboxWorker(st, registry, workerID, budget)
}
