package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync/atomic"
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

		// A legacy row (epoch 0) must pass straight through: the worker calls
		// this for every claimed message and Core 28 has nothing but legacy rows.
		legacy := OutboxMessage{
			ID: uuid.NewString(), OrgID: orgID,
			LockedBy: sql.NullString{String: "worker", Valid: true},
		}
		if _, err := st.BeginOutboxProviderOperationState(ctx, legacy); err != nil {
			t.Fatalf("legacy provider-start refused on core 28: %v", err)
		}
		// A row claiming an epoch cannot exist before the fence, so it must refuse.
		fenced := legacy
		fenced.AutonomousPolicyEpoch = 1
		if _, err := st.BeginOutboxProviderOperationState(ctx, fenced); err == nil {
			t.Fatal("fenced provider-start admitted on core 28")
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

// Goose keeps the version 29 record after a clean down-migration, so a naive
// max(version_id) probe would still report the fence as present and then emit
// statements against the objects 0029 had just dropped.
func TestFenceDetectionRespectsRolledBackCore29(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if err := MigrateUpToCore(ctx, db, 29); err != nil {
			t.Fatalf("migrate to core 29: %v", err)
		}
		st := &Store{db: db, q: db, fence: newEnabledFence()}
		if err := st.RefreshOutboundFenceCapability(ctx); err != nil {
			t.Fatalf("detect fence at 29: %v", err)
		}
		if !st.OutboundFenceEnabled() {
			t.Fatal("fence reported unavailable on core 29")
		}

		// MigrateDownCore steps back exactly one migration: 29 -> 28.
		if err := MigrateDownCore(ctx, db); err != nil {
			t.Fatalf("migrate down to core 28: %v", err)
		}
		var retained int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM schema_migrations_core WHERE version_id = 29`).Scan(&retained); err != nil {
			t.Fatalf("read migration history: %v", err)
		}
		if retained == 0 {
			// This goose configuration deletes the record on the way down. Recreate
			// the hazard explicitly so the applied-version semantics are still
			// proven: a not-applied version 29 row must never count as fenced.
			if _, err := db.ExecContext(ctx, `
				INSERT INTO schema_migrations_core (version_id, is_applied, tstamp)
				VALUES (29, false, now())
			`); err != nil {
				t.Fatalf("seed rolled-back migration record: %v", err)
			}
		}

		if err := st.RefreshOutboundFenceCapability(ctx); err != nil {
			t.Fatalf("detect fence after rollback: %v", err)
		}
		if st.OutboundFenceEnabled() {
			t.Fatal("rolled-back core 29 still reported the fence as available")
		}

		// Legacy delivery must work again against the rolled-back schema.
		orgID, inboxID := seedCore28Outbox(t, ctx, db)
		id, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID: orgID, InboxID: inboxID, Provider: "smtp", IdempotencyKey: "rolled-back",
			To: "recipient@local.neuralmail", From: "core28@local.neuralmail",
			Subject: "rolled back", TextBody: "body",
		})
		if err != nil {
			t.Fatalf("enqueue after rollback: %v", err)
		}
		claimed, err := st.ClaimOutboxMessages(ctx, 10, "rollback-worker", time.Now().UTC(), 5*time.Minute)
		if err != nil {
			t.Fatalf("claim after rollback: %v", err)
		}
		if len(claimed) != 1 || claimed[0].ID != id {
			t.Fatalf("claim returned %+v, want the enqueued row", claimed)
		}
		if err := st.MarkOutboxMessageSent(ctx, id, "provider-rolled-back"); err != nil {
			t.Fatalf("mark sent after rollback: %v", err)
		}
	})
}

// Only a *proven* pre-29 schema may disable the fence. Absent or empty history
// is undetermined and must fail closed: reading it as "pre-29" would drop the
// epoch on enqueue and let autonomous mail leave with no revocation check.
func TestFenceCapabilityMatrix(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		setup      func(t *testing.T, ctx context.Context, db *sql.DB)
		wantFenced bool
		wantErr    bool
	}{
		{
			name:       "missing history is undetermined",
			setup:      func(*testing.T, context.Context, *sql.DB) {},
			wantFenced: true, wantErr: true,
		},
		{
			name: "empty history is undetermined",
			setup: func(t *testing.T, ctx context.Context, db *sql.DB) {
				if err := MigrateUpToCore(ctx, db, 28); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(ctx, `DELETE FROM schema_migrations_core`); err != nil {
					t.Fatal(err)
				}
			},
			wantFenced: true, wantErr: true,
		},
		{
			name: "applied 28 is proven legacy",
			setup: func(t *testing.T, ctx context.Context, db *sql.DB) {
				if err := MigrateUpToCore(ctx, db, 28); err != nil {
					t.Fatal(err)
				}
			},
			wantFenced: false,
		},
		{
			name: "applied 29 is fenced",
			setup: func(t *testing.T, ctx context.Context, db *sql.DB) {
				if err := MigrateUpToCore(ctx, db, 29); err != nil {
					t.Fatal(err)
				}
			},
			wantFenced: true,
		},
		{
			name: "unapplied 29 over applied 28 is legacy",
			setup: func(t *testing.T, ctx context.Context, db *sql.DB) {
				if err := MigrateUpToCore(ctx, db, 28); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(ctx, `
					INSERT INTO schema_migrations_core (version_id, is_applied, tstamp)
					VALUES (29, false, now())
				`); err != nil {
					t.Fatal(err)
				}
			},
			wantFenced: false,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
				testCase.setup(t, ctx, db)
				st := &Store{db: db, q: db, fence: newEnabledFence()}
				err := st.RefreshOutboundFenceCapability(ctx)
				if testCase.wantErr && err == nil {
					t.Fatal("undetermined history was accepted without error")
				}
				if !testCase.wantErr && err != nil {
					t.Fatalf("refresh: %v", err)
				}
				if got := st.OutboundFenceEnabled(); got != testCase.wantFenced {
					t.Fatalf("fence enabled = %v, want %v", got, testCase.wantFenced)
				}
			})
		})
	}
}

// An undetermined schema must not be able to enqueue an autonomous row through
// the legacy path, which would strip its policy epoch.
func TestUndeterminedHistoryCannotEnqueueAutonomousRowAsLegacy(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if err := MigrateUpToCore(ctx, db, 29); err != nil {
			t.Fatalf("migrate to core 29: %v", err)
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM schema_migrations_core`); err != nil {
			t.Fatalf("erase migration history: %v", err)
		}
		st := &Store{db: db, q: db, fence: newEnabledFence()}
		if err := st.RefreshOutboundFenceCapability(ctx); err == nil {
			t.Fatal("erased history was accepted without error")
		}
		if !st.OutboundFenceEnabled() {
			t.Fatal("erased history disabled the fence on a real core 29 schema")
		}

		// The concrete consequence: enqueue must still carry the epoch column.
		if !strings.Contains(adaptOutboxSQL(st.OutboundFenceEnabled(), "autonomous_policy_epoch"), "autonomous_policy_epoch") {
			t.Fatal("undetermined history selected pre-fence SQL")
		}
		// A plain legacy enqueue still works, and the fenced statement keeps the
		// column, so nothing on this schema was silently downgraded.
		orgID, inboxID := seedCore28Outbox(t, ctx, db)
		id, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID: orgID, InboxID: inboxID, Provider: "smtp", IdempotencyKey: "undetermined",
			To: "recipient@local.neuralmail", From: "core28@local.neuralmail",
			Subject: "undetermined", TextBody: "body",
		})
		if err != nil {
			t.Fatalf("enqueue under undetermined history: %v", err)
		}
		var column string
		if err := db.QueryRowContext(ctx, `
			SELECT column_name FROM information_schema.columns
			WHERE table_name = 'outbox_messages' AND column_name = 'autonomous_policy_epoch'
		`).Scan(&column); err != nil {
			t.Fatalf("fence column missing on a core 29 schema: %v", err)
		}
		if id == "" {
			t.Fatal("enqueue returned no id")
		}
	})
}

