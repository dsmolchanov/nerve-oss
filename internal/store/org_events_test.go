package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestInsertOrgEventAndFanOutRequiresExplicitSensitiveConsent(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'events')`, orgID); err != nil {
			t.Fatal(err)
		}
		wildcard, err := st.CreateOrgWebhook(ctx, orgID, "https://wildcard.example.com", nil)
		if err != nil {
			t.Fatal(err)
		}
		explicit, err := st.CreateOrgWebhook(ctx, orgID, "https://received.example.com", []string{"email.received"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateOrgWebhook(ctx, orgID, "https://delivered.example.com", []string{"email.delivered"}); err != nil {
			t.Fatal(err)
		}

		refID := uuid.NewString()
		payload := json.RawMessage(`{"event":"email.received","message_id":"` + refID + `"}`)
		eventID, deliveries, err := st.InsertOrgEventAndFanOut(ctx, orgID, "email.received", "message", refID, payload)
		if err != nil {
			t.Fatalf("insert and fan out: %v", err)
		}
		if eventID == "" || deliveries != 1 {
			t.Fatalf("event=%q deliveries=%d, want one explicit delivery", eventID, deliveries)
		}

		var fannedOutAt time.Time
		if err := db.QueryRowContext(ctx, `SELECT fanned_out_at FROM org_events WHERE id = $1`, eventID).Scan(&fannedOutAt); err != nil {
			t.Fatalf("read journal: %v", err)
		}
		var explicitCount, wildcardCount int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM org_webhook_deliveries WHERE webhook_id = $1`, explicit.ID).Scan(&explicitCount); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM org_webhook_deliveries WHERE webhook_id = $1`, wildcard.ID).Scan(&wildcardCount); err != nil {
			t.Fatal(err)
		}
		if explicitCount != 1 || wildcardCount != 0 {
			t.Fatalf("explicit=%d wildcard=%d, sensitive wildcard must not match", explicitCount, wildcardCount)
		}

		replayedID, replayedDeliveries, err := st.InsertOrgEventAndFanOut(ctx, orgID, "email.received", "message", refID, payload)
		if err != nil {
			t.Fatalf("replay: %v", err)
		}
		if replayedID != eventID || replayedDeliveries != 0 {
			t.Fatalf("replay event=%q deliveries=%d", replayedID, replayedDeliveries)
		}
		var after time.Time
		if err := db.QueryRowContext(ctx, `SELECT fanned_out_at FROM org_events WHERE id = $1`, eventID).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if !after.Equal(fannedOutAt) {
			t.Fatalf("replay changed fanned_out_at from %s to %s", fannedOutAt, after)
		}
	})
}

func TestInsertOrgEventAndFanOutNeverSendsSensitiveEventsOverCleartext(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'cleartext-sensitive')`, orgID); err != nil {
			t.Fatal(err)
		}
		webhook, err := st.CreateOrgWebhook(ctx, orgID, "http://cleartext.example.com", []string{"email.received"})
		if err != nil {
			t.Fatal(err)
		}

		_, deliveries, err := st.InsertOrgEventAndFanOut(
			ctx,
			orgID,
			"email.received",
			"message",
			uuid.NewString(),
			json.RawMessage(`{"event":"email.received"}`),
		)
		if err != nil {
			t.Fatal(err)
		}
		if deliveries != 0 {
			t.Fatalf("cleartext sensitive deliveries=%d, want 0", deliveries)
		}
		var rows int
		if err := db.QueryRowContext(ctx, `
			SELECT count(*) FROM org_webhook_deliveries WHERE webhook_id = $1
		`, webhook.ID).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 0 {
			t.Fatalf("cleartext sensitive delivery rows=%d, want 0", rows)
		}
	})
}

