package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestInsertMessageClassifiesProviderlessAttachmentState(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		_, inboxID, threadID := seedAttachmentMessageParents(t, ctx, db, "insert-classification")
		messages := []struct {
			direction       string
			receivedEmailID string
			want            string
		}{
			{direction: "outbound", receivedEmailID: "outbound-provider", want: "known"},
			{direction: "inbound", want: "known"},
			{direction: "inbound", receivedEmailID: "received-provider", want: "pending_backfill"},
		}
		for index, input := range messages {
			id, err := st.InsertMessage(ctx, Message{
				ID:              uuid.NewString(),
				InboxID:         inboxID,
				ThreadID:        threadID,
				Direction:       input.direction,
				CreatedAt:       time.Now().UTC().Add(time.Duration(index) * time.Second),
				ReceivedEmailID: input.receivedEmailID,
			})
			if err != nil {
				t.Fatal(err)
			}
			var state string
			if err := db.QueryRowContext(ctx, `SELECT attachments_state FROM messages WHERE id = $1`, id).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if state != input.want {
				t.Fatalf("direction=%q received_email_id=%q state=%q, want %q", input.direction, input.receivedEmailID, state, input.want)
			}
		}
	})
}

func TestMessageReadsExposeAttachmentState(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, inboxID, threadID := seedAttachmentMessageParents(t, ctx, db, "read-state")
		messageID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `
			INSERT INTO messages
			  (id, org_id, inbox_id, thread_id, direction, received_email_id, attachments_state,
			   subject, text, html, provider_message_id, internet_message_id, from_json, to_json, cc_json)
			VALUES ($1, $2, $3, $4, 'inbound', 'received-read-state', 'unknown_metadata_expired',
			        '', '', '', '', '', '{}', '[]', '[]')
		`, messageID, orgID, inboxID, threadID); err != nil {
			t.Fatal(err)
		}

		message, err := st.GetMessage(ctx, messageID)
		if err != nil {
			t.Fatal(err)
		}
		if message.AttachmentsState != "unknown_metadata_expired" {
			t.Fatalf("GetMessage attachments_state=%q", message.AttachmentsState)
		}

		_, messages, err := st.GetThread(ctx, threadID)
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) != 1 || messages[0].AttachmentsState != "unknown_metadata_expired" {
			t.Fatalf("GetThread messages=%+v", messages)
		}
	})
}

func TestMessageReadsRemainCompatibleBeforeAttachmentStateMigration(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToVersion(t, ctx, db, 22)
		st := &Store{db: db, q: db}
		orgID, inboxID, threadID := seedAttachmentMessageParents(t, ctx, db, "read-state-core22")
		messageID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `
			INSERT INTO messages
			  (id, org_id, inbox_id, thread_id, direction, received_email_id,
			   subject, text, html, provider_message_id, internet_message_id, from_json, to_json, cc_json)
			VALUES ($1, $2, $3, $4, 'inbound', 'received-core22',
			        '', '', '', '', '', '{}', '[]', '[]')
		`, messageID, orgID, inboxID, threadID); err != nil {
			t.Fatal(err)
		}

		message, err := st.GetMessage(ctx, messageID)
		if err != nil || message.AttachmentsState != "known" {
			t.Fatalf("GetMessage message=%+v err=%v", message, err)
		}
		_, messages, err := st.GetThread(ctx, threadID)
		if err != nil || len(messages) != 1 || messages[0].AttachmentsState != "known" {
			t.Fatalf("GetThread messages=%+v err=%v", messages, err)
		}
	})
}

func TestSetMessageAttachmentsKnownPersistsMetadataAndIsReplaySafe(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, inboxID, threadID := seedAttachmentMessageParents(t, ctx, db, "metadata")
		messageID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `
			INSERT INTO messages (id, org_id, inbox_id, thread_id, direction, received_email_id)
			VALUES ($1, $2, $3, $4, 'inbound', 'received-metadata')
		`, messageID, orgID, inboxID, threadID); err != nil {
			t.Fatal(err)
		}
		size := int64(42)
		metadata := []MessageAttachmentMetadata{
			{
				ProviderAttachmentID: "provider-1",
				Filename:             "one.txt",
				ContentType:          "text/plain",
				ContentDisposition:   "attachment",
				ContentID:            "cid-one",
				SizeBytes:            &size,
			},
			{ProviderAttachmentID: "provider-2", Filename: "two.bin"},
		}
		if err := st.SetMessageAttachmentsKnown(ctx, orgID, messageID, metadata); err != nil {
			t.Fatal(err)
		}
		attachments, err := st.ListMessageAttachments(ctx, orgID, messageID)
		if err != nil || len(attachments) != 2 {
			t.Fatalf("attachments=%v err=%v", attachments, err)
		}
		if attachments[0].Ordinal != 0 || attachments[0].Filename != "one.txt" ||
			!attachments[0].SizeBytes.Valid || attachments[0].SizeBytes.Int64 != 42 {
			t.Fatalf("first attachment=%+v", attachments[0])
		}
		if attachments[1].Ordinal != 1 || attachments[1].ContentType != "application/octet-stream" {
			t.Fatalf("second attachment=%+v", attachments[1])
		}
		attachment, err := st.GetMessageAttachment(ctx, orgID, messageID, attachments[0].ID)
		if err != nil || attachment.ProviderAttachmentID != "provider-1" {
			t.Fatalf("GetMessageAttachment attachment=%+v err=%v", attachment, err)
		}
		if _, err := st.GetMessageAttachment(ctx, uuid.NewString(), messageID, attachments[0].ID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("cross-org GetMessageAttachment err=%v, want sql.ErrNoRows", err)
		}
		var state string
		if err := db.QueryRowContext(ctx, `SELECT attachments_state FROM messages WHERE id = $1`, messageID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "known" {
			t.Fatalf("attachments_state=%q, want known", state)
		}

		metadata[0].Filename = "one-renamed.txt"
		if err := st.SetMessageAttachmentsKnown(ctx, orgID, messageID, metadata); err != nil {
			t.Fatal(err)
		}
		attachments, err = st.ListMessageAttachments(ctx, orgID, messageID)
		if err != nil || len(attachments) != 2 || attachments[0].Filename != "one-renamed.txt" {
			t.Fatalf("replayed attachments=%v err=%v", attachments, err)
		}
	})
}