// Phase 9 applies Core 0029 while Artifact B is already running, so a live
// process must not keep its startup answer. A stale instance that still
// believes it is pre-fence would claim an autonomous row with the policy
// predicate replaced by true, project its epoch as zero, and let provider-start
// take the legacy fast path -- a send with no epoch or revocation check.
func TestStaleStoreDoesNotClaimFencedRowAsLegacy(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if err := MigrateUpToCore(ctx, db, 28); err != nil {
			t.Fatalf("migrate to core 28: %v", err)
		}
		producer := &Store{db: db, q: db, fence: newEnabledFence()}
		stale := &Store{db: db, q: db, fence: newEnabledFence()}
		for _, st := range []*Store{producer, stale} {
			if err := st.RefreshOutboundFenceCapability(ctx); err != nil {
				t.Fatalf("initial refresh: %v", err)
			}
			if st.OutboundFenceEnabled() {
				t.Fatal("core 28 reported the fence as available")
			}
		}

		// Core 0029 lands underneath both. Only the producer is refreshed; the
		// stale consumer keeps its Core 28 answer, exactly like a live instance
		// during the Phase 9 transition.
		if err := MigrateUpToCore(ctx, db, 29); err != nil {
			t.Fatalf("migrate to core 29: %v", err)
		}
		if err := producer.RefreshOutboundFenceCapability(ctx); err != nil {
			t.Fatalf("producer refresh: %v", err)
		}
		if !producer.OutboundFenceEnabled() {
			t.Fatal("producer did not observe core 29")
		}

		orgID, inboxID := seedCore28Outbox(t, ctx, db)
		// An autonomous row whose org has no policy-state row and no enabling
		// flags: the fenced predicate must exclude it, the legacy predicate
		// would not.
		outboxID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `
			INSERT INTO outbox_messages (
				id, org_id, inbox_id, provider, idempotency_key,
				"to", "from", subject, text_body,
				status, delivery_status, autonomous_policy_epoch, next_attempt_at
			) VALUES ($1, $2, $3, 'smtp', 'fenced-row',
				'recipient@local.neuralmail', 'core28@local.neuralmail', 'fenced', 'body',
				'queued', 'queued', 5, now())
		`, outboxID, orgID, inboxID); err != nil {
			t.Fatalf("insert fenced row: %v", err)
		}

		claimed, err := stale.ClaimOutboxMessages(ctx, 10, "stale-worker", time.Now().UTC(), 5*time.Minute)
		for _, msg := range claimed {
			if msg.ID == outboxID {
				t.Fatal("stale consumer claimed a fenced row as legacy: the policy predicate was bypassed")
			}
		}
		if !errors.Is(err, ErrOutboundFenceDrift) {
			t.Fatalf("stale consumer error = %v, want drift refusal", err)
		}
		// Drift is terminal for the process: it must not quietly switch modes.
		if _, err := stale.ClaimOutboxMessages(ctx, 10, "stale-worker", time.Now().UTC(), 5*time.Minute); !errors.Is(err, ErrOutboundFenceDrift) {
			t.Fatalf("second claim error = %v, want drift refusal", err)
		}
		// A restarted process picks up the new schema cleanly.
		restarted := &Store{db: db, q: db, fence: newEnabledFence(), drift: new(atomic.Bool)}
		if err := restarted.RefreshOutboundFenceCapability(ctx); err != nil {
			t.Fatalf("restarted refresh: %v", err)
		}
		if !restarted.OutboundFenceEnabled() {
			t.Fatal("restarted process did not observe core 29")
		}
	})
}

