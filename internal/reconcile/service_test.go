package reconcile

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

	"neuralmail/internal/store"
)

func TestRunRepairsUsageDrift(t *testing.T) {
	withTempStore(t, func(ctx context.Context, st *store.Store) {
		orgID := uuid.NewString()
		now := time.Date(2026, 2, 7, 12, 0, 0, 0, time.UTC)
		start := now.Add(-24 * time.Hour)
		end := now.Add(24 * time.Hour)

		insertOrgAndEntitlement(t, ctx, st, orgID, start, end)
		if _, err := st.DB().ExecContext(ctx, `
			INSERT INTO org_usage_counters (org_id, meter_name, period_start, period_end, used)
			VALUES ($1, 'mcp_units', $2, $3, 10)
		`, orgID, start, end); err != nil {
			t.Fatalf("insert usage counter: %v", err)
		}
		if _, err := st.DB().ExecContext(ctx, `
			INSERT INTO usage_events (org_id, meter_name, quantity, tool_name, status, created_at)
			VALUES
			  ($1, 'mcp_units', 3, 'list_threads', 'success', $2),
			  ($1, 'mcp_units', 1, 'get_thread', 'success', $2),
			  ($1, 'mcp_units', 9, 'send_reply', 'failed', $2)
		`, orgID, now); err != nil {
			t.Fatalf("insert usage events: %v", err)
		}

		svc := NewService(st)
		svc.Now = func() time.Time { return now }
		report, err := svc.Run(ctx)
		if err != nil {
			t.Fatalf("run reconciliation: %v", err)
		}
		if report.CountersRepaired != 1 {
			t.Fatalf("expected 1 repaired counter, got %d", report.CountersRepaired)
		}

		used, err := st.GetOrgUsageCounterUsed(ctx, orgID, "mcp_units", start)
		if err != nil {
			t.Fatalf("query repaired usage: %v", err)
		}
		if used != 4 {
			t.Fatalf("expected repaired usage=4, got %d", used)
		}
	})
}

func TestRunBackstopRolloverCreatesNewPeriodCounter(t *testing.T) {
	withTempStore(t, func(ctx context.Context, st *store.Store) {
		orgID := uuid.NewString()
		now := time.Date(2026, 2, 7, 12, 0, 0, 0, time.UTC)
		start := now.Add(-60 * 24 * time.Hour)
		end := now.Add(-30 * 24 * time.Hour)

		insertOrgAndEntitlement(t, ctx, st, orgID, start, end)
		if _, err := st.DB().ExecContext(ctx, `
			INSERT INTO org_usage_counters (org_id, meter_name, period_start, period_end, used)
			VALUES ($1, 'mcp_units', $2, $3, 0)
		`, orgID, start, end); err != nil {
			t.Fatalf("insert old usage counter: %v", err)
		}

		svc := NewService(st)
		svc.Now = func() time.Time { return now }
		report, err := svc.Run(ctx)
		if err != nil {
			t.Fatalf("run reconciliation: %v", err)
		}
		if report.PeriodsRolled != 1 {
			t.Fatalf("expected 1 rolled period, got %d", report.PeriodsRolled)
		}

		ent, err := st.GetOrgEntitlement(ctx, orgID)
		if err != nil {
			t.Fatalf("query rolled entitlement: %v", err)
		}
		if !ent.UsagePeriodEnd.After(now) && !ent.UsagePeriodEnd.Equal(now) {
			t.Fatalf("expected rolled usage period end >= now, got %s", ent.UsagePeriodEnd)
		}
		if _, err := st.GetOrgUsageCounterUsed(ctx, orgID, "mcp_units", ent.UsagePeriodStart); err != nil {
			t.Fatalf("expected counter for rolled period start: %v", err)
		}
	})
}

