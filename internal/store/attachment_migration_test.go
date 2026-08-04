package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMigration22SeedsUsageAndHasCleanEmptyDown(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if err := MigrateUpToCore(ctx, db, 21); err != nil {
			t.Fatal(err)
		}
		orgID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'blob-schema')`, orgID); err != nil {
			t.Fatal(err)
		}

		if err := MigrateUpToCore(ctx, db, 22); err != nil {
			t.Fatal(err)
		}
		assertTableExists(t, db, "attachment_blobs")
		assertTableExists(t, db, "org_attachment_usage")
		var primaryKey string
		if err := db.QueryRowContext(ctx, `
			SELECT string_agg(attribute.attname, ',' ORDER BY key.ordinality)
			FROM pg_index index
			JOIN pg_class table_name ON table_name.oid = index.indrelid
			JOIN pg_namespace namespace ON namespace.oid = table_name.relnamespace
			CROSS JOIN LATERAL unnest(index.indkey) WITH ORDINALITY AS key(attnum, ordinality)
			JOIN pg_attribute attribute
			  ON attribute.attrelid = table_name.oid AND attribute.attnum = key.attnum
			WHERE namespace.nspname = 'public'
			  AND table_name.relname = 'attachment_blobs'
			  AND index.indisprimary
		`).Scan(&primaryKey); err != nil {
			t.Fatal(err)
		}
		if primaryKey != "org_id,sha256" {
			t.Fatalf("attachment_blobs primary key=%q, want org_id,sha256", primaryKey)
		}
		var used, quota int64
		if err := db.QueryRowContext(ctx, `
			SELECT bytes_used, bytes_quota FROM org_attachment_usage WHERE org_id = $1
		`, orgID).Scan(&used, &quota); err != nil {
			t.Fatal(err)
		}
		if used != 0 || quota != 2<<30 {
			t.Fatalf("usage=(%d,%d), want (0,%d)", used, quota, int64(2<<30))
		}

		if err := MigrateDownCore(ctx, db); err != nil {
			t.Fatalf("clean migration 22 down: %v", err)
		}
		version, err := CurrentVersionCore(ctx, db)
		if err != nil || version != 21 {
			t.Fatalf("version=%d err=%v after migration 22 down", version, err)
		}
	})
}

func TestCreateOrgSeedsUsageOnlyWhenSchemaSupportsIt(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if err := MigrateUpToCore(ctx, db, 21); err != nil {
			t.Fatal(err)
		}
		st := &Store{db: db, q: db}
		if seeded, err := st.SeedMissingOrgAttachmentUsage(ctx); err != nil || seeded != 0 {
			t.Fatalf("pre-0022 seed=%d err=%v, want schema-compatible no-op", seeded, err)
		}
		beforeID, err := st.CreateOrg(ctx, "before-attachment-schema")
		if err != nil {
			t.Fatalf("create org at schema 21: %v", err)
		}
		if err := MigrateUpToCore(ctx, db, 22); err != nil {
			t.Fatal(err)
		}
		var beforeUsage int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM org_attachment_usage WHERE org_id = $1`, beforeID).Scan(&beforeUsage); err != nil {
			t.Fatal(err)
		}
		if beforeUsage != 1 {
			t.Fatalf("migration seed rows=%d, want 1", beforeUsage)
		}

		afterID, err := st.CreateOrg(ctx, "after-attachment-schema")
		if err != nil {
			t.Fatalf("create org at schema 22: %v", err)
		}
		var afterUsage int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM org_attachment_usage WHERE org_id = $1`, afterID).Scan(&afterUsage); err != nil {
			t.Fatal(err)
		}
		if afterUsage != 1 {
			t.Fatalf("create-org usage rows=%d, want 1", afterUsage)
		}
	})
}

func TestMigration23ClassifiesExistingMessageAttachmentState(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if err := MigrateUpToCore(ctx, db, 22); err != nil {
			t.Fatal(err)
		}
		orgID, inboxID, threadID := seedAttachmentMessageParents(t, ctx, db, "classification")
		now := time.Now().UTC()
		messages := []struct {
			id              string
			direction       string
			receivedEmailID any
			createdAt       time.Time
			want            string
		}{
			{id: uuid.NewString(), direction: "outbound", receivedEmailID: "outbound-provider", createdAt: now, want: "known"},
			{id: uuid.NewString(), direction: "inbound", receivedEmailID: nil, createdAt: now, want: "known"},
			{id: uuid.NewString(), direction: "inbound", receivedEmailID: "recent-provider", createdAt: now, want: "pending_backfill"},
			{id: uuid.NewString(), direction: "inbound", receivedEmailID: "expired-provider", createdAt: now.Add(-31 * 24 * time.Hour), want: "unknown_metadata_expired"},
		}
		for _, message := range messages {
			if _, err := db.ExecContext(ctx, `
				INSERT INTO messages
				  (id, org_id, inbox_id, thread_id, direction, received_email_id, created_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
			`, message.id, orgID, inboxID, threadID, message.direction, message.receivedEmailID, message.createdAt); err != nil {
				t.Fatal(err)
			}
		}

		if err := MigrateUpToCore(ctx, db, 23); err != nil {
			t.Fatal(err)
		}
		for _, message := range messages {
			var got string
			if err := db.QueryRowContext(ctx, `SELECT attachments_state FROM messages WHERE id = $1`, message.id).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != message.want {
				t.Fatalf("message %s state=%q, want %q", message.id, got, message.want)
			}
		}
		assertTableExists(t, db, "message_attachments")

		if err := MigrateDownCore(ctx, db); err != nil {
			t.Fatalf("clean migration 23 down: %v", err)
		}
		assertColumnMissing(t, db, "messages", "attachments_state")
	})
}

func TestMessageAttachmentReferencesDriveBlobRefCount(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		orgID, inboxID, threadID := seedAttachmentMessageParents(t, ctx, db, "ref-count")
		messageID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `
			INSERT INTO messages (id, org_id, inbox_id, thread_id, direction, attachments_state)
			VALUES ($1, $2, $3, $4, 'inbound', 'known')
		`, messageID, orgID, inboxID, threadID); err != nil {
			t.Fatal(err)
		}
		for _, sha := range []string{"blob-a", "blob-b"} {
			if _, err := db.ExecContext(ctx, `
				INSERT INTO attachment_blobs (org_id, sha256, size_bytes, content_type, content)
				VALUES ($1, $2, 1, 'application/octet-stream', '\x01')
			`, orgID, sha); err != nil {
				t.Fatal(err)
			}
		}

		if _, err := db.ExecContext(ctx, `
			INSERT INTO message_attachments
			  (org_id, message_id, ordinal, provider_attachment_id, availability, blob_sha256)
			VALUES ($1, $2, 0, 'provider-a', 'available', 'blob-a')
		`, orgID, messageID); err != nil {
			t.Fatal(err)
		}
		assertBlobRefCount(t, ctx, db, orgID, "blob-a", 1)

		if _, err := db.ExecContext(ctx, `
			INSERT INTO message_attachments
			  (org_id, message_id, ordinal, provider_attachment_id, availability, blob_sha256)
			VALUES ($1, $2, 0, 'provider-a', 'available', 'blob-a')
			ON CONFLICT (message_id, provider_attachment_id) DO NOTHING
		`, orgID, messageID); err != nil {
			t.Fatal(err)
		}
		assertBlobRefCount(t, ctx, db, orgID, "blob-a", 1)

		if _, err := db.ExecContext(ctx, `
			UPDATE message_attachments SET blob_sha256 = 'blob-b'
			WHERE message_id = $1 AND provider_attachment_id = 'provider-a'
		`, messageID); err != nil {
			t.Fatal(err)
		}
		assertBlobRefCount(t, ctx, db, orgID, "blob-a", 0)
		assertBlobRefCount(t, ctx, db, orgID, "blob-b", 1)

		if _, err := db.ExecContext(ctx, `
			UPDATE message_attachments SET blob_sha256 = NULL
			WHERE message_id = $1 AND provider_attachment_id = 'provider-a'
		`, messageID); err != nil {
			t.Fatal(err)
		}
		assertBlobRefCount(t, ctx, db, orgID, "blob-b", 0)

		if _, err := db.ExecContext(ctx, `
			UPDATE message_attachments SET blob_sha256 = 'blob-a'
			WHERE message_id = $1 AND provider_attachment_id = 'provider-a'
		`, messageID); err != nil {
			t.Fatal(err)
		}
		assertBlobRefCount(t, ctx, db, orgID, "blob-a", 1)
		if _, err := db.ExecContext(ctx, `DELETE FROM message_attachments WHERE message_id = $1`, messageID); err != nil {
			t.Fatal(err)
		}
		assertBlobRefCount(t, ctx, db, orgID, "blob-a", 0)

		if _, err := db.ExecContext(ctx, `
			INSERT INTO message_attachments
			  (org_id, message_id, ordinal, provider_attachment_id, availability, blob_sha256)
			VALUES ($1, $2, 0, 'provider-cascade', 'available', 'blob-a')
		`, orgID, messageID); err != nil {
			t.Fatal(err)
		}
		assertBlobRefCount(t, ctx, db, orgID, "blob-a", 1)
		if _, err := db.ExecContext(ctx, `DELETE FROM messages WHERE id = $1`, messageID); err != nil {
			t.Fatal(err)
		}
		assertBlobRefCount(t, ctx, db, orgID, "blob-a", 0)
	})
}

func TestMessageAttachmentCompositeForeignKeysRejectCrossTenantReferences(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		orgA, inboxA, threadA := seedAttachmentMessageParents(t, ctx, db, "fk-a")
		orgB, inboxB, threadB := seedAttachmentMessageParents(t, ctx, db, "fk-b")
		messageA := uuid.NewString()
		messageB := uuid.NewString()
		for _, message := range []struct {
			id, orgID, inboxID, threadID string
		}{
			{id: messageA, orgID: orgA, inboxID: inboxA, threadID: threadA},
			{id: messageB, orgID: orgB, inboxID: inboxB, threadID: threadB},
		} {
			if _, err := db.ExecContext(ctx, `
				INSERT INTO messages (id, org_id, inbox_id, thread_id, direction, attachments_state)
				VALUES ($1, $2, $3, $4, 'inbound', 'known')
			`, message.id, message.orgID, message.inboxID, message.threadID); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO attachment_blobs (org_id, sha256, size_bytes, content_type, content)
			VALUES ($1, 'org-b-only', 1, 'application/octet-stream', '\x01')
		`, orgB); err != nil {
			t.Fatal(err)
		}

		if _, err := db.ExecContext(ctx, `
			INSERT INTO message_attachments (org_id, message_id, ordinal, provider_attachment_id)
			VALUES ($1, $2, 0, 'wrong-message-owner')
		`, orgA, messageB); err == nil {
			t.Fatal("mismatched (org_id, message_id) unexpectedly passed its composite foreign key")
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO message_attachments
			  (org_id, message_id, ordinal, provider_attachment_id, availability, blob_sha256)
			VALUES ($1, $2, 0, 'wrong-blob-owner', 'available', 'org-b-only')
		`, orgA, messageA); err == nil {
			t.Fatal("cross-org blob digest unexpectedly passed its composite foreign key")
		}
	})
}

func TestMigration23DownRefusesAttachmentMetadata(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if err := MigrateUpToCore(ctx, db, 23); err != nil {
			t.Fatal(err)
		}
		orgID, inboxID, threadID := seedAttachmentMessageParents(t, ctx, db, "down-refusal")
		messageID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `
			INSERT INTO messages (id, org_id, inbox_id, thread_id, direction, attachments_state)
			VALUES ($1, $2, $3, $4, 'inbound', 'known')
		`, messageID, orgID, inboxID, threadID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO message_attachments (org_id, message_id, ordinal, provider_attachment_id)
			VALUES ($1, $2, 0, 'provider-down')
		`, orgID, messageID); err != nil {
			t.Fatal(err)
		}

		err := MigrateDownCore(ctx, db)
		if err == nil || !strings.Contains(err.Error(), "cannot roll back core migration 0023") {
			t.Fatalf("down error=%v, want metadata refusal", err)
		}
		version, versionErr := CurrentVersionCore(ctx, db)
		if versionErr != nil || version != 23 {
			t.Fatalf("version=%d err=%v after refused down", version, versionErr)
		}
	})
}