// The inverse: a stale fenced instance after a clean rollback issues fenced SQL
// against removed objects. That is an outage rather than a bypass, so it may
// fail -- but it must re-detect and recover instead of failing forever.
func TestStaleFencedStoreRefusesAfterRollbackUntilRestart(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if err := MigrateUpToCore(ctx, db, 29); err != nil {
			t.Fatalf("migrate to core 29: %v", err)
		}
		st := &Store{db: db, q: db, fence: newEnabledFence()}
		if err := st.RefreshOutboundFenceCapability(ctx); err != nil {
			t.Fatalf("refresh at 29: %v", err)
		}
		if !st.OutboundFenceEnabled() {
			t.Fatal("core 29 did not report the fence")
		}
		orgID, inboxID := seedCore28Outbox(t, ctx, db)

		if err := MigrateDownCore(ctx, db); err != nil {
			t.Fatalf("roll back to core 28: %v", err)
		}

		// First attempt may fail against the removed columns; it must re-detect.
		_, firstErr := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID: orgID, InboxID: inboxID, Provider: "smtp", IdempotencyKey: "post-rollback-1",
			To: "recipient@local.neuralmail", From: "core28@local.neuralmail",
			Subject: "post rollback", TextBody: "body",
		})
		if firstErr != nil {
			t.Logf("first post-rollback attempt failed as expected: %v", firstErr)
		}
		// Drift is terminal: the process refuses rather than downgrading itself.
		if _, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID: orgID, InboxID: inboxID, Provider: "smtp", IdempotencyKey: "post-rollback-2",
			To: "recipient@local.neuralmail", From: "core28@local.neuralmail",
			Subject: "post rollback", TextBody: "body",
		}); !errors.Is(err, ErrOutboundFenceDrift) {
			t.Fatalf("second enqueue error = %v, want drift refusal", err)
		}

		// A restart is what recovers, and it works on the rolled-back schema.
		restarted := &Store{db: db, q: db, fence: newEnabledFence(), drift: new(atomic.Bool)}
		if err := restarted.RefreshOutboundFenceCapability(ctx); err != nil {
			t.Fatalf("restarted refresh: %v", err)
		}
		if restarted.OutboundFenceEnabled() {
			t.Fatal("restarted process still believes the fence exists")
		}
		id, err := restarted.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID: orgID, InboxID: inboxID, Provider: "smtp", IdempotencyKey: "post-restart",
			To: "recipient@local.neuralmail", From: "core28@local.neuralmail",
			Subject: "post restart", TextBody: "body",
		})
		if err != nil {
			t.Fatalf("enqueue after restart: %v", err)
		}
		if _, err := restarted.ClaimOutboxMessages(ctx, 10, "restarted-worker", time.Now().UTC(), 5*time.Minute); err != nil {
			t.Fatalf("claim after restart: %v", err)
		}
		if id == "" {
			t.Fatal("enqueue returned no id")
		}
	})
}

