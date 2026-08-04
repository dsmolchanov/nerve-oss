package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCreateListAndDeleteOrgWebhook(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}

		orgID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'acme')`, orgID); err != nil {
			t.Fatalf("insert org: %v", err)
		}

		// Create with explicit event filter.
		wh, err := st.CreateOrgWebhook(ctx, orgID, "https://example.com/hook", []string{"email.delivered", "email.bounced"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if wh.ID == "" {
			t.Error("expected webhook id")
		}
		if wh.Secret == "" || len(wh.Secret) < 32 {
			t.Errorf("expected 64-char hex secret, got %q", wh.Secret)
		}
		if len(wh.Events) != 2 || wh.Events[0] != "email.delivered" || wh.Events[1] != "email.bounced" {
			t.Errorf("expected event filter preserved, got %v", wh.Events)
		}

		// Second subscription with no filter (subscribes to all).
		wh2, err := st.CreateOrgWebhook(ctx, orgID, "https://example.com/all", nil)
		if err != nil {
			t.Fatalf("create all: %v", err)
		}
		if len(wh2.Events) != 0 {
			t.Errorf("expected empty events array, got %v", wh2.Events)
		}

		// List returns both.
		rows, err := st.ListOrgWebhooks(ctx, orgID)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("expected 2 webhooks, got %d", len(rows))
		}

		// Rotate produces a new secret.
		newSecret, err := st.RotateOrgWebhookSecret(ctx, orgID, wh.ID)
		if err != nil {
			t.Fatalf("rotate: %v", err)
		}
		if newSecret == wh.Secret {
			t.Error("rotated secret should differ from original")
		}
		after, err := st.GetOrgWebhook(ctx, orgID, wh.ID)
		if err != nil {
			t.Fatalf("get after rotate: %v", err)
		}
		if after.Secret != newSecret {
			t.Error("persisted secret should match rotation return")
		}

		// Delete removes only the targeted row.
		removed, err := st.DeleteOrgWebhook(ctx, orgID, wh.ID)
		if err != nil || !removed {
			t.Fatalf("delete: removed=%v err=%v", removed, err)
		}
		rows, _ = st.ListOrgWebhooks(ctx, orgID)
		if len(rows) != 1 {
			t.Errorf("expected 1 webhook after delete, got %d", len(rows))
		}
	})
}

func TestFanOutWebhookDeliveries_MatchesFilter(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}

		orgID := uuid.NewString()
		inboxID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'acme')`, orgID); err != nil {
			t.Fatalf("insert org: %v", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO inboxes (id, org_id, address, status) VALUES ($1, $2, 'a@local.neuralmail', 'active')`, inboxID, orgID); err != nil {
			t.Fatalf("insert inbox: %v", err)
		}

		// Seed an outbox message + event to fan out from.
		outboxID, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID:          orgID,
			InboxID:        inboxID,
			Provider:       "smtp",
			IdempotencyKey: "fan-1",
			To:             "to@external.com",
			From:           "a@local.neuralmail",
			Subject:        "hi",
			TextBody:       "body",
		})
		if err != nil {
			t.Fatalf("enqueue outbox: %v", err)
		}
		eventID, err := st.InsertOutboxEventReturningID(ctx, OutboxEvent{
			OrgID:           orgID,
			OutboxMessageID: outboxID,
			EventType:       "email.delivered",
			RawPayload:      json.RawMessage(`{"ok":true}`),
		})
		if err != nil || eventID == "" {
			t.Fatalf("insert event: id=%q err=%v", eventID, err)
		}

		// Three subscriptions:
		//   all  — no filter, subscribes to everything
		//   only — filter includes email.delivered
		//   miss — filter does not include email.delivered
		wAll, _ := st.CreateOrgWebhook(ctx, orgID, "https://all.example.com", nil)
		wOnly, _ := st.CreateOrgWebhook(ctx, orgID, "https://only.example.com", []string{"email.delivered"})
		wMiss, _ := st.CreateOrgWebhook(ctx, orgID, "https://miss.example.com", []string{"email.bounced"})

		count, err := st.FanOutWebhookDeliveries(ctx, orgID, eventID, "email.delivered", json.RawMessage(`{"ok":true}`))
		if err != nil {
			t.Fatalf("fan out: %v", err)
		}
		if count != 2 {
			t.Errorf("expected 2 deliveries (all + only), got %d", count)
		}

		// Verify row-level: the two matching webhooks have a delivery row,
		// the non-matching one does not.
		countFor := func(webhookID string) int {
			var n int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM org_webhook_deliveries WHERE webhook_id = $1`, webhookID).Scan(&n); err != nil {
				t.Fatalf("count for %s: %v", webhookID, err)
			}
			return n
		}
		if countFor(wAll.ID) != 1 {
			t.Errorf("expected 1 row for 'all' webhook, got %d", countFor(wAll.ID))
		}
		if countFor(wOnly.ID) != 1 {
			t.Errorf("expected 1 row for 'only' webhook, got %d", countFor(wOnly.ID))
		}
		if countFor(wMiss.ID) != 0 {
			t.Errorf("expected 0 rows for 'miss' webhook, got %d", countFor(wMiss.ID))
		}

		// Fan out again with the same event id — idempotency must hold.
		// Row counts should not change (ON CONFLICT keeps the existing row).
		count, err = st.FanOutWebhookDeliveries(ctx, orgID, eventID, "email.delivered", json.RawMessage(`{"ok":true}`))
		if err != nil {
			t.Fatalf("fan out replay: %v", err)
		}
		if count != 2 {
			t.Errorf("expected 2 on replay too, got %d", count)
		}
		if countFor(wAll.ID) != 1 || countFor(wOnly.ID) != 1 {
			t.Error("replay should not create duplicate delivery rows")
		}
	})
}

