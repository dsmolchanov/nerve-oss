package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMessageAttachmentWorkerLeaseMirrorAndLoad(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, inboxID, threadID := seedAttachmentMessageParents(t, ctx, db, "worker-flow")
		messageID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `
			INSERT INTO messages (id, org_id, inbox_id, thread_id, direction, received_email_id)
			VALUES ($1, $2, $3, $4, 'inbound', 'received-worker-flow')
		`, messageID, orgID, inboxID, threadID); err != nil {
			t.Fatal(err)
		}
		if err := st.PersistInboundAttachmentMetadata(ctx, orgID, messageID, []InboundAttachmentMetadata{{
			ProviderAttachmentID: "provider-worker-flow",
			Filename:             "flow.pdf",
			ContentType:          "application/pdf",
		}}); err != nil {
			t.Fatal(err)
		}

		claimed, err := st.ClaimMessageAttachments(ctx, 1, "worker-test", time.Now().UTC(), time.Minute)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claimed=%v err=%v", claimed, err)
		}
		if claimed[0].ReceivedEmailID != "received-worker-flow" || claimed[0].AttemptCount != 1 {
			t.Fatalf("claimed attachment=%+v", claimed[0])
		}

		content := []byte("durable-pdf-content")
		digest, err := st.StoreMirroredMessageAttachment(
			ctx, orgID, claimed[0].ID, claimed[0].LockedBy.String, claimed[0].LockedAt.Time, "application/pdf", content,
			time.Now().UTC(),
		)
		if err != nil || digest == "" {
			t.Fatalf("digest=%q err=%v", digest, err)
		}
		attachment, loaded, err := st.LoadMessageAttachmentContent(ctx, orgID, messageID, claimed[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if attachment.Availability != "available" || !attachment.BlobSHA256.Valid || attachment.BlobSHA256.String != digest {
			t.Fatalf("mirrored attachment=%+v", attachment)
		}
		if !bytes.Equal(loaded, content) {
			t.Fatalf("loaded content=%q, want %q", loaded, content)
		}
		if err := st.PersistInboundAttachmentMetadata(ctx, orgID, messageID, []InboundAttachmentMetadata{{
			ProviderAttachmentID: "provider-worker-flow",
			Filename:             "flow.pdf",
			ContentType:          "application/pdf",
		}}); err != nil {
			t.Fatal(err)
		}
		attachment, loaded, err = st.LoadMessageAttachmentContent(ctx, orgID, messageID, claimed[0].ID)
		if err != nil || !attachment.SizeBytes.Valid || attachment.SizeBytes.Int64 != int64(len(content)) || !bytes.Equal(loaded, content) {
			t.Fatalf("envelope replay changed durable result: attachment=%+v content=%q err=%v", attachment, loaded, err)
		}
		if err := st.MarkMessageAttachmentTerminal(ctx, claimed[0].ID, claimed[0].LockedBy.String, claimed[0].LockedAt.Time, "failed", "late worker"); !errors.Is(err, ErrAttachmentLeaseLost) {
			t.Fatalf("late terminal update error=%v, want lease lost", err)
		}
		attachment, loaded, err = st.LoadMessageAttachmentContent(ctx, orgID, messageID, claimed[0].ID)
		if err != nil || attachment.Availability != "available" || !bytes.Equal(loaded, content) {
			t.Fatalf("late worker changed durable result: attachment=%+v content=%q err=%v", attachment, loaded, err)
		}
	})
}