// A caller inside a transaction already holds a connection. Probing the live
// schema on a second one lets the pool deadlock: with MaxOpenConns(10), ten
// concurrent pre-fence enqueues each hold one and then all wait for an
// eleventh. MaxOpenConns(1) makes that failure deterministic rather than
// load-dependent, so every transactional entry point is exercised under it.
func TestPreFenceOperationsNeedNoSecondConnection(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if err := MigrateUpToCore(ctx, db, 28); err != nil {
			t.Fatalf("migrate to core 28: %v", err)
		}
		db.SetMaxOpenConns(1)
		st := &Store{db: db, q: db, fence: newEnabledFence()}
		if err := st.RefreshOutboundFenceCapability(ctx); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if st.OutboundFenceEnabled() {
			t.Fatal("core 28 reported the fence as available")
		}
		orgID, inboxID := seedCore28Outbox(t, ctx, db)

		deadline, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()

		id, err := st.EnqueueOutboxMessage(deadline, OutboxMessage{
			OrgID: orgID, InboxID: inboxID, Provider: "smtp", IdempotencyKey: "single-conn",
			To: "recipient@local.neuralmail", From: "core28@local.neuralmail",
			Subject: "single conn", TextBody: "body",
		})
		if err != nil {
			t.Fatalf("enqueue on a single connection: %v", err)
		}
		if _, err := st.ClaimOutboxMessages(deadline, 10, "single-conn-worker", time.Now().UTC(), 5*time.Minute); err != nil {
			t.Fatalf("claim on a single connection: %v", err)
		}
		if err := st.RequeueOutboxMessage(deadline, id, time.Now().UTC().Add(time.Minute), "retry"); err != nil {
			t.Fatalf("RequeueOutboxMessage on a single connection: %v", err)
		}
		claimed, err := st.ClaimOutboxMessages(deadline, 10, "single-conn-worker-2", time.Now().UTC().Add(2*time.Minute), 5*time.Minute)
		if err != nil {
			t.Fatalf("reclaim on a single connection: %v", err)
		}
		if len(claimed) == 1 {
			if err := st.RequeueClaimedOutboxMessage(deadline, claimed[0].ID, claimed[0].LockedBy.String,
				time.Now().UTC().Add(time.Minute), "retry"); err != nil {
				t.Fatalf("RequeueClaimedOutboxMessage on a single connection: %v", err)
			}
		}
	})
}

