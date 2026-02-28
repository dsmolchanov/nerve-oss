package store

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestOutboxIdempotencyUniqueness(t *testing.T) {
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

		id1, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID:          orgID,
			InboxID:        inboxID,
			Provider:       "smtp",
			IdempotencyKey: "k1",
			To:             "to@local.neuralmail",
			From:           "a@local.neuralmail",
			Subject:        "hello",
			TextBody:       "test",
		})
		if err != nil {
			t.Fatalf("enqueue #1: %v", err)
		}
		id2, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID:          orgID,
			InboxID:        inboxID,
			Provider:       "smtp",
			IdempotencyKey: "k1",
			To:             "to@local.neuralmail",
			From:           "a@local.neuralmail",
			Subject:        "hello",
			TextBody:       "test",
		})
		if err != nil {
			t.Fatalf("enqueue #2: %v", err)
		}
		if id1 != id2 {
			t.Fatalf("expected same outbox id on conflict, got %s vs %s", id1, id2)
		}
	})
}

func TestOutboxContentDedup(t *testing.T) {
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

		// Two messages with same content but different idempotency keys
		// should return the same outbox ID (content dedup)
		id1, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID:          orgID,
			InboxID:        inboxID,
			Provider:       "smtp",
			IdempotencyKey: "key-a",
			To:             "to@local.neuralmail",
			From:           "a@local.neuralmail",
			Subject:        "hello",
			TextBody:       "same body",
		})
		if err != nil {
			t.Fatalf("enqueue #1: %v", err)
		}
		id2, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID:          orgID,
			InboxID:        inboxID,
			Provider:       "smtp",
			IdempotencyKey: "key-b",
			To:             "to@local.neuralmail",
			From:           "a@local.neuralmail",
			Subject:        "hello",
			TextBody:       "same body",
		})
		if err != nil {
			t.Fatalf("enqueue #2: %v", err)
		}
		if id1 != id2 {
			t.Fatalf("expected same outbox id from content dedup, got %s vs %s", id1, id2)
		}
	})
}

func TestOutboxContentDedupAllowsResendAfterSent(t *testing.T) {
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

		// Enqueue and mark as sent
		id1, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID:          orgID,
			InboxID:        inboxID,
			Provider:       "smtp",
			IdempotencyKey: "key-1",
			To:             "to@local.neuralmail",
			From:           "a@local.neuralmail",
			Subject:        "hello",
			TextBody:       "resend body",
		})
		if err != nil {
			t.Fatalf("enqueue #1: %v", err)
		}
		if err := st.MarkOutboxMessageSent(ctx, id1, "provider-msg-1"); err != nil {
			t.Fatalf("mark sent: %v", err)
		}

		// Same content should now be allowed (legitimate re-send)
		id2, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID:          orgID,
			InboxID:        inboxID,
			Provider:       "smtp",
			IdempotencyKey: "key-2",
			To:             "to@local.neuralmail",
			From:           "a@local.neuralmail",
			Subject:        "hello",
			TextBody:       "resend body",
		})
		if err != nil {
			t.Fatalf("enqueue #2 after sent: %v", err)
		}
		if id1 == id2 {
			t.Fatalf("expected different outbox id for re-send after original was sent, got same: %s", id1)
		}
	})
}

func TestOutboxClaimQueryIsConcurrencySafe(t *testing.T) {
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

		const total = 50
		for i := 0; i < total; i++ {
			_, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
				OrgID:          orgID,
				InboxID:        inboxID,
				Provider:       "smtp",
				IdempotencyKey: fmt.Sprintf("k-%d", i),
				To:             "to@local.neuralmail",
				From:           "a@local.neuralmail",
				Subject:        "hello",
				TextBody:       "test",
			})
			if err != nil {
				t.Fatalf("enqueue %d: %v", i, err)
			}
		}

		now := time.Now().UTC()
		claimed := make(map[string]bool, total)
		var mu sync.Mutex

		workers := 5
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				msgs, err := st.ClaimOutboxMessages(ctx, 20, fmt.Sprintf("w-%d", i), now)
				if err != nil {
					t.Errorf("claim worker %d: %v", i, err)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				for _, m := range msgs {
					if claimed[m.ID] {
						t.Errorf("duplicate claim for id=%s", m.ID)
					}
					claimed[m.ID] = true
				}
			}(i)
		}
		wg.Wait()

		if len(claimed) != total {
			t.Fatalf("expected %d unique claimed messages, got %d", total, len(claimed))
		}
	})
}
