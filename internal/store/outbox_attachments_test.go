package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestOutboxAttachmentReverseFKOrderingFails(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		_, orgID, _ := seedOutboundAttachmentStore(t, ctx, db, "reverse-fk-order")
		digest := "reverse-order-blob"
		if _, err := db.ExecContext(ctx, `
			INSERT INTO attachment_blobs (org_id, sha256, size_bytes, content_type, content)
			VALUES ($1, $2, 1, 'text/plain', '\x01')
		`, orgID, digest); err != nil {
			t.Fatal(err)
		}

		// EnqueueOutboxMessage must insert the parent first. Force the reverse
		// order directly to keep the immediate composite FK as a regression gate.
		_, err := db.ExecContext(ctx, `
			INSERT INTO outbox_attachments
			  (org_id, outbox_message_id, ordinal, filename, content_type, size_bytes, sha256, blob_sha256)
			VALUES ($1, $2, 0, 'before-parent.txt', 'text/plain', 1, $3, $3)
		`, orgID, uuid.NewString(), digest)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23503" ||
			!strings.Contains(pgErr.ConstraintName, "outbox_message_id") {
			t.Fatalf("reverse child-before-parent insert err=%v, want outbox parent FK violation", err)
		}
	})
}

func TestEnqueueOutboxMessageAttachmentFingerprintAndReplay(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		st, orgID, inboxID := seedOutboundAttachmentStore(t, ctx, db, "fingerprint")
		first := outboundAttachmentMessage(orgID, inboxID, "fingerprint-a", []OutboundAttachment{
			{Filename: "a.txt", ContentType: "text/plain", Content: []byte("alpha")},
			{Filename: "b.txt", ContentType: "text/plain", Content: []byte("beta")},
		})
		firstID, err := st.EnqueueOutboxMessage(ctx, first)
		if err != nil {
			t.Fatal(err)
		}

		reordered := first
		reordered.IdempotencyKey = "fingerprint-b"
		reordered.Attachments = []OutboundAttachment{first.Attachments[1], first.Attachments[0]}
		reorderedID, err := st.EnqueueOutboxMessage(ctx, reordered)
		if err != nil {
			t.Fatal(err)
		}
		if reorderedID == firstID {
			t.Fatal("attachment ordinal did not affect the outbox fingerprint")
		}

		replay := first
		replay.IdempotencyKey = "fingerprint-replay"
		replayID, err := st.EnqueueOutboxMessage(ctx, replay)
		if err != nil {
			t.Fatal(err)
		}
		if replayID != firstID {
			t.Fatalf("matching fingerprint replay id=%s, want %s", replayID, firstID)
		}

		var refs int
		if err := db.QueryRowContext(ctx, `
			SELECT count(*) FROM outbox_attachments WHERE outbox_message_id = $1
		`, firstID).Scan(&refs); err != nil {
			t.Fatal(err)
		}
		if refs != 2 {
			t.Fatalf("matching replay created refs: count=%d, want 2", refs)
		}
		for _, content := range []string{"alpha", "beta"} {
			var refCount int
			if err := db.QueryRowContext(ctx, `
				SELECT ref_count FROM attachment_blobs
				WHERE org_id = $1 AND content = convert_to($2, 'UTF8')
			`, orgID, content).Scan(&refCount); err != nil {
				t.Fatal(err)
			}
			if refCount != 2 {
				t.Fatalf("blob %q ref_count=%d, want 2 (one per distinct parent)", content, refCount)
			}
		}
	})
}

func TestEnqueueOutboxMessageIdempotencyConflictIncludesAttachments(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		st, orgID, inboxID := seedOutboundAttachmentStore(t, ctx, db, "idempotency-conflict")
		first := outboundAttachmentMessage(orgID, inboxID, "same-key", []OutboundAttachment{
			{Filename: "report.txt", ContentType: "text/plain", Content: []byte("first")},
		})
		if _, err := st.EnqueueOutboxMessage(ctx, first); err != nil {
			t.Fatal(err)
		}
		second := first
		second.Attachments = []OutboundAttachment{
			{Filename: "report.txt", ContentType: "text/plain", Content: []byte("different")},
		}
		if _, err := st.EnqueueOutboxMessage(ctx, second); !errors.Is(err, ErrOutboxIdempotencyConflict) {
			t.Fatalf("err=%v, want ErrOutboxIdempotencyConflict", err)
		}

		var messages, blobs, refs int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM outbox_messages WHERE org_id = $1`, orgID).Scan(&messages); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM attachment_blobs WHERE org_id = $1`, orgID).Scan(&blobs); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM outbox_attachments WHERE org_id = $1`, orgID).Scan(&refs); err != nil {
			t.Fatal(err)
		}
		if messages != 1 || blobs != 1 || refs != 1 {
			t.Fatalf("messages=%d blobs=%d refs=%d, want 1/1/1", messages, blobs, refs)
		}
	})
}

func TestEnqueueOutboxMessageReplaysLegacyNullFingerprint(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		st, orgID, inboxID := seedOutboundAttachmentStore(t, ctx, db, "legacy-null-fingerprint")
		legacyID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `
			INSERT INTO outbox_messages (
			  id, org_id, inbox_id, provider, idempotency_key,
			  "to", "from", subject, text_body, content_hash
			) VALUES (
			  $1, $2, $3, 'smtp', 'legacy-key',
			  'to@example.com', 'from@example.com', 'legacy subject', 'legacy body', NULL
			)
		`, legacyID, orgID, inboxID); err != nil {
			t.Fatal(err)
		}

		replayedID, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID:          orgID,
			InboxID:        inboxID,
			Provider:       "smtp",
			IdempotencyKey: "legacy-key",
			To:             "to@example.com",
			From:           "from@example.com",
			Subject:        "legacy subject",
			TextBody:       "legacy body",
		})
		if err != nil {
			t.Fatalf("legacy retry returned error: %v", err)
		}
		if replayedID != legacyID {
			t.Fatalf("legacy retry id=%q, want %q", replayedID, legacyID)
		}
		var rows int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM outbox_messages WHERE org_id = $1 AND idempotency_key = 'legacy-key'`, orgID).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 1 {
			t.Fatalf("legacy retry left %d rows, want 1", rows)
		}
	})
}

