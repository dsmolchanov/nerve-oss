package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestSuppression_AddRemoveList(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}

		orgID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'acme')`, orgID); err != nil {
			t.Fatalf("insert org: %v", err)
		}

		// Initially not suppressed.
		if got, _, err := st.IsSuppressed(ctx, orgID, "Bob@Example.com"); err != nil || got {
			t.Fatalf("expected not suppressed, got=%v err=%v", got, err)
		}

		// Add a suppression and verify the lookup is case-insensitive.
		if err := st.AddSuppression(ctx, orgID, "Bob@Example.com", "hard_bounce", "bounce"); err != nil {
			t.Fatalf("add suppression: %v", err)
		}
		got, reason, err := st.IsSuppressed(ctx, orgID, "BOB@example.com")
		if err != nil {
			t.Fatalf("is suppressed: %v", err)
		}
		if !got {
			t.Fatalf("expected suppressed=true after add")
		}
		if reason != "hard_bounce" {
			t.Errorf("expected reason hard_bounce, got %q", reason)
		}

		// Idempotent upsert: re-adding overwrites reason.
		if err := st.AddSuppression(ctx, orgID, "bob@example.com", "complaint", "complaint"); err != nil {
			t.Fatalf("re-add suppression: %v", err)
		}
		_, reason, _ = st.IsSuppressed(ctx, orgID, "bob@example.com")
		if reason != "complaint" {
			t.Errorf("expected reason complaint after upsert, got %q", reason)
		}

		// List returns the row.
		rows, err := st.ListSuppressions(ctx, orgID, 0)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("expected 1 row, got %d", len(rows))
		}
		if rows[0].Email != "bob@example.com" {
			t.Errorf("expected lowercased email, got %q", rows[0].Email)
		}

		// Remove and verify.
		removed, err := st.RemoveSuppression(ctx, orgID, "bob@example.com")
		if err != nil || !removed {
			t.Fatalf("remove: removed=%v err=%v", removed, err)
		}
		got, _, _ = st.IsSuppressed(ctx, orgID, "bob@example.com")
		if got {
			t.Errorf("expected suppressed=false after remove")
		}

		// Removing a missing entry returns (false, nil).
		removed, err = st.RemoveSuppression(ctx, orgID, "nope@example.com")
		if err != nil {
			t.Fatalf("remove missing: %v", err)
		}
		if removed {
			t.Errorf("expected removed=false for missing entry")
		}
	})
}

func TestEnqueueOutboxMessage_SuppressedShortCircuits(t *testing.T) {
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

		// Pre-suppress the recipient.
		if err := st.AddSuppression(ctx, orgID, "blocked@example.com", "hard_bounce", "bounce"); err != nil {
			t.Fatalf("add suppression: %v", err)
		}

		outboxID, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID:          orgID,
			InboxID:        inboxID,
			Provider:       "smtp",
			IdempotencyKey: "k-suppressed-1",
			To:             "blocked@example.com",
			From:           "a@local.neuralmail",
			Subject:        "hi",
			TextBody:       "test",
		})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if outboxID == "" {
			t.Fatalf("expected outbox id from suppressed enqueue")
		}

		// Row should be in failed/suppressed state with last_error containing the reason.
		var status, deliveryStatus, lastError string
		row := db.QueryRowContext(ctx, `
			SELECT status, delivery_status, coalesce(last_error, '')
			FROM outbox_messages WHERE id = $1
		`, outboxID)
		if err := row.Scan(&status, &deliveryStatus, &lastError); err != nil {
			t.Fatalf("scan row: %v", err)
		}
		if status != "failed" {
			t.Errorf("expected status=failed, got %q", status)
		}
		if deliveryStatus != "suppressed" {
			t.Errorf("expected delivery_status=suppressed, got %q", deliveryStatus)
		}
		if !strings.Contains(lastError, "suppressed:hard_bounce") {
			t.Errorf("expected last_error to contain suppressed:hard_bounce, got %q", lastError)
		}

		// Audit timeline: outbox_events should have a row for this message
		// linked via outbox_message_id with no provider_message_id.
		var eventCount int
		var providerMsgID sql.NullString
		row = db.QueryRowContext(ctx, `
			SELECT count(*), max(provider_message_id)
			FROM outbox_events WHERE outbox_message_id = $1
		`, outboxID)
		if err := row.Scan(&eventCount, &providerMsgID); err != nil {
			t.Fatalf("scan events: %v", err)
		}
		if eventCount != 1 {
			t.Errorf("expected 1 event row, got %d", eventCount)
		}
		if providerMsgID.Valid {
			t.Errorf("expected null provider_message_id, got %q", providerMsgID.String)
		}

		// Idempotency: re-enqueueing the same key returns the same row id and
		// does NOT append a duplicate event row.
		outboxID2, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID:          orgID,
			InboxID:        inboxID,
			Provider:       "smtp",
			IdempotencyKey: "k-suppressed-1",
			To:             "blocked@example.com",
			From:           "a@local.neuralmail",
			Subject:        "hi",
			TextBody:       "test",
		})
		if err != nil {
			t.Fatalf("re-enqueue: %v", err)
		}
		if outboxID2 != outboxID {
			t.Errorf("expected same outbox id on idempotency replay, got %s vs %s", outboxID, outboxID2)
		}
		row = db.QueryRowContext(ctx, `SELECT count(*) FROM outbox_events WHERE outbox_message_id = $1`, outboxID)
		var afterCount int
		if err := row.Scan(&afterCount); err != nil {
			t.Fatalf("recount events: %v", err)
		}
		if afterCount != 1 {
			t.Errorf("expected event row count to stay at 1 after replay, got %d", afterCount)
		}
	})
}

func TestInsertOutboxEvent_DirectAndJoinPaths(t *testing.T) {
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

		outboxID, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID:          orgID,
			InboxID:        inboxID,
			Provider:       "smtp",
			IdempotencyKey: "ev-1",
			To:             "to@example.com",
			From:           "a@local.neuralmail",
			Subject:        "hi",
			TextBody:       "test",
		})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}

		// Direct path: insert with OutboxMessageID, no provider id.
		err = st.InsertOutboxEvent(ctx, OutboxEvent{
			OrgID:           orgID,
			OutboxMessageID: outboxID,
			EventType:       "synthetic",
			RawPayload:      []byte(`{}`),
			Reason:          "manual",
		})
		if err != nil {
			t.Fatalf("direct insert: %v", err)
		}

		// Simulate provider acceptance: set provider_message_id on the row.
		if _, err := db.ExecContext(ctx, `UPDATE outbox_messages SET provider_message_id = 'pmsg-1' WHERE id = $1`, outboxID); err != nil {
			t.Fatalf("set provider_message_id: %v", err)
		}

		// Join path: webhook callback that only knows the provider id.
		err = st.InsertOutboxEvent(ctx, OutboxEvent{
			OrgID:             orgID,
			ProviderMessageID: "pmsg-1",
			EventType:         "email.delivered",
			RawPayload:        []byte(`{"ok":true}`),
		})
		if err != nil {
			t.Fatalf("join insert: %v", err)
		}

		// Both rows should be linked to the same outbox_message_id.
		var count int
		row := db.QueryRowContext(ctx, `SELECT count(*) FROM outbox_events WHERE outbox_message_id = $1`, outboxID)
		if err := row.Scan(&count); err != nil {
			t.Fatalf("count events: %v", err)
		}
		if count != 2 {
			t.Errorf("expected 2 events linked to %s, got %d", outboxID, count)
		}

		// Bare InsertOutboxEvent with neither id should error.
		err = st.InsertOutboxEvent(ctx, OutboxEvent{OrgID: orgID, EventType: "broken"})
		if err == nil {
			t.Errorf("expected error when both ids missing")
		}
	})
}