func TestInsertOrgEventAndFanOutRollsBackWithRunAsOrg(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'events-rollback')`, orgID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateOrgWebhook(ctx, orgID, "https://received.example.com", []string{"email.received"}); err != nil {
			t.Fatal(err)
		}

		sentinel := errors.New("force outer rollback")
		err := st.RunAsOrg(ctx, orgID, func(scoped *Store) error {
			if _, _, err := scoped.InsertOrgEventAndFanOut(
				ctx,
				orgID,
				"email.received",
				"message",
				uuid.NewString(),
				json.RawMessage(`{"event":"email.received"}`),
			); err != nil {
				return err
			}
			return sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("RunAsOrg error=%v, want sentinel", err)
		}
		for _, table := range []string{"org_events", "org_webhook_deliveries"} {
			var count int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("outer rollback left %d rows in %s", count, table)
			}
		}
	})
}

func TestInsertOrgEventAndFanOutFailureLeavesNoJournal(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'events-failure')`, orgID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateOrgWebhook(ctx, orgID, "https://received.example.com", []string{"email.received"}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `
			ALTER TABLE org_webhook_deliveries
			ADD CONSTRAINT reject_received_for_test CHECK (event_type <> 'email.received')
		`); err != nil {
			t.Fatal(err)
		}

		_, _, err := st.InsertOrgEventAndFanOut(
			ctx,
			orgID,
			"email.received",
			"message",
			uuid.NewString(),
			json.RawMessage(`{"event":"email.received"}`),
		)
		if err == nil {
			t.Fatal("expected forced delivery failure")
		}
		var events, deliveries int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM org_events`).Scan(&events); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM org_webhook_deliveries`).Scan(&deliveries); err != nil {
			t.Fatal(err)
		}
		if events != 0 || deliveries != 0 {
			t.Fatalf("failed fan-out committed events=%d deliveries=%d", events, deliveries)
		}
	})
}

func TestReFanOutOrgEventRepairsPendingJournal(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID := uuid.NewString()
		eventID := uuid.NewString()
		refID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'events-repair')`, orgID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateOrgWebhook(ctx, orgID, "https://received.example.com", []string{"email.received"}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO org_events (id, org_id, event_type, ref_kind, ref_id, payload, created_at)
			VALUES ($1, $2, 'email.received', 'message', $3, '{"event":"email.received"}', now() - interval '10 minutes')
		`, eventID, orgID, refID); err != nil {
			t.Fatal(err)
		}

		pending, err := st.ListPendingOrgEvents(ctx, time.Now().UTC().Add(-5*time.Minute), 10)
		if err != nil || len(pending) != 1 || pending[0].ID != eventID {
			t.Fatalf("pending=%v err=%v", pending, err)
		}
		deliveries, err := st.ReFanOutOrgEvent(ctx, eventID)
		if err != nil || deliveries != 1 {
			t.Fatalf("repair deliveries=%d err=%v", deliveries, err)
		}
		pending, err = st.ListPendingOrgEvents(ctx, time.Now().UTC(), 10)
		if err != nil || len(pending) != 0 {
			t.Fatalf("pending after repair=%v err=%v", pending, err)
		}
		deliveries, err = st.ReFanOutOrgEvent(ctx, eventID)
		if err != nil || deliveries != 0 {
			t.Fatalf("idempotent repair deliveries=%d err=%v", deliveries, err)
		}
	})
}

func TestInsertOrgEventAndFanOutPendingReplayUsesJournalPayload(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID := uuid.NewString()
		eventID := uuid.NewString()
		refID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'events-payload')`, orgID); err != nil {
			t.Fatal(err)
		}
		webhook, err := st.CreateOrgWebhook(ctx, orgID, "https://received.example.com", []string{"email.received"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO org_events (id, org_id, event_type, ref_kind, ref_id, payload)
			VALUES ($1, $2, 'email.received', 'message', $3, '{"source":"journal"}')
		`, eventID, orgID, refID); err != nil {
			t.Fatal(err)
		}

		replayedID, deliveries, err := st.InsertOrgEventAndFanOut(
			ctx,
			orgID,
			"email.received",
			"message",
			refID,
			json.RawMessage(`{"source":"replay"}`),
		)
		if err != nil || replayedID != eventID || deliveries != 1 {
			t.Fatalf("replay event=%q deliveries=%d err=%v", replayedID, deliveries, err)
		}
		var payload json.RawMessage
		if err := db.QueryRowContext(ctx, `
			SELECT payload FROM org_webhook_deliveries
			WHERE webhook_id = $1 AND org_event_id = $2
		`, webhook.ID, eventID).Scan(&payload); err != nil {
			t.Fatal(err)
		}
		var decoded map[string]string
		if err := json.Unmarshal(payload, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded["source"] != "journal" {
			t.Fatalf("delivery payload source=%q, want journal", decoded["source"])
		}
	})
}