func TestRunRepairsPendingOrgEventFanOut(t *testing.T) {
	withTempStore(t, func(ctx context.Context, st *store.Store) {
		orgID := uuid.NewString()
		eventID := uuid.NewString()
		now := time.Date(2026, 2, 7, 12, 0, 0, 0, time.UTC)
		if _, err := st.DB().ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'reconcile-events')`, orgID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateOrgWebhook(ctx, orgID, "https://events.example.com", []string{"email.received"}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().ExecContext(ctx, `
			INSERT INTO org_events (id, org_id, event_type, ref_kind, ref_id, payload, created_at)
			VALUES ($1, $2, 'email.received', 'message', $3, '{}', $4)
		`, eventID, orgID, uuid.NewString(), now.Add(-10*time.Minute)); err != nil {
			t.Fatal(err)
		}

		svc := NewService(st)
		svc.Now = func() time.Time { return now }
		report, err := svc.Run(ctx)
		if err != nil {
			t.Fatalf("run reconciliation: %v", err)
		}
		if report.OrgEventsFannedOut != 1 {
			t.Fatalf("org events fanned out=%d, want 1", report.OrgEventsFannedOut)
		}
		var fannedOut bool
		if err := st.DB().QueryRowContext(ctx, `SELECT fanned_out_at IS NOT NULL FROM org_events WHERE id = $1`, eventID).Scan(&fannedOut); err != nil {
			t.Fatal(err)
		}
		if !fannedOut {
			t.Fatal("reconciler did not stamp pending org event")
		}
		var deliveries int
		if err := st.DB().QueryRowContext(ctx, `SELECT count(*) FROM org_webhook_deliveries WHERE org_event_id = $1`, eventID).Scan(&deliveries); err != nil {
			t.Fatal(err)
		}
		if deliveries != 1 {
			t.Fatalf("deliveries=%d, want 1", deliveries)
		}
	})
}

func TestRunAtSchema19SkipsUnavailableOrgEventJournal(t *testing.T) {
	withTempStore(t, func(ctx context.Context, st *store.Store) {
		for {
			version, err := store.CurrentVersionCore(ctx, st.DB())
			if err != nil {
				t.Fatal(err)
			}
			if version <= 19 {
				break
			}
			if err := store.MigrateDownCore(ctx, st.DB()); err != nil {
				t.Fatalf("down from schema %d: %v", version, err)
			}
		}
		version, err := store.CurrentVersionCore(ctx, st.DB())
		if err != nil || version != 19 {
			t.Fatalf("core version=%d err=%v, want 19", version, err)
		}

		report, err := NewService(st).Run(ctx)
		if err != nil {
			t.Fatalf("run reconciliation at schema 19: %v", err)
		}
		if report.OrgEventsFannedOut != 0 {
			t.Fatalf("org events fanned out=%d, want 0 without journal schema", report.OrgEventsFannedOut)
		}
		if report.AttachmentUsageSeeded != 0 {
			t.Fatalf("attachment usage seeded=%d, want 0 without attachment schema", report.AttachmentUsageSeeded)
		}
	})
}

func TestRunSeedsMissingAttachmentUsage(t *testing.T) {
	withTempStore(t, func(ctx context.Context, st *store.Store) {
		orgID := uuid.NewString()
		if _, err := st.DB().ExecContext(ctx, `
			INSERT INTO orgs (id, name) VALUES ($1, 'missing-attachment-usage')
		`, orgID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().ExecContext(ctx, `
			INSERT INTO attachment_blobs
			  (org_id, sha256, size_bytes, content_type, content)
			VALUES ($1, 'preexisting', 3, 'application/octet-stream', '\x010203')
		`, orgID); err != nil {
			t.Fatal(err)
		}

		report, err := NewService(st).Run(ctx)
		if err != nil {
			t.Fatalf("run reconciliation: %v", err)
		}
		if report.AttachmentUsageSeeded != 1 {
			t.Fatalf("attachment usage seeded=%d, want 1", report.AttachmentUsageSeeded)
		}
		var rows int
		var bytesUsed int64
		if err := st.DB().QueryRowContext(ctx, `
			SELECT count(*), COALESCE(max(bytes_used), 0)
			FROM org_attachment_usage WHERE org_id = $1
		`, orgID).Scan(&rows, &bytesUsed); err != nil {
			t.Fatal(err)
		}
		if rows != 1 || bytesUsed != 3 {
			t.Fatalf("usage rows=%d bytes_used=%d, want 1/3", rows, bytesUsed)
		}
	})
}

func TestRunReleasesSentOutboxAttachmentsThenGarbageCollectsAfterGrace(t *testing.T) {
	withTempStore(t, func(ctx context.Context, st *store.Store) {
		now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
		orgID, err := st.CreateOrg(ctx, "outbox-retention-reconcile")
		if err != nil {
			t.Fatal(err)
		}
		inboxID := uuid.NewString()
		if _, err := st.DB().ExecContext(ctx, `
			INSERT INTO inboxes (id, org_id, address, status)
			VALUES ($1, $2, 'retention-reconcile@local.nerve.email', 'active')
		`, inboxID, orgID); err != nil {
			t.Fatal(err)
		}
		content := []byte("old retained bytes")
		outboxID, err := st.EnqueueOutboxMessage(ctx, store.OutboxMessage{
			OrgID: orgID, InboxID: inboxID, Provider: "smtp",
			IdempotencyKey: "retention-reconcile-key",
			To:             "to@example.com", From: "from@example.com", Subject: "retention", TextBody: "body",
			Attachments: []store.OutboundAttachment{{Filename: "old.txt", ContentType: "text/plain", Content: content}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.MarkOutboxMessageSent(ctx, outboxID, "provider-id"); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().ExecContext(ctx, `
			UPDATE outbox_messages SET terminal_at = $2 WHERE id = $1
		`, outboxID, now.Add(-91*24*time.Hour)); err != nil {
			t.Fatal(err)
		}

		svc := NewService(st)
		svc.Now = func() time.Time { return now }
		report, err := svc.Run(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if report.OutboxAttachmentsReleased != 1 || report.AttachmentBlobsDeleted != 0 || report.AttachmentBytesReleased != 0 {
			t.Fatalf("release report=%+v", report)
		}

		if _, err := st.DB().ExecContext(ctx, `
			UPDATE attachment_blobs SET last_ref_at = $2 WHERE org_id = $1
		`, orgID, now.Add(-8*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().ExecContext(ctx, `
			UPDATE org_attachment_usage SET bytes_used = 999 WHERE org_id = $1
		`, orgID); err != nil {
			t.Fatal(err)
		}
		report, err = svc.Run(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if report.OutboxAttachmentsReleased != 0 || report.AttachmentBlobsDeleted != 1 || report.AttachmentBytesReleased != int64(len(content)) || report.AttachmentUsageRepaired != 1 {
			t.Fatalf("GC report=%+v", report)
		}
		var blobs int
		var bytesUsed int64
		if err := st.DB().QueryRowContext(ctx, `SELECT count(*) FROM attachment_blobs WHERE org_id = $1`, orgID).Scan(&blobs); err != nil {
			t.Fatal(err)
		}
		if err := st.DB().QueryRowContext(ctx, `SELECT bytes_used FROM org_attachment_usage WHERE org_id = $1`, orgID).Scan(&bytesUsed); err != nil {
			t.Fatal(err)
		}
		if blobs != 0 || bytesUsed != 0 {
			t.Fatalf("post-reconcile blobs=%d bytes_used=%d", blobs, bytesUsed)
		}
	})
}

func insertOrgAndEntitlement(t *testing.T, ctx context.Context, st *store.Store, orgID string, periodStart, periodEnd time.Time) {
	t.Helper()
	if _, err := st.DB().ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'reconcile-org')`, orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `
		INSERT INTO org_entitlements (
			org_id, plan_code, subscription_status, mcp_rpm, monthly_units, max_inboxes,
			usage_period_start, usage_period_end
		) VALUES ($1, 'pro', 'active', 1000, 100, 10, $2, $3)
	`, orgID, periodStart, periodEnd); err != nil {
		t.Fatalf("insert org entitlement: %v", err)
	}
}

