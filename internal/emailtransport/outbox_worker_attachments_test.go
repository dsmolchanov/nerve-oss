package emailtransport

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"neuralmail/internal/memguard"
	"neuralmail/internal/store"
)

type capturedOutbound struct {
	idempotencyKey string
	message        OutboundMessage
}

type captureOutboundAdapter struct {
	deliveries []capturedOutbound
	onSend     func()
}

func (a *captureOutboundAdapter) Name() string { return "capture" }

func (a *captureOutboundAdapter) SendMessage(_ context.Context, message OutboundMessage, idempotencyKey string) (string, error) {
	if a.onSend != nil {
		a.onSend()
	}
	copyMessage := message
	copyMessage.Attachments = make([]store.OutboundAttachment, len(message.Attachments))
	for index, attachment := range message.Attachments {
		copyMessage.Attachments[index] = attachment
		copyMessage.Attachments[index].Content = append([]byte(nil), attachment.Content...)
	}
	a.deliveries = append(a.deliveries, capturedOutbound{idempotencyKey: idempotencyKey, message: copyMessage})
	return "provider-" + idempotencyKey, nil
}

func TestOutboxWorkerReservesSharedMemoryBeforeBlobLoad(t *testing.T) {
	withOutboxWorkerDatabase(t, func(ctx context.Context, db *sql.DB, st *store.Store) {
		orgID := uuid.NewString()
		inboxID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'worker-memory')`, orgID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO inboxes (id, org_id, address, status)
			VALUES ($1, $2, 'worker-memory@local.neuralmail', 'active')
		`, inboxID, orgID); err != nil {
			t.Fatal(err)
		}
		content := []byte("reserved bytes")
		outboxID, err := st.EnqueueOutboxMessage(ctx, workerAttachmentMessage(orgID, inboxID, "memory-held", []store.OutboundAttachment{{
			Filename: "memory.txt", ContentType: "text/plain", Content: content,
		}}))
		if err != nil {
			t.Fatal(err)
		}

		budget, err := memguard.New(int64(len(content)))
		if err != nil {
			t.Fatal(err)
		}
		adapter := &captureOutboundAdapter{}
		adapter.onSend = func() {
			if used := budget.Used(); used != int64(len(content)) {
				t.Fatalf("budget used during provider call=%d, want %d", used, len(content))
			}
		}
		registry := NewRegistry()
		if err := registry.RegisterOutbound(adapter); err != nil {
			t.Fatal(err)
		}
		worker := NewOutboxWorker(st, registry, "memory-held-test")
		worker.MemoryBudget = budget
		worker.ClaimLimit = 1
		worker.claimAndDeliver(ctx)

		if len(adapter.deliveries) != 1 {
			t.Fatalf("deliveries=%d, want 1", len(adapter.deliveries))
		}
		if budget.Used() != 0 {
			t.Fatalf("budget leaked %d bytes after delivery", budget.Used())
		}
		var status string
		if err := db.QueryRowContext(ctx, `SELECT status FROM outbox_messages WHERE id = $1`, outboxID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "sent" {
			t.Fatalf("status=%q, want sent", status)
		}
	})
}