func TestAttachmentTablesEnforceTenantRLSAndLegacyDenyAll(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		type tenant struct {
			orgID     string
			messageID string
		}
		tenants := make([]tenant, 0, 2)
		for _, suffix := range []string{"rls-a", "rls-b"} {
			orgID, inboxID, threadID := seedAttachmentMessageParents(t, ctx, db, suffix)
			messageID := uuid.NewString()
			if _, err := db.ExecContext(ctx, `
				INSERT INTO messages (id, org_id, inbox_id, thread_id, direction, attachments_state)
				VALUES ($1, $2, $3, $4, 'inbound', 'known')
			`, messageID, orgID, inboxID, threadID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `
				INSERT INTO attachment_blobs (org_id, sha256, size_bytes, content_type, content)
				VALUES ($1, $2, 1, 'application/octet-stream', '\x01')
			`, orgID, "blob-"+suffix); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `
				INSERT INTO message_attachments
				  (org_id, message_id, ordinal, provider_attachment_id, availability, blob_sha256)
				VALUES ($1, $2, 0, $3, 'available', $4)
			`, orgID, messageID, "provider-"+suffix, "blob-"+suffix); err != nil {
				t.Fatal(err)
			}
			tenants = append(tenants, tenant{orgID: orgID, messageID: messageID})
		}
		for _, tenant := range tenants {
			if _, err := db.ExecContext(ctx, `
				INSERT INTO org_attachment_usage (org_id) VALUES ($1)
			`, tenant.orgID); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO attachments (message_id, object_ref, mime, size)
			VALUES ($1, 'legacy-object', 'application/octet-stream', 1)
		`, tenants[0].messageID); err != nil {
			t.Fatal(err)
		}

		roleName := "rls_attachment_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD 'rls_attachment'`, roleName)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`GRANT USAGE ON SCHEMA public TO %s`, roleName)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`
			GRANT SELECT, INSERT, UPDATE, DELETE
			ON attachment_blobs, org_attachment_usage, message_attachments, attachments
			TO %s
		`, roleName)); err != nil {
			t.Fatal(err)
		}

		var databaseName string
		if err := db.QueryRowContext(ctx, `SELECT current_database()`).Scan(&databaseName); err != nil {
			t.Fatal(err)
		}
		baseDSN := os.Getenv("NM_TEST_DB_DSN")
		if baseDSN == "" {
			baseDSN = "postgres://neuralmail@127.0.0.1:54320/neuralmail?sslmode=disable"
		}
		appDSN, err := dsnWithDatabase(baseDSN, databaseName)
		if err != nil {
			t.Fatal(err)
		}
		appDSN, err = dsnWithCredentials(appDSN, roleName, "rls_attachment")
		if err != nil {
			t.Fatal(err)
		}
		appDB, err := sql.Open("pgx", appDSN)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = appDB.Close()
			_, _ = db.ExecContext(context.Background(), fmt.Sprintf(`DROP OWNED BY %s`, roleName))
			_, _ = db.ExecContext(context.Background(), fmt.Sprintf(`DROP ROLE IF EXISTS %s`, roleName))
		})

		st := &Store{db: appDB, q: appDB}
		for _, table := range []string{"attachment_blobs", "org_attachment_usage", "message_attachments"} {
			var visible int
			if err := st.RunAsOrg(ctx, tenants[0].orgID, func(scoped *Store) error {
				return scoped.q.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&visible)
			}); err != nil {
				t.Fatalf("read %s: %v", table, err)
			}
			if visible != 1 {
				t.Fatalf("visible %s rows=%d, want 1", table, visible)
			}
		}

		var legacyVisible int
		if err := st.RunAsOrg(ctx, tenants[0].orgID, func(scoped *Store) error {
			return scoped.q.QueryRowContext(ctx, `SELECT count(*) FROM attachments`).Scan(&legacyVisible)
		}); err != nil {
			t.Fatal(err)
		}
		if legacyVisible != 0 {
			t.Fatalf("legacy attachment rows visible=%d, want deny-all", legacyVisible)
		}

		err = st.RunAsOrg(ctx, tenants[0].orgID, func(scoped *Store) error {
			_, innerErr := scoped.q.ExecContext(ctx, `
				INSERT INTO attachment_blobs (org_id, sha256, size_bytes, content_type, content)
				VALUES ($1, 'cross-org', 1, 'application/octet-stream', '\x01')
			`, tenants[1].orgID)
			return innerErr
		})
		if err == nil {
			t.Fatal("cross-org blob insert unexpectedly passed RLS")
		}
	})
}