func TestSetMessageAttachmentsKnownRecordsKnownEmptyEnvelope(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, inboxID, threadID := seedAttachmentMessageParents(t, ctx, db, "metadata-empty")
		messageID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `
			INSERT INTO messages (id, org_id, inbox_id, thread_id, direction, received_email_id)
			VALUES ($1, $2, $3, $4, 'inbound', 'received-empty')
		`, messageID, orgID, inboxID, threadID); err != nil {
			t.Fatal(err)
		}
		if err := st.SetMessageAttachmentsKnown(ctx, orgID, messageID, nil); err != nil {
			t.Fatal(err)
		}
		var state string
		var rows int
		if err := db.QueryRowContext(ctx, `SELECT attachments_state FROM messages WHERE id = $1`, messageID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM message_attachments WHERE message_id = $1`, messageID).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if state != "known" || rows != 0 {
			t.Fatalf("state=%q rows=%d, want known empty envelope", state, rows)
		}
	})
}

func TestSetMessageAttachmentsKnownDoesNotDisturbMirroredReference(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, inboxID, threadID := seedAttachmentMessageParents(t, ctx, db, "metadata-mirrored")
		messageID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `
			INSERT INTO messages (id, org_id, inbox_id, thread_id, direction, received_email_id)
			VALUES ($1, $2, $3, $4, 'inbound', 'received-mirrored')
		`, messageID, orgID, inboxID, threadID); err != nil {
			t.Fatal(err)
		}
		metadata := []MessageAttachmentMetadata{{ProviderAttachmentID: "provider-1", Filename: "before.txt"}}
		if err := st.SetMessageAttachmentsKnown(ctx, orgID, messageID, metadata); err != nil {
			t.Fatal(err)
		}
		digest, _, err := st.StoreAttachmentBlob(ctx, orgID, "text/plain", []byte("mirrored"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `
			UPDATE message_attachments
			SET availability = 'available', blob_sha256 = $3, mirrored_at = now()
			WHERE org_id = $1 AND message_id = $2
		`, orgID, messageID, digest); err != nil {
			t.Fatal(err)
		}
		assertBlobRefCount(t, ctx, db, orgID, digest, 1)

		metadata[0].Filename = "after.txt"
		if err := st.SetMessageAttachmentsKnown(ctx, orgID, messageID, metadata); err != nil {
			t.Fatal(err)
		}
		attachments, err := st.ListMessageAttachments(ctx, orgID, messageID)
		if err != nil || len(attachments) != 1 {
			t.Fatalf("attachments=%v err=%v", attachments, err)
		}
		if attachments[0].Filename != "after.txt" || attachments[0].Availability != "available" ||
			!attachments[0].BlobSHA256.Valid || attachments[0].BlobSHA256.String != digest {
			t.Fatalf("attachment after replay=%+v", attachments[0])
		}
		assertBlobRefCount(t, ctx, db, orgID, digest, 1)
	})
}

func TestSetMessageAttachmentsKnownRollsBackWithRunAsOrg(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, inboxID, threadID := seedAttachmentMessageParents(t, ctx, db, "metadata-rollback")
		messageID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `
			INSERT INTO messages (id, org_id, inbox_id, thread_id, direction, received_email_id)
			VALUES ($1, $2, $3, $4, 'inbound', 'received-rollback')
		`, messageID, orgID, inboxID, threadID); err != nil {
			t.Fatal(err)
		}
		sentinel := errors.New("force metadata rollback")
		err := st.RunAsOrg(ctx, orgID, func(scoped *Store) error {
			if err := scoped.SetMessageAttachmentsKnown(ctx, orgID, messageID, []MessageAttachmentMetadata{{ProviderAttachmentID: "provider-rollback"}}); err != nil {
				return err
			}
			return sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("RunAsOrg err=%v, want sentinel", err)
		}
		var rows int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM message_attachments WHERE message_id = $1`, messageID).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 0 {
			t.Fatalf("rolled-back metadata rows=%d", rows)
		}
		var state string
		if err := db.QueryRowContext(ctx, `SELECT attachments_state FROM messages WHERE id = $1`, messageID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "pending_backfill" {
			t.Fatalf("rolled-back state=%q, want pending_backfill", state)
		}
	})
}

func TestSetMessageAttachmentsKnownRejectsInvalidMetadataBeforeWriting(t *testing.T) {
	st := &Store{}
	negative := int64(-1)
	for _, attachments := range [][]MessageAttachmentMetadata{
		{{ProviderAttachmentID: ""}},
		{{ProviderAttachmentID: "duplicate"}, {ProviderAttachmentID: "duplicate"}},
		{{ProviderAttachmentID: "negative", SizeBytes: &negative}},
	} {
		if err := st.SetMessageAttachmentsKnown(context.Background(), uuid.NewString(), uuid.NewString(), attachments); err == nil {
			t.Fatalf("invalid metadata unexpectedly accepted: %+v", attachments)
		}
	}
}