func TestOutboxWorkerRequeuesBeforeBlobLoadWhenMemoryExhausted(t *testing.T) {
	withOutboxWorkerDatabase(t, func(ctx context.Context, db *sql.DB, st *store.Store) {
		orgID := uuid.NewString()
		inboxID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'worker-memory-exhausted')`, orgID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO inboxes (id, org_id, address, status)
			VALUES ($1, $2, 'worker-memory-exhausted@local.neuralmail', 'active')
		`, inboxID, orgID); err != nil {
			t.Fatal(err)
		}
		outboxID, err := st.EnqueueOutboxMessage(ctx, workerAttachmentMessage(orgID, inboxID, "memory-exhausted", []store.OutboundAttachment{{
			Filename: "large.txt", ContentType: "text/plain", Content: []byte("larger than budget"),
		}}))
		if err != nil {
			t.Fatal(err)
		}

		budget, err := memguard.New(1)
		if err != nil {
			t.Fatal(err)
		}
		adapter := &captureOutboundAdapter{}
		registry := NewRegistry()
		if err := registry.RegisterOutbound(adapter); err != nil {
			t.Fatal(err)
		}
		worker := NewOutboxWorker(st, registry, "memory-exhausted-test")
		worker.MemoryBudget = budget
		worker.ClaimLimit = 1
		worker.BaseBackoff = time.Millisecond
		worker.claimAndDeliver(ctx)

		if len(adapter.deliveries) != 0 {
			t.Fatalf("provider called %d times despite exhausted budget", len(adapter.deliveries))
		}
		if budget.Used() != 0 {
			t.Fatalf("exhausted reservation changed budget usage to %d", budget.Used())
		}
		var status string
		var lastError sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT status, last_error FROM outbox_messages WHERE id = $1`, outboxID).Scan(&status, &lastError); err != nil {
			t.Fatal(err)
		}
		if status != "queued" || !lastError.Valid || !strings.Contains(lastError.String, "memory budget exhausted") {
			t.Fatalf("status=%q last_error=%q, want queued budget exhaustion", status, lastError.String)
		}
	})
}

func (a *captureOutboundAdapter) GetDeliveryStatus(context.Context, string) (DeliveryStatus, error) {
	return DeliveryStatusUnknown, ErrNotSupported
}

func TestOutboxWorkerAttachmentRoundTripAndReleasedFailure(t *testing.T) {
	withOutboxWorkerDatabase(t, func(ctx context.Context, db *sql.DB, st *store.Store) {
		orgID := uuid.NewString()
		inboxID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'worker-attachments')`, orgID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO inboxes (id, org_id, address, status)
			VALUES ($1, $2, 'worker@local.neuralmail', 'active')
		`, inboxID, orgID); err != nil {
			t.Fatal(err)
		}

		attachments := []store.OutboundAttachment{
			{Filename: "first.txt", ContentType: "text/plain", Content: []byte("first bytes")},
			{Filename: "report.pdf", ContentType: "application/pdf", Content: []byte{0x25, 0x50, 0x44, 0x46}},
		}
		deliverID, err := st.EnqueueOutboxMessage(ctx, workerAttachmentMessage(orgID, inboxID, "deliver-two", attachments))
		if err != nil {
			t.Fatal(err)
		}

		releasedID, err := st.EnqueueOutboxMessage(ctx, workerAttachmentMessage(orgID, inboxID, "released", []store.OutboundAttachment{{
			Filename: "released.txt", ContentType: "text/plain", Content: []byte("released bytes"),
		}}))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `
			UPDATE outbox_attachments SET blob_sha256 = NULL WHERE outbox_message_id = $1
		`, releasedID); err != nil {
			t.Fatal(err)
		}

		adapter := &captureOutboundAdapter{}
		registry := NewRegistry()
		if err := registry.RegisterOutbound(adapter); err != nil {
			t.Fatal(err)
		}
		worker := NewOutboxWorker(st, registry, "attachment-test")
		worker.BaseBackoff = time.Millisecond
		worker.claimAndDeliver(ctx)

		if len(adapter.deliveries) != 1 {
			t.Fatalf("adapter deliveries=%d, want only the available message", len(adapter.deliveries))
		}
		delivery := adapter.deliveries[0]
		if delivery.idempotencyKey != "deliver-two" {
			t.Fatalf("delivered idempotency key=%q", delivery.idempotencyKey)
		}
		if len(delivery.message.Attachments) != len(attachments) {
			t.Fatalf("delivered attachments=%d, want %d", len(delivery.message.Attachments), len(attachments))
		}
		for index, want := range attachments {
			got := delivery.message.Attachments[index]
			if got.Filename != want.Filename || got.ContentType != want.ContentType || string(got.Content) != string(want.Content) {
				t.Fatalf("attachment %d=%+v content=%x, want %+v content=%x", index, got, got.Content, want, want.Content)
			}
		}

		for id, wantStatus := range map[string]string{deliverID: "sent", releasedID: "failed"} {
			var status string
			var lastError sql.NullString
			if err := db.QueryRowContext(ctx, `SELECT status, last_error FROM outbox_messages WHERE id = $1`, id).Scan(&status, &lastError); err != nil {
				t.Fatal(err)
			}
			if status != wantStatus {
				t.Fatalf("outbox %s status=%q, want %q (last_error=%q)", id, status, wantStatus, lastError.String)
			}
			if id == releasedID && (!lastError.Valid || !strings.Contains(lastError.String, "attachments released")) {
				t.Fatalf("released row last_error=%q, want typed release failure", lastError.String)
			}
		}
	})
}

func workerAttachmentMessage(orgID, inboxID, idempotencyKey string, attachments []store.OutboundAttachment) store.OutboxMessage {
	return store.OutboxMessage{
		OrgID:          orgID,
		InboxID:        inboxID,
		Provider:       "capture",
		IdempotencyKey: idempotencyKey,
		To:             "recipient@example.com",
		From:           "worker@local.neuralmail",
		Subject:        "worker attachment test",
		TextBody:       "body",
		Attachments:    attachments,
	}
}

func withOutboxWorkerDatabase(t *testing.T, run func(context.Context, *sql.DB, *store.Store)) {
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
			t.Fatalf("postgres required for outbox worker test: %v", err)
		}
		t.Skipf("postgres unavailable for outbox worker test: %v", err)
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
	if err := store.MigrateCore(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	run(context.Background(), db, st)
}

func outboxWorkerDSNWithDatabase(rawDSN, databaseName string) (string, error) {
	parsed, err := url.Parse(rawDSN)
	if err != nil {
		return "", err
	}
	parsed.Path = "/" + databaseName
	return parsed.String(), nil
}
