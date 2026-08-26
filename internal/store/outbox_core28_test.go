package store

import (
	"context"
	"database/sql"
	"strings"
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
		if err != nil {
			// Refusing is acceptable; silently claiming it as legacy is not.
			t.Logf("stale consumer refused: %v", err)
		}
		for _, msg := range claimed {
			if msg.ID == outboxID {
				t.Fatal("stale consumer claimed a fenced row as legacy: the policy predicate was bypassed")
			}
		}
		if !stale.OutboundFenceEnabled() {
			t.Fatal("stale consumer did not revalidate against the live core 29 schema")
		}
	})
}

// The inverse: a stale fenced instance after a clean rollback issues fenced SQL
// against removed objects. That is an outage rather than a bypass, so it may
// fail -- but it must re-detect and recover instead of failing forever.
func TestStaleFencedStoreRecoversAfterRollback(t *testing.T) {
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
		if st.OutboundFenceEnabled() {
			t.Fatal("store still believes the fence exists after a clean rollback")
		}

		// Having re-detected, it must work.
		id, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID: orgID, InboxID: inboxID, Provider: "smtp", IdempotencyKey: "post-rollback-2",
			To: "recipient@local.neuralmail", From: "core28@local.neuralmail",
			Subject: "post rollback", TextBody: "body",
		})
		if err != nil {
			t.Fatalf("enqueue after re-detection: %v", err)
		}
		if _, err := st.ClaimOutboxMessages(ctx, 10, "recovered-worker", time.Now().UTC(), 5*time.Minute); err != nil {
			t.Fatalf("claim after re-detection: %v", err)
		}
		if id == "" {
			t.Fatal("enqueue returned no id")
		}
	})
}
