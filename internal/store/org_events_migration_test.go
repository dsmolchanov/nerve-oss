package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMigration20IsExpandOnly(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if err := MigrateUpToCore(ctx, db, 19); err != nil {
			t.Fatal(err)
		}
		if err := MigrateUpToCore(ctx, db, 20); err != nil {
			t.Fatal(err)
		}
		version, err := CurrentVersionCore(ctx, db)
		if err != nil || version != 20 {
			t.Fatalf("version=%d err=%v", version, err)
		}
		assertTableExists(t, db, "org_events")
		assertColumnExists(t, db, "org_webhook_deliveries", "org_event_id")
		assertColumnNotNull(t, db, "org_webhook_deliveries", "outbox_event_id")

		if err := MigrateDownCore(ctx, db); err != nil {
			t.Fatalf("clean migration 20 down: %v", err)
		}
		version, err = CurrentVersionCore(ctx, db)
		if err != nil || version != 19 {
			t.Fatalf("version after down=%d err=%v", version, err)
		}
	})
}

func TestMigration20DownRefusesJournalRowsWithoutDeliveries(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if err := MigrateUpToCore(ctx, db, 20); err != nil {
			t.Fatal(err)
		}
		orgID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'migration-20-refuse')`, orgID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO org_events (org_id, event_type, ref_kind, ref_id, payload, fanned_out_at)
			VALUES ($1, 'email.received', 'message', $2, '{}', now())
		`, orgID, uuid.NewString()); err != nil {
			t.Fatal(err)
		}

		err := MigrateDownCore(ctx, db)
		if err == nil || !strings.Contains(err.Error(), "cannot roll back core migration 0020") {
			t.Fatalf("down error=%v, want journal refusal", err)
		}
		version, versionErr := CurrentVersionCore(ctx, db)
		if versionErr != nil || version != 20 {
			t.Fatalf("version=%d err=%v after refused down", version, versionErr)
		}
	})
}