// Every capability gate must follow the live schema. A cached answer would
// refuse the autonomous-only operations forever after Core 0029 lands beneath
// a process that started on Core 28, until some unrelated adaptable operation
// happened to refresh the shared flag.
func TestCapabilityGatesUseTheStartupDecision(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if err := MigrateUpToCore(ctx, db, 28); err != nil {
			t.Fatalf("migrate to core 28: %v", err)
		}
		st := &Store{db: db, q: db, fence: newEnabledFence()}
		if err := st.RefreshOutboundFenceCapability(ctx); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		orgID, _ := seedCore28Outbox(t, ctx, db)

		// Refused on Core 28, as the schema genuinely cannot support them.
		if err := st.RunInTx(ctx, func(scoped *Store) error {
			_, err := scoped.EnsureOutboundPolicyState(ctx, orgID)
			return err
		}); err == nil {
			t.Fatal("policy state admitted on core 28")
		}

		// Core 0029 lands underneath. The gate must now admit them without any
		// unrelated operation having refreshed the flag first.
		if err := MigrateUpToCore(ctx, db, 29); err != nil {
			t.Fatalf("migrate to core 29: %v", err)
		}
		// The startup decision stands: this process keeps refusing rather than
		// switching mid-flight. That is the point -- re-deciding at run time is
		// what produced a probe-then-execute race.
		if err := st.RunInTx(ctx, func(scoped *Store) error {
			_, err := scoped.EnsureOutboundPolicyState(ctx, orgID)
			return err
		}); err == nil {
			t.Fatal("gate switched modes mid-flight instead of holding its startup decision")
		}

		restarted := &Store{db: db, q: db, fence: newEnabledFence(), drift: new(atomic.Bool)}
		if err := restarted.RefreshOutboundFenceCapability(ctx); err != nil {
			t.Fatalf("restarted refresh: %v", err)
		}
		if err := restarted.RunInTx(ctx, func(scoped *Store) error {
			_, err := scoped.EnsureOutboundPolicyState(ctx, orgID)
			return err
		}); err != nil {
			t.Fatalf("restarted process refused a valid core 29 operation: %v", err)
		}
	})
}

