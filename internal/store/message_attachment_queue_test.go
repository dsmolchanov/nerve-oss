package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
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
			if attachment.ReceivedEmailID != "received-"+messageID {
				t.Fatalf("received email id=%q, want queue source", attachment.ReceivedEmailID)
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
		terminal, err := st.RequeueMessageAttachment(ctx, claimed[0].ID, "worker-a", claimed[0].LockedAt.Time, next, "temporary")
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
		terminal, err = st.RequeueMessageAttachment(ctx, claimed[0].ID, "worker-b", claimed[0].LockedAt.Time, next, "exhausted")
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

func TestClaimMessageAttachmentsTerminalizesExpiredSixthLease(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		st, _, _ := seedMessageAttachmentQueue(t, ctx, db, []string{"crashed-sixth"})
		now := time.Now().UTC()
		var attachmentID string
		if err := db.QueryRowContext(ctx, `
			UPDATE message_attachments
			SET attempt_count = $1
			WHERE provider_attachment_id = 'crashed-sixth'
			RETURNING id::text
		`, MaxMessageAttachmentAttempts-1).Scan(&attachmentID); err != nil {
			t.Fatal(err)
		}
		claimed, err := st.ClaimMessageAttachments(ctx, 1, "worker", now, 5*time.Minute)
		if err != nil || len(claimed) != 1 || claimed[0].AttemptCount != MaxMessageAttachmentAttempts {
			t.Fatalf("sixth claim=%v err=%v", claimed, err)
		}

		claimed, err = st.ClaimMessageAttachments(ctx, 1, "worker", now.Add(6*time.Minute), 5*time.Minute)
		if err != nil || len(claimed) != 0 {
			t.Fatalf("expired sixth reclaim=%v err=%v, want no work", claimed, err)
		}
		var availability, lastError string
		var attempts int
		var locked bool
		if err := db.QueryRowContext(ctx, `
			SELECT availability, attempt_count, last_error,
			       locked_at IS NOT NULL OR locked_by IS NOT NULL
			FROM message_attachments WHERE id = $1
		`, attachmentID).Scan(&availability, &attempts, &lastError, &locked); err != nil {
			t.Fatal(err)
		}
		if availability != "failed" || attempts != MaxMessageAttachmentAttempts || locked || !strings.Contains(lastError, "retry budget exhausted") {
			t.Fatalf("availability=%q attempts=%d locked=%v last_error=%q", availability, attempts, locked, lastError)
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
		if err := st.MarkMessageAttachmentAvailable(ctx, byProvider["available"].ID, "worker", byProvider["available"].LockedAt.Time, digest, now); err != nil {
			t.Fatal(err)
		}
		if err := st.MarkMessageAttachmentTerminal(ctx, byProvider["expired"].ID, "worker", byProvider["expired"].LockedAt.Time, "expired", "provider returned 404"); err != nil {
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

func TestMessageAttachmentCompletionRejectsStaleLeaseFromSameWorker(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		st, _, _ := seedMessageAttachmentQueue(t, ctx, db, []string{"lease"})
		now := time.Now().UTC()
		first, err := st.ClaimMessageAttachments(ctx, 1, "worker", now, 5*time.Minute)
		if err != nil || len(first) != 1 {
			t.Fatalf("first claim=%v err=%v", first, err)
		}
		second, err := st.ClaimMessageAttachments(ctx, 1, "worker", now.Add(6*time.Minute), 5*time.Minute)
		if err != nil || len(second) != 1 {
			t.Fatalf("second claim=%v err=%v", second, err)
		}
		if err := st.MarkMessageAttachmentTerminal(ctx, first[0].ID, "worker", first[0].LockedAt.Time, "expired", "stale"); !errors.Is(err, ErrAttachmentLeaseLost) {
			t.Fatalf("stale completion err=%v, want lease lost", err)
		}
		if _, err := st.RequeueMessageAttachment(ctx, first[0].ID, "worker", first[0].LockedAt.Time, now.Add(time.Hour), "stale"); !errors.Is(err, ErrAttachmentLeaseLost) {
			t.Fatalf("stale requeue err=%v, want lease lost", err)
		}
		if err := st.MarkMessageAttachmentTerminal(ctx, second[0].ID, "worker", second[0].LockedAt.Time, "expired", "current"); err != nil {
			t.Fatal(err)
		}
	})
}

func TestMarkMessageAttachmentTerminalRejectsInvalidStateBeforeSQL(t *testing.T) {
	st := &Store{}
	if err := st.MarkMessageAttachmentTerminal(context.Background(), uuid.NewString(), "worker", time.Now(), "available", ""); err == nil {
		t.Fatal("non-terminal availability unexpectedly accepted")
	}
}

func TestStoreMirroredMessageAttachmentCommitsBlobAndReferenceAtomically(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		st, orgID, _ := seedMessageAttachmentQueue(t, ctx, db, []string{"success", "lost-lease"})
		now := time.Now().UTC()
		claimed, err := st.ClaimMessageAttachments(ctx, 2, "worker-a", now, 5*time.Minute)
		if err != nil || len(claimed) != 2 {
			t.Fatalf("claimed=%v err=%v", claimed, err)
		}
		byProviderID := make(map[string]MessageAttachment, len(claimed))
		for _, attachment := range claimed {
			byProviderID[attachment.ProviderAttachmentID] = attachment
		}

		content := []byte("atomic content")
		digest, err := st.StoreMirroredMessageAttachment(
			ctx,
			orgID,
			byProviderID["success"].ID,
			"worker-a",
			byProviderID["success"].LockedAt.Time,
			"text/plain",
			content,
			now,
		)
		if err != nil {
			t.Fatal(err)
		}
		info, err := st.GetAttachmentBlobInfo(ctx, orgID, digest)
		if err != nil || info.RefCount != 1 || info.SizeBytes != int64(len(content)) {
			t.Fatalf("blob info=%+v err=%v", info, err)
		}
		var availability, referencedDigest string
		if err := db.QueryRowContext(ctx, `
			SELECT availability, blob_sha256 FROM message_attachments WHERE id = $1
		`, byProviderID["success"].ID).Scan(&availability, &referencedDigest); err != nil {
			t.Fatal(err)
		}
		if availability != "available" || referencedDigest != digest {
			t.Fatalf("availability=%q digest=%q", availability, referencedDigest)
		}

		orphanDigest, err := st.StoreMirroredMessageAttachment(
			ctx,
			orgID,
			byProviderID["lost-lease"].ID,
			"wrong-worker",
			byProviderID["lost-lease"].LockedAt.Time,
			"text/plain",
			[]byte("must roll back"),
			now,
		)
		if !errors.Is(err, ErrAttachmentLeaseLost) {
			t.Fatalf("lost lease err=%v", err)
		}
		var orphanCount int
		if err := db.QueryRowContext(ctx, `
			SELECT count(*) FROM attachment_blobs WHERE org_id = $1 AND sha256 = $2
		`, orgID, orphanDigest).Scan(&orphanCount); err != nil {
			t.Fatal(err)
		}
		if orphanCount != 0 {
			t.Fatalf("lost lease committed %d orphan blobs", orphanCount)
		}
		assertAttachmentUsage(t, ctx, db, orgID, int64(len(content)), 1)
	})
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