func seedAttachmentMessageParents(t *testing.T, ctx context.Context, db *sql.DB, suffix string) (string, string, string) {
	t.Helper()
	orgID := uuid.NewString()
	inboxID := uuid.NewString()
	threadID := uuid.NewString()
	if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, $2)`, orgID, "attachments-"+suffix); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO inboxes (id, org_id, address, status) VALUES ($1, $2, $3, 'active')
	`, inboxID, orgID, suffix+"@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO threads (id, org_id, inbox_id, status, participants)
		VALUES ($1, $2, $3, 'open', '[]')
	`, threadID, orgID, inboxID); err != nil {
		t.Fatal(err)
	}
	return orgID, inboxID, threadID
}

func assertBlobRefCount(t *testing.T, ctx context.Context, db *sql.DB, orgID, sha string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRowContext(ctx, `
		SELECT ref_count FROM attachment_blobs WHERE org_id = $1 AND sha256 = $2
	`, orgID, sha).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("blob %s ref_count=%d, want %d", sha, got, want)
	}
}

func assertColumnMissing(t *testing.T, db *sql.DB, table, column string) {
	t.Helper()
	var exists bool
	if err := db.QueryRow(`
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2
		)
	`, table, column).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatalf("column %s.%s unexpectedly exists", table, column)
	}
}
