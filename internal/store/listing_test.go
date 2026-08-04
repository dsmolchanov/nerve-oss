package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestListThreadsForInboxBasic(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)

		st := &Store{db: db, q: db}
		orgID := uuid.NewString()
		inboxID := uuid.NewString()

		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'acme')`, orgID); err != nil {
			t.Fatalf("insert org: %v", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO inboxes (id, org_id, address, status) VALUES ($1, $2, 'test@example.com', 'active')`, inboxID, orgID); err != nil {
			t.Fatalf("insert inbox: %v", err)
		}

		// Insert a thread with messages
		threadID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO threads (id, inbox_id, org_id, subject, status, participants, updated_at) VALUES ($1, $2, $3, 'Test Subject', 'open', '[]', $4)`,
			threadID, inboxID, orgID, time.Now().UTC()); err != nil {
			t.Fatalf("insert thread: %v", err)
		}
		msg1ID := uuid.NewString()
		msg2ID := uuid.NewString()
		now := time.Now().UTC()
		if _, err := db.ExecContext(ctx, `INSERT INTO messages (id, inbox_id, thread_id, org_id, direction, subject, text, created_at, from_json, to_json, cc_json) VALUES ($1, $2, $3, $4, 'inbound', 'Test Subject', 'Hello world', $5, '{"email":"a@b.com"}', '[]', '[]')`,
			msg1ID, inboxID, threadID, orgID, now.Add(-time.Minute)); err != nil {
			t.Fatalf("insert msg1: %v", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO messages (id, inbox_id, thread_id, org_id, direction, subject, text, created_at, from_json, to_json, cc_json) VALUES ($1, $2, $3, $4, 'outbound', 'Re: Test Subject', 'Reply here', $5, '{"email":"b@c.com"}', '[]', '[]')`,
			msg2ID, inboxID, threadID, orgID, now); err != nil {
			t.Fatalf("insert msg2: %v", err)
		}

		threads, err := st.ListThreadsForInbox(ctx, inboxID, "", 50, 0)
		if err != nil {
			t.Fatalf("list threads: %v", err)
		}
		if len(threads) != 1 {
			t.Fatalf("expected 1 thread, got %d", len(threads))
		}
		if threads[0].ID != threadID {
			t.Fatalf("expected thread %s, got %s", threadID, threads[0].ID)
		}
		if threads[0].MessageCount != 2 {
			t.Fatalf("expected 2 messages, got %d", threads[0].MessageCount)
		}
		if threads[0].Subject != "Test Subject" {
			t.Fatalf("expected subject 'Test Subject', got %q", threads[0].Subject)
		}
		if threads[0].LatestSnippet == "" {
			t.Fatalf("expected non-empty latest snippet")
		}
	})
}

func TestListThreadsForInboxStatusFilter(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)

		st := &Store{db: db, q: db}
		orgID := uuid.NewString()
		inboxID := uuid.NewString()

		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'acme')`, orgID); err != nil {
			t.Fatalf("insert org: %v", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO inboxes (id, org_id, address, status) VALUES ($1, $2, 'test@example.com', 'active')`, inboxID, orgID); err != nil {
			t.Fatalf("insert inbox: %v", err)
		}

		now := time.Now().UTC()
		openID := uuid.NewString()
		closedID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO threads (id, inbox_id, org_id, subject, status, participants, updated_at) VALUES ($1, $2, $3, 'Open Thread', 'open', '[]', $4)`,
			openID, inboxID, orgID, now); err != nil {
			t.Fatalf("insert open thread: %v", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO threads (id, inbox_id, org_id, subject, status, participants, updated_at) VALUES ($1, $2, $3, 'Closed Thread', 'closed', '[]', $4)`,
			closedID, inboxID, orgID, now); err != nil {
			t.Fatalf("insert closed thread: %v", err)
		}

		// Filter by open
		threads, err := st.ListThreadsForInbox(ctx, inboxID, "open", 50, 0)
		if err != nil {
			t.Fatalf("list threads: %v", err)
		}
		if len(threads) != 1 || threads[0].ID != openID {
			t.Fatalf("expected only open thread, got %d threads", len(threads))
		}

		// All threads
		all, err := st.ListThreadsForInbox(ctx, inboxID, "", 50, 0)
		if err != nil {
			t.Fatalf("list all: %v", err)
		}
		if len(all) != 2 {
			t.Fatalf("expected 2 threads, got %d", len(all))
		}
	})
}

func TestListThreadsForInboxPagination(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)

		st := &Store{db: db, q: db}
		orgID := uuid.NewString()
		inboxID := uuid.NewString()

		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'acme')`, orgID); err != nil {
			t.Fatalf("insert org: %v", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO inboxes (id, org_id, address, status) VALUES ($1, $2, 'test@example.com', 'active')`, inboxID, orgID); err != nil {
			t.Fatalf("insert inbox: %v", err)
		}

		now := time.Now().UTC()
		for i := 0; i < 5; i++ {
			tid := uuid.NewString()
			if _, err := db.ExecContext(ctx, `INSERT INTO threads (id, inbox_id, org_id, subject, status, participants, updated_at) VALUES ($1, $2, $3, $4, 'open', '[]', $5)`,
				tid, inboxID, orgID, "Thread "+string(rune('A'+i)), now.Add(time.Duration(i)*time.Minute)); err != nil {
				t.Fatalf("insert thread %d: %v", i, err)
			}
		}

		// Limit to 2
		page1, err := st.ListThreadsForInbox(ctx, inboxID, "", 2, 0)
		if err != nil {
			t.Fatalf("page1: %v", err)
		}
		if len(page1) != 2 {
			t.Fatalf("expected 2, got %d", len(page1))
		}

		// Offset 2
		page2, err := st.ListThreadsForInbox(ctx, inboxID, "", 2, 2)
		if err != nil {
			t.Fatalf("page2: %v", err)
		}
		if len(page2) != 2 {
			t.Fatalf("expected 2, got %d", len(page2))
		}

		// No overlap
		if page1[0].ID == page2[0].ID || page1[1].ID == page2[0].ID {
			t.Fatalf("page overlap detected")
		}
	})
}