func withTempStore(t *testing.T, run func(ctx context.Context, st *store.Store)) {
	t.Helper()

	baseDSN := os.Getenv("NM_TEST_DB_DSN")
	if baseDSN == "" {
		baseDSN = "postgres://neuralmail:neuralmail@127.0.0.1:54320/neuralmail?sslmode=disable"
	}
	adminDSN, err := dsnWithDatabase(baseDSN, "postgres")
	if err != nil {
		t.Fatalf("build admin dsn: %v", err)
	}
	adminDB, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatalf("open admin db: %v", err)
	}
	t.Cleanup(func() { _ = adminDB.Close() })

	pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := adminDB.PingContext(pingCtx); err != nil {
		t.Skipf("postgres unavailable for reconcile tests: %v", err)
	}

	dbName := "nerve_rec_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := adminDB.ExecContext(context.Background(), fmt.Sprintf(`CREATE DATABASE %s`, dbName)); err != nil {
		t.Fatalf("create test db: %v", err)
	}
	testDSN, err := dsnWithDatabase(baseDSN, dbName)
	if err != nil {
		t.Fatalf("build test dsn: %v", err)
	}
	st, err := store.Open(testDSN)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	if err := store.MigrateAll(context.Background(), st.DB()); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	t.Cleanup(func() {
		_, _ = adminDB.ExecContext(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, dbName)
		_, _ = adminDB.ExecContext(context.Background(), fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, dbName))
	})

	run(context.Background(), st)
}

func dsnWithDatabase(rawDSN, dbName string) (string, error) {
	parsed, err := url.Parse(rawDSN)
	if err != nil {
		return "", err
	}
	parsed.Path = "/" + dbName
	return parsed.String(), nil
}
