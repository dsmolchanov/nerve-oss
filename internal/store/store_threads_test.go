package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestInsertMessageWithThreadReplayDoesNotCreateOrphanThread(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, err := st.CreateOrg(ctx, "thread-replay-"+uuid.NewString()[:8])
		if err != nil {
			t.Fatalf("create org: %v", err)
		}
		domain := "replay-" + uuid.NewString()[:8] + ".example"
		domainID, err := st.CreateOrgDomain(ctx, orgID, domain, "verify", "selector", "private", "public", "cname")
		if err != nil {
			t.Fatalf("create domain: %v", err)
		}
		if err := st.UpdateOrgDomainStatus(ctx, domainID, "active"); err != nil {
			t.Fatalf("activate domain: %v", err)
		}
		inbox, err := st.CreateInboxForOrg(ctx, orgID, "agent@"+domain, domainID, "resend")
		if err != nil {
			t.Fatalf("create inbox: %v", err)
		}

		message := Message{
			Direction:         "inbound",
			Subject:           "Replay",
			Text:              "same provider delivery",
			ProviderMessageID: "provider-" + uuid.NewString(),
			From:              Participant{Email: "sender@example.com"},
			To:                []Participant{{Email: inbox.Address}},
			CreatedAt:         time.Now().UTC(),
		}
		firstThreadID, firstMessageID, err := st.InsertMessageWithThread(ctx, inbox.ID, "", message)
		if err != nil {
			t.Fatalf("first insert: %v", err)
		}
		secondThreadID, secondMessageID, err := st.InsertMessageWithThread(ctx, inbox.ID, "", message)
		if err != nil {
			t.Fatalf("replay insert: %v", err)
		}
		if secondThreadID != firstThreadID || secondMessageID != firstMessageID {
			t.Fatalf("replay IDs = thread:%s message:%s, want %s/%s", secondThreadID, secondMessageID, firstThreadID, firstMessageID)
		}

		var threadCount, messageCount int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM threads WHERE inbox_id = $1`, inbox.ID).Scan(&threadCount); err != nil {
			t.Fatalf("count threads: %v", err)
		}
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE inbox_id = $1`, inbox.ID).Scan(&messageCount); err != nil {
			t.Fatalf("count messages: %v", err)
		}
		if threadCount != 1 || messageCount != 1 {
			t.Fatalf("replay rows = threads:%d messages:%d, want 1/1", threadCount, messageCount)
		}
	})
}

func TestConcurrentInsertMessageWithThreadReplayUsesOneThread(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, err := st.CreateOrg(ctx, "thread-concurrent-"+uuid.NewString()[:8])
		if err != nil {
			t.Fatalf("create org: %v", err)
		}
		domain := "thread-concurrent-" + uuid.NewString()[:8] + ".example"
		domainID, err := st.CreateOrgDomain(ctx, orgID, domain, "verify", "selector", "private", "public", "cname")
		if err != nil {
			t.Fatalf("create domain: %v", err)
		}
		if err := st.UpdateOrgDomainStatus(ctx, domainID, "active"); err != nil {
			t.Fatalf("activate domain: %v", err)
		}
		inbox, err := st.CreateInboxForOrg(ctx, orgID, "agent@"+domain, domainID, "resend")
		if err != nil {
			t.Fatalf("create inbox: %v", err)
		}
		message := Message{
			Direction: "inbound", ProviderMessageID: "provider-" + uuid.NewString(),
			From: Participant{Email: "sender@example.com"}, CreatedAt: time.Now().UTC(),
		}

		type result struct {
			threadID, messageID string
			err                 error
		}
		start := make(chan struct{})
		results := make(chan result, 2)
		for range 2 {
			go func() {
				<-start
				threadID, messageID, insertErr := st.InsertMessageWithThread(ctx, inbox.ID, "", message)
				results <- result{threadID: threadID, messageID: messageID, err: insertErr}
			}()
		}
		close(start)
		first, second := <-results, <-results
		if first.err != nil || second.err != nil {
			t.Fatalf("concurrent inserts = first:%v second:%v", first.err, second.err)
		}
		if first.threadID != second.threadID || first.messageID != second.messageID {
			t.Fatalf("concurrent IDs = %+v / %+v", first, second)
		}
		var threadCount, messageCount int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM threads WHERE inbox_id = $1`, inbox.ID).Scan(&threadCount); err != nil {
			t.Fatalf("count threads: %v", err)
		}
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE inbox_id = $1`, inbox.ID).Scan(&messageCount); err != nil {
			t.Fatalf("count messages: %v", err)
		}
		if threadCount != 1 || messageCount != 1 {
			t.Fatalf("concurrent rows = threads:%d messages:%d, want 1/1", threadCount, messageCount)
		}
	})
}