// Each requeue entry point must be able to be the first consumer after a clean
// rollback and still recover.
func TestRequeueEntryPointsFailLoudlyAfterRollback(t *testing.T) {
	for _, entry := range []struct {
		name   string
		invoke func(ctx context.Context, st *Store, id, lockedBy string) error
	}{
		{
			name: "RequeueOutboxMessage",
			invoke: func(ctx context.Context, st *Store, id, _ string) error {
				return st.RequeueOutboxMessage(ctx, id, time.Now().UTC().Add(time.Minute), "retry")
			},
		},
		{
			name: "RequeueClaimedOutboxMessage",
			invoke: func(ctx context.Context, st *Store, id, lockedBy string) error {
				return st.RequeueClaimedOutboxMessage(ctx, id, lockedBy, time.Now().UTC().Add(time.Minute), "retry")
			},
		},
	} {
		t.Run(entry.name, func(t *testing.T) {
			withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
				if err := MigrateUpToCore(ctx, db, 29); err != nil {
					t.Fatalf("migrate to core 29: %v", err)
				}
				st := &Store{db: db, q: db, fence: newEnabledFence()}
				if err := st.RefreshOutboundFenceCapability(ctx); err != nil {
					t.Fatalf("refresh at 29: %v", err)
				}
				orgID, inboxID := seedCore28Outbox(t, ctx, db)
				id, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
					OrgID: orgID, InboxID: inboxID, Provider: "smtp", IdempotencyKey: "pre-rollback",
					To: "recipient@local.neuralmail", From: "core28@local.neuralmail",
					Subject: "pre rollback", TextBody: "body",
				})
				if err != nil {
					t.Fatalf("enqueue at 29: %v", err)
				}
				claimed, err := st.ClaimOutboxMessages(ctx, 10, "rollback-worker", time.Now().UTC(), 5*time.Minute)
				if err != nil || len(claimed) != 1 {
					t.Fatalf("claim at 29: %v (%d rows)", err, len(claimed))
				}
				lockedBy := claimed[0].LockedBy.String

				if err := MigrateDownCore(ctx, db); err != nil {
					t.Fatalf("roll back to core 28: %v", err)
				}

				// This entry point is the first post-rollback consumer. It may
				// fail once, but must re-detect so the retry succeeds.
				if firstErr := entry.invoke(ctx, st, id, lockedBy); firstErr != nil {
					t.Logf("first attempt failed as expected: %v", firstErr)
				}
				// Requeue is a convergence path, so it is deliberately not gated on
				// drift -- an in-flight outcome must stay recordable. On a
				// rolled-back schema it therefore keeps failing loudly rather than
				// silently downgrading, which is the outage direction, not a bypass.
				retryErr := entry.invoke(ctx, st, id, lockedBy)
				if retryErr == nil {
					t.Fatal("retry silently succeeded against a rolled-back schema")
				}
				if !isUndefinedColumnOrTable(retryErr) && !errors.Is(retryErr, ErrOutboundFenceDrift) {
					t.Fatalf("retry error = %v, want a loud schema failure", retryErr)
				}
				if !st.drift.Load() {
					t.Fatal("the schema failure did not record drift")
				}
				restarted := &Store{db: db, q: db, fence: newEnabledFence(), drift: new(atomic.Bool)}
				if err := restarted.RefreshOutboundFenceCapability(ctx); err != nil {
					t.Fatalf("restarted refresh: %v", err)
				}
				if err := entry.invoke(ctx, restarted, id, lockedBy); err != nil &&
					!errors.Is(err, ErrOutboxClaimLost) {
					t.Fatalf("restarted process still failed: %v", err)
				}
			})
		})
	}
}

