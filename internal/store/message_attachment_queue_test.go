package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestClaimMessageAttachmentsHonorsScheduleAndLeaseExpiry(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		st, orgID, messageID := seedMessageAttachmentQueue(t, ctx, db, []string{"due", "future", "fresh", "stale"})
		now := time.Now().UTC()
		if _, err := db.ExecContext(ctx, `
			UPDATE message_attachments
			SET next_attempt_at = CASE provider_attachment_id
			      WHEN 'future' THEN $2::timestamptz
			      ELSE $1::timestamptz
			    END,
			    locked_at = CASE provider_attachment_id
			      WHEN 'fresh' THEN $1::timestamptz - interval '1 minute'
			      WHEN 'stale' THEN $1::timestamptz - interval '10 minutes'
			    END,
			    locked_by = CASE
			      WHEN provider_attachment_id IN ('fresh', 'stale') THEN 'previous-worker'
			    END
			WHERE org_id = $3 AND message_id = $4
		`, now, now.Add(time.Hour), orgID, messageID); err != nil {
			t.Fatal(err)
		}

		claimed, err := st.ClaimMessageAttachments(ctx, 10, "worker-a", now, 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		assertClaimedProviderIDs(t, claimed, "due", "stale")
		for _, attachment := range claimed {
			if attachment.AttemptCount != 1 || !attachment.LockedBy.Valid || attachment.LockedBy.String != "worker-a" {
				t.Fatalf("claimed attachment=%+v", attachment)
			}
		}

		claimed, err = st.ClaimMessageAttachments(ctx, 10, "worker-b", now, 5*time.Minute)
		if err != nil || len(claimed) != 0 {
			t.Fatalf("fresh re-claim=%v err=%v, want none", claimed, err)
		}

		claimed, err = st.ClaimMessageAttachments(ctx, 10, "worker-b", now.Add(6*time.Minute), 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		assertClaimedProviderIDs(t, claimed, "due", "fresh", "stale")
	})
}

func TestRequeueMessageAttachmentBacksOffAndFailsSixthAttempt(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		st, _, _ := seedMessageAttachmentQueue(t, ctx, db, []string{"retry"})
		now := time.Now().UTC()
		claimed, err := st.ClaimMessageAttachments(ctx, 1, "worker-a", now, 5*time.Minute)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("first claim=%v err=%v", claimed, err)
		}
		next := now.Add(time.Minute)
		terminal, err := st.RequeueMessageAttachment(ctx, claimed[0].ID, "worker-a", next, "temporary")
		if err != nil || terminal {
			t.Fatalf("first retry terminal=%v err=%v", terminal, err)
		}
		if early, err := st.ClaimMessageAttachments(ctx, 1, "worker-b", now.Add(30*time.Second), 5*time.Minute); err != nil || len(early) != 0 {
			t.Fatalf("early claim=%v err=%v", early, err)
		}

		if _, err := db.ExecContext(ctx, `
			UPDATE message_attachments
			SET attempt_count = 5, next_attempt_at = $2
			WHERE id = $1
		`, claimed[0].ID, now); err != nil {
			t.Fatal(err)
		}
		claimed, err = st.ClaimMessageAttachments(ctx, 1, "worker-b", now, 5*time.Minute)
		if err != nil || len(claimed) != 1 || claimed[0].AttemptCount != MaxMessageAttachmentAttempts {
			t.Fatalf("sixth claim=%v err=%v", claimed, err)
		}
		terminal, err = st.RequeueMessageAttachment(ctx, claimed[0].ID, "worker-b", next, "exhausted")
		if err != nil || !terminal {
			t.Fatalf("sixth retry terminal=%v err=%v", terminal, err)
		}
		var availability, lastError string
		var locked bool
		if err := db.QueryRowContext(ctx, `
			SELECT availability, last_error, locked_at IS NOT NULL
			FROM message_attachments WHERE id = $1
		`, claimed[0].ID).Scan(&availability, &lastError, &locked); err != nil {
			t.Fatal(err)
		}
		if availability != "failed" || lastError != "exhausted" || locked {
			t.Fatalf("availability=%q last_error=%q locked=%v", availability, lastError, locked)
		}
	})
}