func TestClaimWebhookDeliveriesAndMarkDelivered(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}

		orgID := uuid.NewString()
		inboxID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'acme')`, orgID); err != nil {
			t.Fatalf("insert org: %v", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO inboxes (id, org_id, address, status) VALUES ($1, $2, 'a@local.neuralmail', 'active')`, inboxID, orgID); err != nil {
			t.Fatalf("insert inbox: %v", err)
		}
		outboxID, _ := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID: orgID, InboxID: inboxID, Provider: "smtp", IdempotencyKey: "claim-1",
			To: "to@external.com", From: "a@local.neuralmail", Subject: "hi", TextBody: "b",
		})
		eventID, _ := st.InsertOutboxEventReturningID(ctx, OutboxEvent{
			OrgID: orgID, OutboxMessageID: outboxID, EventType: "email.delivered", RawPayload: json.RawMessage(`{}`),
		})
		wh, _ := st.CreateOrgWebhook(ctx, orgID, "https://example.com/hook", nil)
		if _, err := st.EnqueueWebhookDelivery(ctx, orgID, wh.ID, eventID, "email.delivered", json.RawMessage(`{"k":"v"}`)); err != nil {
			t.Fatalf("enqueue delivery: %v", err)
		}

		// Claim the delivery (as a worker).
		claimed, err := st.ClaimWebhookDeliveries(ctx, 10, "worker-1", time.Time{}, 0)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if len(claimed) != 1 {
			t.Fatalf("expected 1 claimed, got %d", len(claimed))
		}
		if claimed[0].Status != "delivering" {
			t.Errorf("expected status=delivering, got %q", claimed[0].Status)
		}
		if claimed[0].AttemptCount != 1 {
			t.Errorf("expected attempt_count=1 after claim, got %d", claimed[0].AttemptCount)
		}

		// A second concurrent claim sees nothing (row is locked).
		empty, err := st.ClaimWebhookDeliveries(ctx, 10, "worker-2", time.Time{}, 0)
		if err != nil {
			t.Fatalf("second claim: %v", err)
		}
		if len(empty) != 0 {
			t.Errorf("expected 0 claimed on second pass, got %d", len(empty))
		}

		// LookupWebhookSubscription returns url + secret.
		url, secret, err := st.LookupWebhookSubscription(ctx, wh.ID)
		if err != nil {
			t.Fatalf("lookup: %v", err)
		}
		if url != "https://example.com/hook" || secret == "" {
			t.Errorf("lookup returned url=%q secret=%q", url, secret)
		}

		// Mark delivered.
		if err := st.MarkWebhookDeliveryDelivered(ctx, claimed[0].ID, 200); err != nil {
			t.Fatalf("mark delivered: %v", err)
		}
		var status string
		var statusCode sql.NullInt32
		row := db.QueryRowContext(ctx, `SELECT status, last_status_code FROM org_webhook_deliveries WHERE id = $1`, claimed[0].ID)
		if err := row.Scan(&status, &statusCode); err != nil {
			t.Fatalf("scan after deliver: %v", err)
		}
		if status != "delivered" {
			t.Errorf("expected status=delivered, got %q", status)
		}
		if !statusCode.Valid || statusCode.Int32 != 200 {
			t.Errorf("expected last_status_code=200, got %+v", statusCode)
		}
	})
}

func TestParseTextArray(t *testing.T) {
	cases := map[string][]string{
		"":                                {},
		"{}":                              {},
		"null":                            {},
		`{a,b,c}`:                         {"a", "b", "c"},
		`{"hello world"}`:                 {"hello world"},
		`{email.delivered,email.bounced}`: {"email.delivered", "email.bounced"},
		`{"with,comma","plain"}`:          {"with,comma", "plain"},
	}
	for in, want := range cases {
		got := parseTextArray(in)
		if len(got) != len(want) {
			t.Errorf("parseTextArray(%q) length: got %v, want %v", in, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("parseTextArray(%q)[%d] = %q, want %q", in, i, got[i], want[i])
			}
		}
	}
}
