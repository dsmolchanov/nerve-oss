package emailtransport

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"neuralmail/internal/memguard"
	"neuralmail/internal/store"
)

// withCore28WorkerDatabase brings a scratch database up to Core 28 only, the
// predecessor half of Artifact B's [28,29] window.
func withCore28WorkerDatabase(t *testing.T, run func(context.Context, *sql.DB, *store.Store)) {
	t.Helper()
	baseDSN := os.Getenv("NM_TEST_DB_DSN")
	if baseDSN == "" {
		baseDSN = "postgres://neuralmail:neuralmail@127.0.0.1:54320/neuralmail?sslmode=disable"
	}
	adminDSN, err := outboxWorkerDSNWithDatabase(baseDSN, "postgres")
	if err != nil {
		t.Fatal(err)
	}
	adminDB, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adminDB.Close() })

	pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := adminDB.PingContext(pingCtx); err != nil {
		if os.Getenv("NM_REQUIRE_DB") == "1" {
			t.Fatalf("postgres required for core 28 worker test: %v", err)
		}
		t.Skipf("postgres unavailable for core 28 worker test: %v", err)
	}

	databaseName := "nerve_worker_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := adminDB.ExecContext(context.Background(), fmt.Sprintf(`CREATE DATABASE %s`, databaseName)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = adminDB.ExecContext(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, databaseName)
		_, _ = adminDB.ExecContext(context.Background(), fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, databaseName))
	})

	testDSN, err := outboxWorkerDSNWithDatabase(baseDSN, databaseName)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", testDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.MigrateUpToCore(context.Background(), db, 28); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.RefreshOutboundFenceCapability(context.Background()); err != nil {
		t.Fatal(err)
	}
	run(context.Background(), db, st)
}

// The worker calls BeginOutboxProviderOperationState for every claimed message.
// On Core 28 every row is legacy, so a fence guard placed ahead of the legacy
// fast path refuses each one and the worker requeues it without ever reaching
// the provider — Artifact B could not deliver mail on the Core 28 half of its
// window. This drives the real worker end to end rather than marking the row
// sent by hand, which is what let that regression through.
func TestOutboxWorkerDeliversLegacyMessageOnCore28(t *testing.T) {
	withCore28WorkerDatabase(t, func(ctx context.Context, db *sql.DB, st *store.Store) {
		if st.OutboundFenceEnabled() {
			t.Fatal("core 28 reported the 0029 fence as available")
		}
		orgID := uuid.NewString()
		inboxID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'core28-worker')`, orgID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO inboxes (id, org_id, address, status)
			VALUES ($1, $2, 'core28-worker@local.neuralmail', 'active')
		`, inboxID, orgID); err != nil {
			t.Fatal(err)
		}

		outboxID, err := st.EnqueueOutboxMessage(ctx, store.OutboxMessage{
			OrgID: orgID, InboxID: inboxID, Provider: "capture", IdempotencyKey: "core28-worker-send",
			To: "recipient@local.neuralmail", From: "core28-worker@local.neuralmail",
			Subject: "core28 worker", TextBody: "delivered on core 28",
		})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		claimed, err := st.ClaimOutboxMessages(ctx, 10, "core28-worker", time.Now().UTC(), 5*time.Minute)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if len(claimed) != 1 || claimed[0].ID != outboxID {
			t.Fatalf("claim returned %+v, want the enqueued row", claimed)
		}

		adapter := &captureOutboundAdapter{}
		registry := NewRegistry()
		if err := registry.RegisterOutbound(adapter); err != nil {
			t.Fatal(err)
		}
		budget, err := memguard.New(64 << 20)
		if err != nil {
			t.Fatal(err)
		}
		worker := NewOutboxWorker(st, registry, "core28-worker", budget)
		if err := worker.deliverOne(ctx, claimed[0]); err != nil {
			t.Fatalf("deliver on core 28: %v", err)
		}
		if len(adapter.deliveries) != 1 {
			t.Fatalf("provider called %d times, want 1 — the fence refused a legacy send", len(adapter.deliveries))
		}

		var status string
		if err := db.QueryRowContext(ctx, `SELECT status FROM outbox_messages WHERE id = $1`, outboxID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "sent" {
			t.Fatalf("status = %q, want sent", status)
		}
	})
}
