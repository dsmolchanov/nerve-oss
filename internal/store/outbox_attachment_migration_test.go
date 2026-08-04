package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMigration25CreatesOutboxAttachmentLifecycle(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if err := MigrateUpToCore(ctx, db, 24); err != nil {
			t.Fatal(err)
		}
		orgID, inboxID, _ := seedAttachmentMessageParents(t, ctx, db, "outbox-attachment-schema")
		outboxIDs := map[string]string{
			"queued": uuid.NewString(),
			"sent":   uuid.NewString(),
			"failed": uuid.NewString(),
		}
		for status, id := range outboxIDs {
			if _, err := db.ExecContext(ctx, `
				INSERT INTO outbox_messages
				  (id, org_id, inbox_id, provider, idempotency_key, "to", "from", subject, status)
				VALUES ($1, $2, $3, 'smtp', $4, 'to@example.com', 'from@example.com', 'subject', $5)
			`, id, orgID, inboxID, "migration-25-"+status, status); err != nil {
				t.Fatal(err)
			}
		}

		migrationStartedAt := time.Now().UTC().Add(-time.Second)
		if err := MigrateUpToCore(ctx, db, 25); err != nil {
			t.Fatal(err)
		}
		assertTableExists(t, db, "outbox_attachments")
		for status, id := range outboxIDs {
			var terminalAt sql.NullTime
			if err := db.QueryRowContext(ctx, `SELECT terminal_at FROM outbox_messages WHERE id = $1`, id).Scan(&terminalAt); err != nil {
				t.Fatal(err)
			}
			wantTerminal := status == "sent" || status == "failed"
			if terminalAt.Valid != wantTerminal {
				t.Fatalf("status=%s terminal_at=%v, want valid=%v", status, terminalAt, wantTerminal)
			}
			if terminalAt.Valid && terminalAt.Time.Before(migrationStartedAt) {
				t.Fatalf("status=%s terminal_at=%s predates migration", status, terminalAt.Time)
			}
		}

		digest := "outbox-blob"
		if _, err := db.ExecContext(ctx, `
			INSERT INTO attachment_blobs (org_id, sha256, size_bytes, content_type, content)
			VALUES ($1, $2, 1, 'text/plain', '\x01')
		`, orgID, digest); err != nil {
			t.Fatal(err)
		}
		attachmentID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `
			INSERT INTO outbox_attachments
			  (id, org_id, outbox_message_id, ordinal, filename, content_type, size_bytes, sha256, blob_sha256)
			VALUES ($1, $2, $3, 0, 'report.txt', 'text/plain', 1, $4, $4)
		`, attachmentID, orgID, outboxIDs["sent"], digest); err != nil {
			t.Fatal(err)
		}
		assertBlobRefCount(t, ctx, db, orgID, digest, 1)
		if _, err := db.ExecContext(ctx, `
			UPDATE outbox_attachments SET blob_sha256 = NULL WHERE id = $1
		`, attachmentID); err != nil {
			t.Fatal(err)
		}
		assertBlobRefCount(t, ctx, db, orgID, digest, 0)
		var retainedDigest string
		if err := db.QueryRowContext(ctx, `SELECT sha256 FROM outbox_attachments WHERE id = $1`, attachmentID).Scan(&retainedDigest); err != nil {
			t.Fatal(err)
		}
		if retainedDigest != digest {
			t.Fatalf("retained sha256=%q, want %q", retainedDigest, digest)
		}

		if err := MigrateDownCore(ctx, db); err == nil || !strings.Contains(err.Error(), "outbox attachment metadata exists") {
			t.Fatalf("populated migration down err=%v", err)
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM outbox_attachments`); err != nil {
			t.Fatal(err)
		}
		if err := MigrateDownCore(ctx, db); err != nil {
			t.Fatal(err)
		}
		version, err := CurrentVersionCore(ctx, db)
		if err != nil || version != 24 {
			t.Fatalf("version=%d err=%v after migration down", version, err)
		}
		var tableName sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT to_regclass('public.outbox_attachments')::text`).Scan(&tableName); err != nil {
			t.Fatal(err)
		}
		if tableName.Valid {
			t.Fatalf("outbox_attachments still exists after migration down")
		}
	})
}
