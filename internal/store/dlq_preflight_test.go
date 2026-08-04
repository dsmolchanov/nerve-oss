package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestPreflightDomainVerification(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}

		orgID := uuid.NewString()
		inboxID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'acme')`, orgID); err != nil {
			t.Fatalf("insert org: %v", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO inboxes (id, org_id, address, status) VALUES ($1, $2, 'a@pending.example.com', 'active')`, inboxID, orgID); err != nil {
			t.Fatalf("insert inbox: %v", err)
		}

		// Claim pending.example.com but leave it in pending status.
		if _, err := db.ExecContext(ctx, `
			INSERT INTO org_domains (id, org_id, domain, status, verification_token)
			VALUES ($1, $2, 'pending.example.com', 'pending', 'tok-1')
		`, uuid.NewString(), orgID); err != nil {
			t.Fatalf("insert pending domain: %v", err)
		}
		// Also claim active.example.com in active status.
		if _, err := db.ExecContext(ctx, `
			INSERT INTO org_domains (id, org_id, domain, status, verification_token, verified_at)
			VALUES ($1, $2, 'active.example.com', 'active', 'tok-2', now())
		`, uuid.NewString(), orgID); err != nil {
			t.Fatalf("insert active domain: %v", err)
		}

		// Case 1: sending from an unverified (pending) claimed domain → rejected.
		_, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID:          orgID,
			InboxID:        inboxID,
			Provider:       "resend",
			IdempotencyKey: "pf-pending-1",
			To:             "someone@external.com",
			From:           "a@pending.example.com",
			Subject:        "hi",
			TextBody:       "body",
		})
		if !errors.Is(err, ErrDomainNotVerified) {
			t.Errorf("expected ErrDomainNotVerified for pending domain, got %v", err)
		}

		// Case 2: sending from "Name <addr@domain>" form on pending domain → rejected.
		_, err = st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID:          orgID,
			InboxID:        inboxID,
			Provider:       "resend",
			IdempotencyKey: "pf-pending-2",
			To:             "someone@external.com",
			From:           "Alice <a@pending.example.com>",
			Subject:        "hi",
			TextBody:       "body",
		})
		if !errors.Is(err, ErrDomainNotVerified) {
			t.Errorf("expected ErrDomainNotVerified for displayname form, got %v", err)
		}

		// Case 3: sending from an active claimed domain → allowed.
		id, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID:          orgID,
			InboxID:        inboxID,
			Provider:       "resend",
			IdempotencyKey: "pf-active-1",
			To:             "someone@external.com",
			From:           "bot@active.example.com",
			Subject:        "hi",
			TextBody:       "body",
		})
		if err != nil {
			t.Errorf("expected active domain to be allowed, got err=%v", err)
		}
		if id == "" {
			t.Error("expected non-empty outbox id for allowed enqueue")
		}

		// Case 4: sending from the legacy local.neuralmail domain → allowed (skipped).
		id, err = st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID:          orgID,
			InboxID:        inboxID,
			Provider:       "smtp",
			IdempotencyKey: "pf-local-1",
			To:             "someone@external.com",
			From:           "dev@local.neuralmail",
			Subject:        "hi",
			TextBody:       "body",
		})
		if err != nil {
			t.Errorf("expected local.neuralmail to be allowed, got err=%v", err)
		}
		if id == "" {
			t.Error("expected non-empty outbox id for local enqueue")
		}

		// Case 5: sending from an unclaimed domain → allowed (legacy behavior preserved).
		id, err = st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID:          orgID,
			InboxID:        inboxID,
			Provider:       "smtp",
			IdempotencyKey: "pf-unclaimed-1",
			To:             "someone@external.com",
			From:           "bot@unclaimed.example.com",
			Subject:        "hi",
			TextBody:       "body",
		})
		if err != nil {
			t.Errorf("expected unclaimed domain to be allowed, got err=%v", err)
		}
		if id == "" {
			t.Error("expected non-empty outbox id for unclaimed enqueue")
		}
	})
}

