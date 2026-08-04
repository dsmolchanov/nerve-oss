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

// TestDispatcher_EndToEnd exercises the sensitive inbound webhook flow:
//  1. Spin up a real HTTP test server that verifies the HMAC signature.
//  2. Journal email.received + fan out in real Postgres.
//  3. Return 503 once, then ACK the signed retry.
//  4. Assert the delivery is deduplicated and marked delivered.
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
	// Fake customer webhook server — first response is retryable, second ACKs.
	var (
		captured     http.Header
		capturedBody []byte
		captureMu    sync.Mutex
		attempts     int
	)
	webhookServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captureMu.Lock()
		attempts++
		captured = r.Header.Clone()
		capturedBody = body
		currentAttempt := attempts
		captureMu.Unlock()
		if currentAttempt == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer webhookServer.Close()

	wh, err := st.CreateOrgWebhook(ctx, orgID, webhookServer.URL, []string{"email.received"})
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	messageID := uuid.NewString()
	eventPayload := json.RawMessage(`{"event":"email.received","message_id":"` + messageID + `"}`)
	eventID, deliveryCount, err := st.InsertOrgEventAndFanOut(ctx, orgID, "email.received", "message", messageID, eventPayload)
	if err != nil || eventID == "" || deliveryCount != 1 {
		t.Fatalf("insert inbound event: id=%q deliveries=%d err=%v", eventID, deliveryCount, err)
	}

	// Drive one cycle of the dispatcher directly (bypass Run so the
	// test finishes in well under one poll interval).
	dispatcher := NewDispatcher(st, "test-worker")
	dispatcher.HTTPClient = webhookServer.Client()
	dispatcher.BaseBackoff = time.Millisecond
	dispatcher.claimAndDeliver(ctx)
	if _, err := db.ExecContext(ctx, `UPDATE org_webhook_deliveries SET next_attempt_at = now() - interval '1 second' WHERE webhook_id = $1`, wh.ID); err != nil {
		t.Fatalf("make retry claimable: %v", err)
	}
	dispatcher.claimAndDeliver(ctx)
	captureMu.Lock()
	gotAttempts := attempts
	captureMu.Unlock()
	if gotAttempts != 2 {
		t.Fatalf("webhook attempts = %d, want retry then ACK", gotAttempts)
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
	var storedDeliveries int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM org_webhook_deliveries WHERE org_event_id = $1`, eventID).Scan(&storedDeliveries); err != nil {
		t.Fatalf("count inbound deliveries: %v", err)
	}
	if storedDeliveries != 1 {
		t.Fatalf("inbound delivery rows = %d, want 1", storedDeliveries)
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