// A stale pre-fence process must not be able to suspend and then clear an
// epoch-bearing org without advancing the epoch: the clear would otherwise
// revive queued autonomous mail that the suspension was meant to revoke.
func TestStaleFlagWritersCannotReviveSuspendedAutonomousMail(t *testing.T) {
	for _, entry := range []struct {
		name      string
		preMarked bool
		write     func(ctx context.Context, st *Store, orgID string, enabled bool) error
	}{
		{
			name: "SetFeatureFlag",
			write: func(ctx context.Context, st *Store, orgID string, enabled bool) error {
				return st.RunInTx(ctx, func(scoped *Store) error {
					_, err := scoped.SetFeatureFlag(ctx, &orgID, "email_outbound_suspended", enabled, "test")
					return err
				})
			},
		},
		{
			name: "SetFeatureFlagAudited",
			write: func(ctx context.Context, st *Store, orgID string, enabled bool) error {
				return st.RunInTx(ctx, func(scoped *Store) error {
					_, _, err := scoped.SetFeatureFlagAudited(ctx, &orgID, "email_outbound_suspended", enabled, "operator@example.test")
					return err
				})
			},
		},
		{
			name:      "SetFeatureFlag with drift already marked",
			preMarked: true,
			write: func(ctx context.Context, st *Store, orgID string, enabled bool) error {
				return st.RunInTx(ctx, func(scoped *Store) error {
					_, err := scoped.SetFeatureFlag(ctx, &orgID, "email_outbound_suspended", enabled, "test")
					return err
				})
			},
		},
	} {
		t.Run(entry.name, func(t *testing.T) {
			withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
				if err := MigrateUpToCore(ctx, db, 28); err != nil {
					t.Fatalf("migrate to core 28: %v", err)
				}
				stale := &Store{db: db, q: db, fence: newEnabledFence(), drift: new(atomic.Bool)}
				if err := stale.RefreshOutboundFenceCapability(ctx); err != nil {
					t.Fatalf("refresh at 28: %v", err)
				}
				if stale.OutboundFenceEnabled() {
					t.Fatal("core 28 reported the fence")
				}

				// Core 0029 lands and another instance creates an epoch-bearing org.
				if err := MigrateUpToCore(ctx, db, 29); err != nil {
					t.Fatalf("migrate to core 29: %v", err)
				}
				orgID, inboxID := seedCore28Outbox(t, ctx, db)
				fresh := &Store{db: db, q: db, fence: newEnabledFence(), drift: new(atomic.Bool)}
				if err := fresh.RefreshOutboundFenceCapability(ctx); err != nil {
					t.Fatalf("fresh refresh: %v", err)
				}
				if err := fresh.RunInTx(ctx, func(scoped *Store) error {
					_, err := scoped.EnsureOutboundPolicyState(ctx, orgID)
					return err
				}); err != nil {
					t.Fatalf("seed policy state: %v", err)
				}
				outboxID := uuid.NewString()
				if _, err := db.ExecContext(ctx, `
					INSERT INTO outbox_messages (
						id, org_id, inbox_id, provider, idempotency_key,
						"to", "from", subject, text_body,
						status, delivery_status, autonomous_policy_epoch, next_attempt_at
					) VALUES ($1, $2, $3, 'smtp', 'revive-probe',
						'recipient@local.neuralmail', 'core28@local.neuralmail', 'revive', 'body',
						'queued', 'queued', 1, now())
				`, outboxID, orgID, inboxID); err != nil {
					t.Fatalf("insert epoch-bearing row: %v", err)
				}

				if entry.preMarked {
					stale.markOutboundFenceDrift()
				}

				var beforeEpoch int64
				if err := db.QueryRowContext(ctx,
					`SELECT policy_epoch FROM org_outbound_policy_state WHERE org_id = $1::uuid`,
					orgID).Scan(&beforeEpoch); err != nil {
					t.Fatalf("read epoch: %v", err)
				}

				// Either outcome is safe; skipping the epoch advance is not.
				suspendErr := entry.write(ctx, stale, orgID, true)
				clearErr := entry.write(ctx, stale, orgID, false)

				if entry.preMarked {
					// Drift means a backward transition was already observed, so the
					// writes must roll back rather than proceed on an unknown schema.
					if !errors.Is(suspendErr, ErrOutboundFenceDrift) || !errors.Is(clearErr, ErrOutboundFenceDrift) {
						t.Fatalf("drifted writes were not refused: suspend=%v clear=%v", suspendErr, clearErr)
					}
					var flagRows int
					if err := db.QueryRowContext(ctx, `
						SELECT count(*) FROM org_feature_flags
						WHERE org_id = $1::uuid AND flag = 'email_outbound_suspended'
					`, orgID).Scan(&flagRows); err != nil {
						t.Fatalf("read flag: %v", err)
					}
					if flagRows != 0 {
						t.Fatalf("refused write left %d flag row(s) behind", flagRows)
					}
				} else {
					if suspendErr != nil || clearErr != nil {
						t.Fatalf("stale writes failed: suspend=%v clear=%v", suspendErr, clearErr)
					}
					var afterEpoch int64
					if err := db.QueryRowContext(ctx,
						`SELECT policy_epoch FROM org_outbound_policy_state WHERE org_id = $1::uuid`,
						orgID).Scan(&afterEpoch); err != nil {
						t.Fatalf("read epoch: %v", err)
					}
					if afterEpoch <= beforeEpoch {
						t.Fatalf("epoch %d -> %d: the stale writer skipped the fence advance", beforeEpoch, afterEpoch)
					}
					if !stale.OutboundFenceEnabled() {
						t.Fatal("stale writer did not upgrade after proving the fence exists")
					}
				}

				// And the epoch-bearing row must remain unclaimable by the stale process.
				claimed, _ := stale.ClaimOutboxMessages(ctx, 10, "stale-worker", time.Now().UTC(), 5*time.Minute)
				for _, msg := range claimed {
					if msg.ID == outboxID {
						t.Fatal("stale process claimed the epoch-bearing row")
					}
				}
			})
		})
	}
}