func TestListThreadsForInboxDefaultLimitAndOffset(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)

		st := &Store{db: db, q: db}
		orgID := uuid.NewString()
		inboxID := uuid.NewString()

		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'acme')`, orgID); err != nil {
			t.Fatalf("insert org: %v", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO inboxes (id, org_id, address, status) VALUES ($1, $2, 'test@example.com', 'active')`, inboxID, orgID); err != nil {
			t.Fatalf("insert inbox: %v", err)
		}

		// Negative limit defaults to 50, negative offset defaults to 0
		threads, err := st.ListThreadsForInbox(ctx, inboxID, "", -1, -5)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if threads == nil {
			// nil is ok for empty result
		}
		if len(threads) != 0 {
			t.Fatalf("expected 0 threads, got %d", len(threads))
		}
	})
}

func TestListOutboxForInboxBasic(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)

		st := &Store{db: db, q: db}
		orgID := uuid.NewString()
		inboxID := uuid.NewString()

		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'acme')`, orgID); err != nil {
			t.Fatalf("insert org: %v", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO inboxes (id, org_id, address, status) VALUES ($1, $2, 'test@example.com', 'active')`, inboxID, orgID); err != nil {
			t.Fatalf("insert inbox: %v", err)
		}

		// Enqueue some messages
		id1, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID:          orgID,
			InboxID:        inboxID,
			Provider:       "resend",
			IdempotencyKey: "k1",
			To:             "to@test.com",
			From:           "test@example.com",
			Subject:        "Subject 1",
			TextBody:       "body 1",
		})
		if err != nil {
			t.Fatalf("enqueue 1: %v", err)
		}
		id2, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID:          orgID,
			InboxID:        inboxID,
			Provider:       "resend",
			IdempotencyKey: "k2",
			To:             "to@test.com",
			From:           "test@example.com",
			Subject:        "Subject 2",
			TextBody:       "body 2",
		})
		if err != nil {
			t.Fatalf("enqueue 2: %v", err)
		}

		messages, err := st.ListOutboxForInbox(ctx, inboxID, 50, 0)
		if err != nil {
			t.Fatalf("list outbox: %v", err)
		}
		if len(messages) != 2 {
			t.Fatalf("expected 2, got %d", len(messages))
		}

		// Should be ordered by created_at DESC
		ids := map[string]bool{messages[0].ID: true, messages[1].ID: true}
		if !ids[id1] || !ids[id2] {
			t.Fatalf("expected both IDs present, got %v", ids)
		}

		// Check fields are populated
		for _, m := range messages {
			if m.OrgID != orgID {
				t.Fatalf("expected org_id %s, got %s", orgID, m.OrgID)
			}
			if m.Subject == "" {
				t.Fatalf("expected non-empty subject")
			}
		}
	})
}

func TestListOutboxForInboxPagination(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)

		st := &Store{db: db, q: db}
		orgID := uuid.NewString()
		inboxID := uuid.NewString()

		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'acme')`, orgID); err != nil {
			t.Fatalf("insert org: %v", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO inboxes (id, org_id, address, status) VALUES ($1, $2, 'test@example.com', 'active')`, inboxID, orgID); err != nil {
			t.Fatalf("insert inbox: %v", err)
		}

		for i := 0; i < 5; i++ {
			if _, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
				OrgID:          orgID,
				InboxID:        inboxID,
				Provider:       "resend",
				IdempotencyKey: "key-" + uuid.NewString(),
				To:             "to@test.com",
				From:           "test@example.com",
				Subject:        "Msg",
				TextBody:       "body-" + uuid.NewString(),
			}); err != nil {
				t.Fatalf("enqueue %d: %v", i, err)
			}
		}

		page1, err := st.ListOutboxForInbox(ctx, inboxID, 2, 0)
		if err != nil {
			t.Fatalf("page1: %v", err)
		}
		if len(page1) != 2 {
			t.Fatalf("expected 2, got %d", len(page1))
		}

		page2, err := st.ListOutboxForInbox(ctx, inboxID, 2, 2)
		if err != nil {
			t.Fatalf("page2: %v", err)
		}
		if len(page2) != 2 {
			t.Fatalf("expected 2, got %d", len(page2))
		}

		if page1[0].ID == page2[0].ID {
			t.Fatalf("page overlap detected")
		}
	})
}

func TestListOutboxForInboxEmpty(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)

		st := &Store{db: db, q: db}
		orgID := uuid.NewString()
		inboxID := uuid.NewString()

		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'acme')`, orgID); err != nil {
			t.Fatalf("insert org: %v", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO inboxes (id, org_id, address, status) VALUES ($1, $2, 'test@example.com', 'active')`, inboxID, orgID); err != nil {
			t.Fatalf("insert inbox: %v", err)
		}

		messages, err := st.ListOutboxForInbox(ctx, inboxID, 50, 0)
		if err != nil {
			t.Fatalf("list outbox: %v", err)
		}
		if len(messages) != 0 {
			t.Fatalf("expected 0, got %d", len(messages))
		}
	})
}
