package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestUpdateOutboxDeliveryStatusMonotonic(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)

		st := &Store{db: db, q: db}
		orgID := uuid.NewString()
		inboxID := uuid.NewString()

		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'acme')`, orgID); err != nil {
			t.Fatalf("insert org: %v", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO inboxes (id, org_id, address, status) VALUES ($1, $2, 'a@test.com', 'active')`, inboxID, orgID); err != nil {
			t.Fatalf("insert inbox: %v", err)
		}

		id, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID:          orgID,
			InboxID:        inboxID,
			Provider:       "resend",
			IdempotencyKey: "ds-test",
			To:             "to@test.com",
			From:           "a@test.com",
			Subject:        "Test",
			TextBody:       "body",
		})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}

		providerMsgID := "re_" + uuid.NewString()[:8]
		if err := st.MarkOutboxMessageSent(ctx, id, providerMsgID); err != nil {
			t.Fatalf("mark sent: %v", err)
		}

		// Progress: sent -> delivered
		if err := st.UpdateOutboxDeliveryStatus(ctx, providerMsgID, "sent"); err != nil {
			t.Fatalf("update to sent: %v", err)
		}
		if err := st.UpdateOutboxDeliveryStatus(ctx, providerMsgID, "delivered"); err != nil {
			t.Fatalf("update to delivered: %v", err)
		}

		// Verify current status
		var status string
		if err := db.QueryRowContext(ctx, `SELECT delivery_status FROM outbox_messages WHERE id = $1`, id).Scan(&status); err != nil {
			t.Fatalf("read status: %v", err)
		}
		if status != "delivered" {
			t.Fatalf("expected delivered, got %q", status)
		}

		// Try to regress: delivered -> sent (should be rejected by monotonic enforcement)
		if err := st.UpdateOutboxDeliveryStatus(ctx, providerMsgID, "sent"); err != nil {
			t.Fatalf("regress attempt: %v", err)
		}
		if err := db.QueryRowContext(ctx, `SELECT delivery_status FROM outbox_messages WHERE id = $1`, id).Scan(&status); err != nil {
			t.Fatalf("read status after regress: %v", err)
		}
		if status != "delivered" {
			t.Fatalf("expected delivered (monotonic), got %q", status)
		}
	})
}

func TestInsertOutboxEvent(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)

		st := &Store{db: db, q: db}
		orgID := uuid.NewString()
		inboxID := uuid.NewString()

		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'acme')`, orgID); err != nil {
			t.Fatalf("insert org: %v", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO inboxes (id, org_id, address, status) VALUES ($1, $2, 'a@test.com', 'active')`, inboxID, orgID); err != nil {
			t.Fatalf("insert inbox: %v", err)
		}

		id, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID:          orgID,
			InboxID:        inboxID,
			Provider:       "resend",
			IdempotencyKey: "evt-test",
			To:             "to@test.com",
			From:           "a@test.com",
			Subject:        "Test",
			TextBody:       "body",
		})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}

		providerMsgID := "re_" + uuid.NewString()[:8]
		if err := st.MarkOutboxMessageSent(ctx, id, providerMsgID); err != nil {
			t.Fatalf("mark sent: %v", err)
		}

		// Insert event
		if err := st.InsertOutboxEvent(ctx, OutboxEvent{
			OrgID:             orgID,
			ProviderMessageID: providerMsgID,
			EventType:         "email.delivered",
			RawPayload:        json.RawMessage(`{"email_id":"` + providerMsgID + `"}`),
			Reason:            "",
		}); err != nil {
			t.Fatalf("insert event: %v", err)
		}

		// Verify event was inserted
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM outbox_events WHERE provider_message_id = $1`, providerMsgID).Scan(&count); err != nil {
			t.Fatalf("count events: %v", err)
		}
		if count != 1 {
			t.Fatalf("expected 1 event, got %d", count)
		}
	})
}

func TestInsertOutboxEventBounceWithReason(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)

		st := &Store{db: db, q: db}
		orgID := uuid.NewString()
		inboxID := uuid.NewString()

		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'acme')`, orgID); err != nil {
			t.Fatalf("insert org: %v", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO inboxes (id, org_id, address, status) VALUES ($1, $2, 'a@test.com', 'active')`, inboxID, orgID); err != nil {
			t.Fatalf("insert inbox: %v", err)
		}

		id, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID:          orgID,
			InboxID:        inboxID,
			Provider:       "resend",
			IdempotencyKey: "bounce-test",
			To:             "to@test.com",
			From:           "a@test.com",
			Subject:        "Test",
			TextBody:       "body",
		})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}

		providerMsgID := "re_" + uuid.NewString()[:8]
		if err := st.MarkOutboxMessageSent(ctx, id, providerMsgID); err != nil {
			t.Fatalf("mark sent: %v", err)
		}

		// Insert bounce event with reason
		if err := st.InsertOutboxEvent(ctx, OutboxEvent{
			OrgID:             orgID,
			ProviderMessageID: providerMsgID,
			EventType:         "email.bounced",
			RawPayload:        json.RawMessage(`{"email_id":"` + providerMsgID + `","reason":"Mailbox full"}`),
			Reason:            "Mailbox full",
		}); err != nil {
			t.Fatalf("insert bounce event: %v", err)
		}

		// Verify reason was stored
		var reason string
		if err := db.QueryRowContext(ctx, `SELECT coalesce(reason, '') FROM outbox_events WHERE provider_message_id = $1`, providerMsgID).Scan(&reason); err != nil {
			t.Fatalf("read reason: %v", err)
		}
		if reason != "Mailbox full" {
			t.Fatalf("expected 'Mailbox full', got %q", reason)
		}
	})
}