func TestEnqueueOutboxMessageRejectsEmptyAttachmentBeforeSQL(t *testing.T) {
	st := &Store{}
	_, err := st.EnqueueOutboxMessage(context.Background(), outboundAttachmentMessage(
		uuid.NewString(), uuid.NewString(), "empty", []OutboundAttachment{
			{Filename: "empty.txt", ContentType: "text/plain"},
		},
	))
	if !errors.Is(err, ErrAttachmentEmpty) {
		t.Fatalf("err=%v, want ErrAttachmentEmpty", err)
	}
}

func TestEnqueueOutboxMessageQuotaFailureIsAtomic(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		st, orgID, inboxID := seedOutboundAttachmentStore(t, ctx, db, "quota-atomic")
		if _, err := db.ExecContext(ctx, `
			INSERT INTO org_attachment_usage (org_id, bytes_quota) VALUES ($1, 0)
		`, orgID); err != nil {
			t.Fatal(err)
		}
		msg := outboundAttachmentMessage(orgID, inboxID, "quota", []OutboundAttachment{
			{Filename: "one.txt", ContentType: "text/plain", Content: []byte("x")},
		})
		if _, err := st.EnqueueOutboxMessage(ctx, msg); !errors.Is(err, ErrAttachmentQuotaExceeded) {
			t.Fatalf("err=%v, want ErrAttachmentQuotaExceeded", err)
		}

		for table, want := range map[string]int{
			"outbox_messages":    0,
			"outbox_attachments": 0,
			"attachment_blobs":   0,
		} {
			var count int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE org_id = $1`, orgID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != want {
				t.Fatalf("%s count=%d, want %d", table, count, want)
			}
		}
		var used int64
		if err := db.QueryRowContext(ctx, `SELECT bytes_used FROM org_attachment_usage WHERE org_id = $1`, orgID).Scan(&used); err != nil {
			t.Fatal(err)
		}
		if used != 0 {
			t.Fatalf("bytes_used=%d after rollback, want 0", used)
		}
	})
}

func TestEnqueueSuppressedMessageStoresAttachmentsAndTerminalTime(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		st, orgID, inboxID := seedOutboundAttachmentStore(t, ctx, db, "suppressed-terminal")
		if err := st.AddSuppression(ctx, orgID, "blocked@example.com", "hard_bounce", "bounce"); err != nil {
			t.Fatal(err)
		}
		msg := outboundAttachmentMessage(orgID, inboxID, "suppressed", []OutboundAttachment{
			{Filename: "notice.txt", ContentType: "text/plain", Content: []byte("notice")},
		})
		msg.To = "blocked@example.com"
		outboxID, err := st.EnqueueOutboxMessage(ctx, msg)
		if err != nil {
			t.Fatal(err)
		}

		var status string
		var terminalAt sql.NullTime
		var refs, events int
		if err := db.QueryRowContext(ctx, `
			SELECT status, terminal_at,
			       (SELECT count(*) FROM outbox_attachments WHERE outbox_message_id = outbox_messages.id),
			       (SELECT count(*) FROM outbox_events WHERE outbox_message_id = outbox_messages.id)
			FROM outbox_messages WHERE id = $1
		`, outboxID).Scan(&status, &terminalAt, &refs, &events); err != nil {
			t.Fatal(err)
		}
		if status != "failed" || !terminalAt.Valid || refs != 1 || events != 1 {
			t.Fatalf("status=%s terminal=%v refs=%d events=%d", status, terminalAt.Valid, refs, events)
		}
	})
}

func TestOutboxAttachmentDeliveryLoadAndTerminalLifecycle(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		st, orgID, inboxID := seedOutboundAttachmentStore(t, ctx, db, "delivery-lifecycle")
		msg := outboundAttachmentMessage(orgID, inboxID, "delivery", []OutboundAttachment{
			{Filename: "first.txt", ContentType: "text/plain", Content: []byte("first")},
			{Filename: "second.pdf", ContentType: "application/pdf", Content: []byte("second")},
		})
		outboxID, err := st.EnqueueOutboxMessage(ctx, msg)
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := st.LoadOutboxMessageAttachments(ctx, orgID, outboxID)
		if err != nil {
			t.Fatal(err)
		}
		if len(loaded) != 2 || string(loaded[0].Content) != "first" || string(loaded[1].Content) != "second" {
			t.Fatalf("loaded attachments=%+v", loaded)
		}

		if err := st.MarkOutboxMessageFailed(ctx, outboxID, "provider failure"); err != nil {
			t.Fatal(err)
		}
		assertOutboxTerminalState(t, ctx, db, outboxID, "failed", true)
		replayed, err := st.ReplayOutboxMessage(ctx, orgID, outboxID)
		if err != nil || !replayed {
			t.Fatalf("replayed=%v err=%v", replayed, err)
		}
		assertOutboxTerminalState(t, ctx, db, outboxID, "queued", false)

		if err := st.MarkOutboxMessageSent(ctx, outboxID, "provider-id"); err != nil {
			t.Fatal(err)
		}
		assertOutboxTerminalState(t, ctx, db, outboxID, "sent", true)
	})
}

func TestReplayOutboxMessageRejectsReleasedAttachments(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		st, orgID, inboxID := seedOutboundAttachmentStore(t, ctx, db, "released-replay")
		msg := outboundAttachmentMessage(orgID, inboxID, "released", []OutboundAttachment{
			{Filename: "audit.txt", ContentType: "text/plain", Content: []byte("retained metadata")},
		})
		outboxID, err := st.EnqueueOutboxMessage(ctx, msg)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.MarkOutboxMessageFailed(ctx, outboxID, "failed"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `
			UPDATE outbox_attachments SET blob_sha256 = NULL WHERE outbox_message_id = $1
		`, outboxID); err != nil {
			t.Fatal(err)
		}

		if _, err := st.LoadOutboxMessageAttachments(ctx, orgID, outboxID); !errors.Is(err, ErrAttachmentsReleased) {
			t.Fatalf("load err=%v, want ErrAttachmentsReleased", err)
		}
		replayed, err := st.ReplayOutboxMessage(ctx, orgID, outboxID)
		if replayed || !errors.Is(err, ErrAttachmentsReleased) {
			t.Fatalf("replayed=%v err=%v, want released error", replayed, err)
		}
		assertOutboxTerminalState(t, ctx, db, outboxID, "failed", true)

		var digest, blobDigest sql.NullString
		if err := db.QueryRowContext(ctx, `
			SELECT sha256, blob_sha256 FROM outbox_attachments WHERE outbox_message_id = $1
		`, outboxID).Scan(&digest, &blobDigest); err != nil {
			t.Fatal(err)
		}
		if !digest.Valid || digest.String == "" || blobDigest.Valid {
			t.Fatalf("sha256=%v blob_sha256=%v after release", digest, blobDigest)
		}
	})
}

func assertOutboxTerminalState(t *testing.T, ctx context.Context, db *sql.DB, id, wantStatus string, wantTerminal bool) {
	t.Helper()
	var status string
	var terminalAt sql.NullTime
	if err := db.QueryRowContext(ctx, `
		SELECT status, terminal_at FROM outbox_messages WHERE id = $1
	`, id).Scan(&status, &terminalAt); err != nil {
		t.Fatal(err)
	}
	if status != wantStatus || terminalAt.Valid != wantTerminal {
		t.Fatalf("status=%s terminal=%v, want %s/%v", status, terminalAt.Valid, wantStatus, wantTerminal)
	}
}

func seedOutboundAttachmentStore(t *testing.T, ctx context.Context, db *sql.DB, name string) (*Store, string, string) {
	t.Helper()
	migrateToLatest(t, ctx, db)
	st := &Store{db: db, q: db}
	orgID := uuid.NewString()
	inboxID := uuid.NewString()
	if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, $2)`, orgID, name); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO inboxes (id, org_id, address, status)
		VALUES ($1, $2, $3, 'active')
	`, inboxID, orgID, name+"@local.neuralmail"); err != nil {
		t.Fatal(err)
	}
	return st, orgID, inboxID
}

func outboundAttachmentMessage(orgID, inboxID, idempotencyKey string, attachments []OutboundAttachment) OutboxMessage {
	return OutboxMessage{
		OrgID:          orgID,
		InboxID:        inboxID,
		Provider:       "smtp",
		IdempotencyKey: idempotencyKey,
		To:             "to@example.com",
		From:           "from@example.com",
		Subject:        "attachment test",
		TextBody:       "same body",
		Attachments:    attachments,
	}
}