func TestCompleteMessageAttachmentMirrorUpdatesAvailabilityAndRefCount(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		st, orgID, messageID := seedMessageAttachmentQueue(t, ctx, db, []string{"available", "expired"})
		now := time.Now().UTC()
		claimed, err := st.ClaimMessageAttachments(ctx, 10, "worker", now, 5*time.Minute)
		if err != nil || len(claimed) != 2 {
			t.Fatalf("claim=%v err=%v", claimed, err)
		}
		byProvider := make(map[string]MessageAttachment, len(claimed))
		for _, attachment := range claimed {
			byProvider[attachment.ProviderAttachmentID] = attachment
		}
		digest, _, err := st.StoreAttachmentBlob(ctx, orgID, "text/plain", []byte("mirrored"))
		if err != nil {
			t.Fatal(err)
		}
		if err := st.MarkMessageAttachmentAvailable(ctx, byProvider["available"].ID, "worker", digest, now); err != nil {
			t.Fatal(err)
		}
		if err := st.MarkMessageAttachmentTerminal(ctx, byProvider["expired"].ID, "worker", "expired", "provider returned 404"); err != nil {
			t.Fatal(err)
		}
		assertBlobRefCount(t, ctx, db, orgID, digest, 1)
		attachments, err := st.ListMessageAttachments(ctx, orgID, messageID)
		if err != nil {
			t.Fatal(err)
		}
		states := map[string]string{}
		for _, attachment := range attachments {
			states[attachment.ProviderAttachmentID] = attachment.Availability
			if attachment.LockedAt.Valid || attachment.LockedBy.Valid {
				t.Fatalf("completed attachment retained lease: %+v", attachment)
			}
		}
		if states["available"] != "available" || states["expired"] != "expired" {
			t.Fatalf("states=%v", states)
		}
	})
}

func TestMessageAttachmentCompletionRejectsStaleWorker(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		st, _, _ := seedMessageAttachmentQueue(t, ctx, db, []string{"lease"})
		now := time.Now().UTC()
		first, err := st.ClaimMessageAttachments(ctx, 1, "worker-a", now, 5*time.Minute)
		if err != nil || len(first) != 1 {
			t.Fatalf("first claim=%v err=%v", first, err)
		}
		second, err := st.ClaimMessageAttachments(ctx, 1, "worker-b", now.Add(6*time.Minute), 5*time.Minute)
		if err != nil || len(second) != 1 {
			t.Fatalf("second claim=%v err=%v", second, err)
		}
		if err := st.MarkMessageAttachmentTerminal(ctx, first[0].ID, "worker-a", "expired", "stale"); !errors.Is(err, ErrAttachmentLeaseLost) {
			t.Fatalf("stale completion err=%v, want lease lost", err)
		}
		if err := st.MarkMessageAttachmentTerminal(ctx, second[0].ID, "worker-b", "expired", "current"); err != nil {
			t.Fatal(err)
		}
	})
}

func TestMarkMessageAttachmentTerminalRejectsInvalidStateBeforeSQL(t *testing.T) {
	st := &Store{}
	if err := st.MarkMessageAttachmentTerminal(context.Background(), uuid.NewString(), "worker", "available", ""); err == nil {
		t.Fatal("non-terminal availability unexpectedly accepted")
	}
}

func seedMessageAttachmentQueue(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	providerIDs []string,
) (*Store, string, string) {
	t.Helper()
	migrateToLatest(t, ctx, db)
	st := &Store{db: db, q: db}
	orgID, inboxID, threadID := seedAttachmentMessageParents(t, ctx, db, "mirror-queue-"+uuid.NewString())
	messageID := uuid.NewString()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO messages (id, org_id, inbox_id, thread_id, direction, received_email_id)
		VALUES ($1, $2, $3, $4, 'inbound', $5)
	`, messageID, orgID, inboxID, threadID, "received-"+messageID); err != nil {
		t.Fatal(err)
	}
	metadata := make([]MessageAttachmentMetadata, 0, len(providerIDs))
	for _, providerID := range providerIDs {
		metadata = append(metadata, MessageAttachmentMetadata{ProviderAttachmentID: providerID})
	}
	if err := st.SetMessageAttachmentsKnown(ctx, orgID, messageID, metadata); err != nil {
		t.Fatal(err)
	}
	return st, orgID, messageID
}

func assertClaimedProviderIDs(t *testing.T, attachments []MessageAttachment, want ...string) {
	t.Helper()
	got := make(map[string]bool, len(attachments))
	for _, attachment := range attachments {
		got[attachment.ProviderAttachmentID] = true
	}
	if len(got) != len(want) {
		t.Fatalf("claimed provider ids=%v, want %v", got, want)
	}
	for _, providerID := range want {
		if !got[providerID] {
			t.Fatalf("claimed provider ids=%v, missing %q", got, providerID)
		}
	}
}