func TestMigration21RelaxesSourceAndDisablesCleartextInbound(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if err := MigrateUpToCore(ctx, db, 20); err != nil {
			t.Fatal(err)
		}
		orgID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'migration-21')`, orgID); err != nil {
			t.Fatal(err)
		}
		type seed struct {
			url    string
			events string
		}
		for _, webhook := range []seed{
			{url: "http://inbound.example.com", events: `{email.received}`},
			{url: "http://outbound.example.com", events: `{email.delivered}`},
			{url: "https://inbound.example.com", events: `{email.received}`},
			{url: "HTTPS://uppercase.example.com", events: `{email.received}`},
		} {
			if _, err := db.ExecContext(ctx, `
				INSERT INTO org_webhooks (org_id, url, secret, events)
				VALUES ($1, $2, 'secret', $3::text[])
			`, orgID, webhook.url, webhook.events); err != nil {
				t.Fatal(err)
			}
		}

		if err := MigrateUpToCore(ctx, db, 21); err != nil {
			t.Fatal(err)
		}
		var nullable string
		if err := db.QueryRowContext(ctx, `
			SELECT is_nullable FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = 'org_webhook_deliveries' AND column_name = 'outbox_event_id'
		`).Scan(&nullable); err != nil {
			t.Fatal(err)
		}
		if nullable != "YES" {
			t.Fatalf("outbox_event_id nullable=%s, want YES", nullable)
		}
		for _, check := range []struct {
			url          string
			wantDisabled bool
		}{
			{url: "http://inbound.example.com", wantDisabled: true},
			{url: "http://outbound.example.com", wantDisabled: false},
			{url: "https://inbound.example.com", wantDisabled: false},
			{url: "HTTPS://uppercase.example.com", wantDisabled: false},
		} {
			var disabled sql.NullTime
			if err := db.QueryRowContext(ctx, `SELECT disabled_at FROM org_webhooks WHERE url = $1`, check.url).Scan(&disabled); err != nil {
				t.Fatal(err)
			}
			if disabled.Valid != check.wantDisabled {
				t.Fatalf("url=%s disabled=%v want=%v", check.url, disabled.Valid, check.wantDisabled)
			}
		}

		if err := MigrateDownCore(ctx, db); err != nil {
			t.Fatalf("clean migration 21 down: %v", err)
		}
		assertColumnNotNull(t, db, "org_webhook_deliveries", "outbox_event_id")
	})
}

func TestMigration21DownRefusesOrgEventDeliveries(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		orgID := uuid.NewString()
		webhookID := uuid.NewString()
		eventID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'migration-21-refuse')`, orgID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO org_webhooks (id, org_id, url, secret, events) VALUES ($1, $2, 'https://example.com', 'secret', '{email.received}')`, webhookID, orgID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO org_events (id, org_id, event_type, ref_kind, ref_id, payload) VALUES ($1, $2, 'email.received', 'message', $3, '{}')`, eventID, orgID, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO org_webhook_deliveries (org_id, webhook_id, org_event_id, event_type, payload) VALUES ($1, $2, $3, 'email.received', '{}')`, orgID, webhookID, eventID); err != nil {
			t.Fatal(err)
		}

		err := MigrateDownCore(ctx, db)
		if err == nil || !strings.Contains(err.Error(), "cannot roll back core migration 0021") {
			t.Fatalf("down error=%v, want org event refusal", err)
		}
		version, versionErr := CurrentVersionCore(ctx, db)
		if versionErr != nil || version != 21 {
			t.Fatalf("version=%d err=%v after refused down", version, versionErr)
		}
	})
}

func TestDualReaderClaimsNullableAndOutboundDeliveriesTogether(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID := uuid.NewString()
		inboxID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'dual-reader')`, orgID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO inboxes (id, org_id, address, status) VALUES ($1, $2, 'dual@example.com', 'active')`, inboxID, orgID); err != nil {
			t.Fatal(err)
		}
		webhook, err := st.CreateOrgWebhook(ctx, orgID, "https://dual.example.com", []string{"email.delivered", "email.received"})
		if err != nil {
			t.Fatal(err)
		}
		outboxID, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID: orgID, InboxID: inboxID, Provider: "smtp", IdempotencyKey: "dual-reader",
			To: "to@example.com", From: "dual@example.com", Subject: "subject", TextBody: "body",
		})
		if err != nil {
			t.Fatal(err)
		}
		outboxEventID, err := st.InsertOutboxEventReturningID(ctx, OutboxEvent{
			OrgID: orgID, OutboxMessageID: outboxID, EventType: "email.delivered", RawPayload: json.RawMessage(`{}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.EnqueueWebhookDelivery(ctx, orgID, webhook.ID, outboxEventID, "email.delivered", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		orgEventID, _, err := st.InsertOrgEventAndFanOut(ctx, orgID, "email.received", "message", uuid.NewString(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatal(err)
		}

		oldRows, err := db.QueryContext(ctx, `
			SELECT outbox_event_id::text FROM org_webhook_deliveries
			ORDER BY (outbox_event_id IS NULL) DESC
		`)
		if err != nil {
			t.Fatal(err)
		}
		if !oldRows.Next() {
			t.Fatal("expected a nullable delivery row")
		}
		var oldOutboxEventID string
		if err := oldRows.Scan(&oldOutboxEventID); err == nil {
			t.Fatal("pre-dual-reader string scan unexpectedly accepted NULL")
		}
		oldRows.Close()

		claimed, err := st.ClaimWebhookDeliveries(ctx, 10, "dual-reader", time.Now().UTC(), time.Minute)
		if err != nil {
			t.Fatalf("dual reader claim: %v", err)
		}
		if len(claimed) != 2 {
			t.Fatalf("claimed=%d, want nullable and outbound rows", len(claimed))
		}
		seenOutbox, seenOrg := false, false
		for _, delivery := range claimed {
			switch {
			case delivery.OutboxEventID == outboxEventID && delivery.OrgEventID == "":
				seenOutbox = true
			case delivery.OutboxEventID == "" && delivery.OrgEventID == orgEventID:
				seenOrg = true
			default:
				t.Fatalf("invalid claimed source pair: outbox=%q org=%q", delivery.OutboxEventID, delivery.OrgEventID)
			}
		}
		if !seenOutbox || !seenOrg {
			t.Fatalf("seenOutbox=%v seenOrg=%v", seenOutbox, seenOrg)
		}
	})
}