func TestDLQListAndReplay(t *testing.T) {
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

		// Enqueue and manually mark as permanently failed.
		id, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID:          orgID,
			InboxID:        inboxID,
			Provider:       "smtp",
			IdempotencyKey: "dlq-1",
			To:             "to@external.com",
			From:           "a@local.neuralmail",
			Subject:        "hi",
			TextBody:       "body",
		})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if err := st.MarkOutboxMessageFailed(ctx, id, "permanent:invalid_recipient: 422 invalid"); err != nil {
			t.Fatalf("mark failed: %v", err)
		}

		// ListFailedOutboxForOrg returns the row.
		rows, err := st.ListFailedOutboxForOrg(ctx, orgID, 10)
		if err != nil {
			t.Fatalf("list failed: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("expected 1 failed row, got %d", len(rows))
		}
		if rows[0].ID != id {
			t.Errorf("expected id %s, got %s", id, rows[0].ID)
		}
		if !rows[0].LastError.Valid || !strings.Contains(rows[0].LastError.String, "permanent:invalid_recipient") {
			t.Errorf("expected last_error to mark permanent, got %q", rows[0].LastError.String)
		}

		// GetOutboxMessageByIDForOrg returns the row.
		msg, err := st.GetOutboxMessageByIDForOrg(ctx, orgID, id)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if msg.Status != "failed" {
			t.Errorf("expected status failed, got %q", msg.Status)
		}

		// Cross-org lookup returns ErrNoRows.
		otherOrg := uuid.NewString()
		_, err = st.GetOutboxMessageByIDForOrg(ctx, otherOrg, id)
		if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("expected sql.ErrNoRows for cross-org lookup, got %v", err)
		}

		// Replay resets the row.
		replayed, err := st.ReplayOutboxMessage(ctx, orgID, id)
		if err != nil || !replayed {
			t.Fatalf("replay: replayed=%v err=%v", replayed, err)
		}
		after, err := st.GetOutboxMessageByIDForOrg(ctx, orgID, id)
		if err != nil {
			t.Fatalf("get after replay: %v", err)
		}
		if after.Status != "queued" {
			t.Errorf("expected status=queued after replay, got %q", after.Status)
		}
		if after.AttemptCount != 0 {
			t.Errorf("expected attempt_count=0 after replay, got %d", after.AttemptCount)
		}
		if after.LastError.Valid && after.LastError.String != "" {
			t.Errorf("expected last_error cleared after replay, got %q", after.LastError.String)
		}

		// Replaying a non-failed row (now 'queued') returns false.
		replayed, err = st.ReplayOutboxMessage(ctx, orgID, id)
		if err != nil {
			t.Fatalf("second replay: %v", err)
		}
		if replayed {
			t.Errorf("expected replayed=false when row is not in failed state")
		}

		// Listing DLQ after replay returns no rows.
		rows, err = st.ListFailedOutboxForOrg(ctx, orgID, 10)
		if err != nil {
			t.Fatalf("list after replay: %v", err)
		}
		if len(rows) != 0 {
			t.Errorf("expected 0 failed rows after replay, got %d", len(rows))
		}
	})
}

func TestExtractDomainFromAddress(t *testing.T) {
	cases := map[string]string{
		"":                             "",
		"plain":                        "",
		"user@host.com":                "host.com",
		"User@HOST.com":                "host.com",
		"Alice <user@host.com>":        "host.com",
		"Alice <User@HOST.com>":        "host.com",
		"  Alice <user@host.com>  ":    "host.com",
		"no-at-sign":                   "",
		"trailing@":                    "",
		"Multiple <one@a.com> <two@b>": "b",
	}
	for in, want := range cases {
		if got := extractDomainFromAddress(in); got != want {
			t.Errorf("extractDomainFromAddress(%q) = %q, want %q", in, got, want)
		}
	}
}
