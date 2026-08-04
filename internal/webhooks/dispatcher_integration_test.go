package webhooks

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"neuralmail/internal/store"
)

// TestDispatcher_EndToEnd exercises the full customer webhook flow:
//  1. Spin up a real HTTP test server that verifies the HMAC signature.
//  2. Seed an outbox row + outbox_event + org_webhook in real Postgres.
//  3. Fan out the event to the subscription.
//  4. Run one claim cycle of the dispatcher.
//  5. Assert the delivery marked 'delivered' with status_code 200.
func TestDispatcher_EndToEnd(t *testing.T) {
	baseDSN := os.Getenv("NM_TEST_DB_DSN")
	if baseDSN == "" {
		baseDSN = "postgres://neuralmail:neuralmail@127.0.0.1:54320/neuralmail?sslmode=disable"
	}
	adminDB, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = adminDB.Close() })
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer pingCancel()
	if err := adminDB.PingContext(pingCtx); err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}

	dbName := "nerve_wh_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := adminDB.ExecContext(context.Background(), "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create db: %v", err)
	}
	t.Cleanup(func() {
		_, _ = adminDB.ExecContext(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, dbName)
		_, _ = adminDB.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+dbName)
	})

	testDSN := strings.Replace(baseDSN, "/neuralmail?", "/"+dbName+"?", 1)
	st, err := store.Open(testDSN)
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	defer st.Close()
	db := st.DB()
	ctx := context.Background()
	if err := store.MigrateCore(ctx, db); err != nil {
		t.Fatalf("migrate core: %v", err)
	}
	if err := store.MigrateCloud(ctx, db); err != nil {
		t.Fatalf("migrate cloud: %v", err)
	}

	orgID := uuid.NewString()
	if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'acme')`, orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	inboxID := uuid.NewString()
	if _, err := db.ExecContext(ctx, `INSERT INTO inboxes (id, org_id, address, status) VALUES ($1, $2, 'a@local.neuralmail', 'active')`, inboxID, orgID); err != nil {
		t.Fatalf("insert inbox: %v", err)
	}

	// Fake customer webhook server — captures one request and verifies
	// the signature using SignPayload, then returns 200.
	var (
		captured     http.Header
		capturedBody []byte
		gotOnce      sync.Once
		done         = make(chan struct{})
	)
	webhookServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotOnce.Do(func() {
			captured = r.Header.Clone()
			capturedBody = body
			close(done)
		})
		w.WriteHeader(http.StatusOK)
	}))
	defer webhookServer.Close()

	// Seed outbox + event + subscription.
	outboxID, err := st.EnqueueOutboxMessage(ctx, store.OutboxMessage{
		OrgID: orgID, InboxID: inboxID, Provider: "smtp", IdempotencyKey: "e2e-1",
		To: "to@external.com", From: "a@local.neuralmail", Subject: "hi", TextBody: "body",
	})
	if err != nil {
		t.Fatalf("enqueue outbox: %v", err)
	}
	eventPayload := json.RawMessage(`{"email_id":"res-1","event":"delivered"}`)
	eventID, err := st.InsertOutboxEventReturningID(ctx, store.OutboxEvent{
		OrgID: orgID, OutboxMessageID: outboxID, EventType: "email.delivered",
		RawPayload: eventPayload,
	})
	if err != nil || eventID == "" {
		t.Fatalf("insert event: id=%q err=%v", eventID, err)
	}
	wh, err := st.CreateOrgWebhook(ctx, orgID, webhookServer.URL, nil)
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	if _, err := st.FanOutWebhookDeliveries(ctx, orgID, eventID, "email.delivered", eventPayload); err != nil {
		t.Fatalf("fan out: %v", err)
	}

	// Drive one cycle of the dispatcher directly (bypass Run so the
	// test finishes in well under one poll interval).
	dispatcher := NewDispatcher(st, "test-worker")
	dispatcher.HTTPClient = webhookServer.Client()
	dispatcher.claimAndDeliver(ctx)

	// Wait for the fake server to see the request.
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("webhook was not delivered within 3s")
	}

	// Assert the delivery row landed in 'delivered' state.
	var status string
	var statusCode sql.NullInt32
	row := db.QueryRowContext(ctx, `
		SELECT status, last_status_code FROM org_webhook_deliveries
		WHERE webhook_id = $1
	`, wh.ID)
	if err := row.Scan(&status, &statusCode); err != nil {
		t.Fatalf("scan delivery: %v", err)
	}
	if status != "delivered" {
		t.Errorf("expected status=delivered, got %q", status)
	}
	if !statusCode.Valid || statusCode.Int32 != 200 {
		t.Errorf("expected last_status_code=200, got %+v", statusCode)
	}

	// Assert the signature headers are present and valid.
	ts := captured.Get("X-Nerve-Timestamp")
	sig := captured.Get("X-Nerve-Signature")
	if ts == "" {
		t.Error("missing X-Nerve-Timestamp header")
	}
	if sig == "" {
		t.Error("missing X-Nerve-Signature header")
	}
	// Signature format is "t=<ts>,v1=<hex>". Pull out the hex part.
	var hexSig string
	for _, part := range strings.Split(sig, ",") {
		if strings.HasPrefix(part, "v1=") {
			hexSig = strings.TrimPrefix(part, "v1=")
			break
		}
	}
	wantSig := SignPayload(wh.Secret, ts, capturedBody)
	if hexSig == "" || hexSig != wantSig {
		t.Errorf("signature mismatch\ngot:  %s\nwant: %s", hexSig, wantSig)
	}
}