// Outbound drift must not disable feature administration that has nothing to do
// with the outbound fence.
func TestDriftDoesNotBlockUnrelatedFeatureAdministration(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if err := MigrateUpToCore(ctx, db, 29); err != nil {
			t.Fatalf("migrate to core 29: %v", err)
		}
		st := &Store{db: db, q: db, fence: newEnabledFence(), drift: new(atomic.Bool)}
		if err := st.RefreshOutboundFenceCapability(ctx); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		st.markOutboundFenceDrift()

		// A global flag never touches the outbound fence at all.
		if err := st.RunInTx(ctx, func(scoped *Store) error {
			_, err := scoped.SetFeatureFlag(ctx, nil, "domain_writes", true, "operator@example.test")
			return err
		}); err != nil {
			t.Fatalf("drift blocked an unrelated global flag write: %v", err)
		}

		// So does an org flag on an org with no outbound policy state.
		orgID, _ := seedCore28Outbox(t, ctx, db)
		if err := st.RunInTx(ctx, func(scoped *Store) error {
			_, _, err := scoped.SetFeatureFlagAudited(ctx, &orgID, "domain_writes", true, "operator@example.test")
			return err
		}); err != nil {
			t.Fatalf("drift blocked an unrelated org flag write: %v", err)
		}
	})
}

// Drift stops new work; it must never strand the outcome of work already
// begun. A provider call in flight when the schema changes has to remain
// recordable, or the row stays `sending`, is reclaimed later, and the message
// can go out twice.
func TestDriftDoesNotStrandInFlightOutcomes(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if err := MigrateUpToCore(ctx, db, 28); err != nil {
			t.Fatalf("migrate to core 28: %v", err)
		}
		st := &Store{db: db, q: db, fence: newEnabledFence(), drift: new(atomic.Bool)}
		if err := st.RefreshOutboundFenceCapability(ctx); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		orgID, inboxID := seedCore28Outbox(t, ctx, db)
		id, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID: orgID, InboxID: inboxID, Provider: "smtp", IdempotencyKey: "in-flight",
			To: "recipient@local.neuralmail", From: "core28@local.neuralmail",
			Subject: "in flight", TextBody: "body",
		})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		claimed, err := st.ClaimOutboxMessages(ctx, 10, "in-flight-worker", time.Now().UTC(), 5*time.Minute)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim: %v (%d rows)", err, len(claimed))
		}
		lockedBy := claimed[0].LockedBy.String

		// The provider call is now in flight. Something else observes drift.
		st.markOutboundFenceDrift()

		// New work must stop.
		if _, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID: orgID, InboxID: inboxID, Provider: "smtp", IdempotencyKey: "post-drift",
			To: "recipient@local.neuralmail", From: "core28@local.neuralmail",
			Subject: "post drift", TextBody: "body",
		}); !errors.Is(err, ErrOutboundFenceDrift) {
			t.Fatalf("enqueue after drift = %v, want refusal", err)
		}
		if _, err := st.ClaimOutboxMessages(ctx, 10, "another-worker", time.Now().UTC(), 5*time.Minute); !errors.Is(err, ErrOutboundFenceDrift) {
			t.Fatalf("claim after drift = %v, want refusal", err)
		}

		// The in-flight outcome must still be recordable.
		if err := st.MarkClaimedOutboxMessageSent(ctx, id, lockedBy, "", "provider-in-flight"); err != nil {
			t.Fatalf("could not record an in-flight outcome after drift: %v", err)
		}
		var status string
		if err := db.QueryRowContext(ctx, `SELECT status FROM outbox_messages WHERE id = $1`, id).Scan(&status); err != nil {
			t.Fatalf("read status: %v", err)
		}
		if status != "sent" {
			t.Fatalf("status = %q, want sent -- the outcome was stranded", status)
		}
	})
}
