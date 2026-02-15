package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

func TestCloudControlPlaneMigrationFromEmptyDatabase(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)

		for _, table := range []string{
			"plan_entitlements",
			"subscriptions",
			"org_entitlements",
			"org_usage_counters",
			"usage_events",
			"webhook_events",
			"cloud_api_keys",
		} {
			assertTableExists(t, db, table)
		}

		assertColumnNotNull(t, db, "threads", "org_id")
		assertColumnNotNull(t, db, "messages", "org_id")
	})
}

func TestCloudControlPlaneMigrationFromLegacyStateBackfillsOrgID(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToVersion(t, ctx, db, 1)

		inboxID := uuid.NewString()
		threadID := uuid.NewString()
		messageID := uuid.NewString()

		if _, err := db.ExecContext(ctx, `INSERT INTO inboxes (id, address, status) VALUES ($1, $2, 'active')`, inboxID, "legacy@local.neuralmail"); err != nil {
			t.Fatalf("insert legacy inbox: %v", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO threads (id, inbox_id, subject, status, participants, updated_at) VALUES ($1, $2, $3, 'open', '[]'::jsonb, now())`, threadID, inboxID, "legacy thread"); err != nil {
			t.Fatalf("insert legacy thread: %v", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO messages (id, inbox_id, thread_id, direction, text) VALUES ($1, $2, $3, 'inbound', 'legacy message')`, messageID, inboxID, threadID); err != nil {
			t.Fatalf("insert legacy message: %v", err)
		}

		migrateToLatest(t, ctx, db)

		var inboxOrgID, threadOrgID, messageOrgID string
		if err := db.QueryRowContext(ctx, `SELECT org_id::text FROM inboxes WHERE id = $1`, inboxID).Scan(&inboxOrgID); err != nil {
			t.Fatalf("query backfilled inbox org: %v", err)
		}
		if err := db.QueryRowContext(ctx, `SELECT org_id::text FROM threads WHERE id = $1`, threadID).Scan(&threadOrgID); err != nil {
			t.Fatalf("query backfilled thread org: %v", err)
		}
		if err := db.QueryRowContext(ctx, `SELECT org_id::text FROM messages WHERE id = $1`, messageID).Scan(&messageOrgID); err != nil {
			t.Fatalf("query backfilled message org: %v", err)
		}

		if inboxOrgID == "" || threadOrgID == "" || messageOrgID == "" {
			t.Fatalf("expected non-empty org ids after migration: inbox=%q thread=%q message=%q", inboxOrgID, threadOrgID, messageOrgID)
		}
		if inboxOrgID != threadOrgID || inboxOrgID != messageOrgID {
			t.Fatalf("expected all backfilled org ids to match: inbox=%s thread=%s message=%s", inboxOrgID, threadOrgID, messageOrgID)
		}
	})
}

func TestUsageEventsReplayIDPartialUniqueIndex(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)

		orgID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, $2)`, orgID, "acme"); err != nil {
			t.Fatalf("insert org: %v", err)
		}

		insertUsageEvent := func(replayID *string) error {
			return db.QueryRowContext(ctx, `
				INSERT INTO usage_events (id, org_id, meter_name, quantity, tool_name, replay_id, status)
				VALUES ($1, $2, 'mcp_units', 1, 'list_threads', $3, 'success')
				RETURNING id`,
				uuid.NewString(), orgID, replayID,
			).Scan(new(string))
		}

		replayID := "replay-1"
		if err := insertUsageEvent(&replayID); err != nil {
			t.Fatalf("insert first replay event: %v", err)
		}
		if err := insertUsageEvent(&replayID); err == nil {
			t.Fatalf("expected duplicate replay_id to violate unique partial index")
		}

		if err := insertUsageEvent(nil); err != nil {
			t.Fatalf("insert null replay event #1: %v", err)
		}
		if err := insertUsageEvent(nil); err != nil {
			t.Fatalf("insert null replay event #2: %v", err)
		}
	})
}

func TestTenantRLSBlocksCrossOrgReadsWithScopedSession(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)

		orgA := uuid.NewString()
		orgB := uuid.NewString()
		inboxA := uuid.NewString()
		inboxB := uuid.NewString()
		threadA := uuid.NewString()
		threadB := uuid.NewString()

		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'org-a'), ($2, 'org-b')`, orgA, orgB); err != nil {
			t.Fatalf("insert orgs: %v", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO inboxes (id, org_id, address, status) VALUES ($1, $2, 'a@local.neuralmail', 'active')`, inboxA, orgA); err != nil {
			t.Fatalf("insert org A inbox: %v", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO inboxes (id, org_id, address, status) VALUES ($1, $2, 'b@local.neuralmail', 'active')`, inboxB, orgB); err != nil {
			t.Fatalf("insert org B inbox: %v", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO threads (id, inbox_id, org_id, subject, status, participants, updated_at) VALUES ($1, $2, $3, 'thread-a', 'open', '[]'::jsonb, now())`, threadA, inboxA, orgA); err != nil {
			t.Fatalf("insert org A thread: %v", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO threads (id, inbox_id, org_id, subject, status, participants, updated_at) VALUES ($1, $2, $3, 'thread-b', 'open', '[]'::jsonb, now())`, threadB, inboxB, orgB); err != nil {
			t.Fatalf("insert org B thread: %v", err)
		}

		roleName := "rls_app_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD 'rls_app'`, roleName)); err != nil {
			t.Fatalf("create rls role: %v", err)
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`GRANT USAGE ON SCHEMA public TO %s`, roleName)); err != nil {
			t.Fatalf("grant schema usage: %v", err)
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE, DELETE ON inboxes, threads, messages TO %s`, roleName)); err != nil {
			t.Fatalf("grant table permissions: %v", err)
		}

		var dbName string
		if err := db.QueryRowContext(ctx, `SELECT current_database()`).Scan(&dbName); err != nil {
			t.Fatalf("resolve current database: %v", err)
		}
		baseDSN := os.Getenv("NM_TEST_DB_DSN")
		if baseDSN == "" {
			baseDSN = "postgres://neuralmail@127.0.0.1:54320/neuralmail?sslmode=disable"
		}
		appDSN, err := dsnWithDatabase(baseDSN, dbName)
		if err != nil {
			t.Fatalf("build app dsn: %v", err)
		}
		appDSN, err = dsnWithCredentials(appDSN, roleName, "rls_app")
		if err != nil {
			t.Fatalf("set app credentials: %v", err)
		}
		appDB, err := sql.Open("pgx", appDSN)
		if err != nil {
			t.Fatalf("open app role connection: %v", err)
		}
		defer appDB.Close()

		st := &Store{db: appDB, q: appDB}
		var visibleThreads int
		if err := st.RunAsOrg(ctx, orgA, func(scoped *Store) error {
			return scoped.q.QueryRowContext(ctx, `SELECT count(*) FROM threads`).Scan(&visibleThreads)
		}); err != nil {
			t.Fatalf("run as org A: %v", err)
		}
		if visibleThreads != 1 {
			t.Fatalf("expected org A to see 1 thread via RLS, got %d", visibleThreads)
		}

		var crossOrgRows int
		if err := st.RunAsOrg(ctx, orgA, func(scoped *Store) error {
			return scoped.q.QueryRowContext(ctx, `SELECT count(*) FROM threads WHERE id = $1`, threadB).Scan(&crossOrgRows)
		}); err != nil {
			t.Fatalf("run cross-org lookup: %v", err)
		}
		if crossOrgRows != 0 {
			t.Fatalf("expected org A to see 0 rows for org B thread, got %d", crossOrgRows)
		}
	})
}

func TestOrgDomainsMigrationAppliesCleanly(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)

		assertTableExists(t, db, "org_domains")

		// Verify new columns on inboxes
		assertColumnExists(t, db, "inboxes", "org_domain_id")

		// Verify new columns on entitlements
		assertColumnExists(t, db, "plan_entitlements", "max_domains")
		assertColumnExists(t, db, "org_entitlements", "max_domains")
	})
}

func TestOrgDomainsCanonicalCheckConstraint(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)

		orgID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'acme')`, orgID); err != nil {
			t.Fatalf("insert org: %v", err)
		}

		// Valid lowercase domain should succeed
		_, err := db.ExecContext(ctx, `
			INSERT INTO org_domains (org_id, domain, verification_token, dkim_selector, dkim_method)
			VALUES ($1, 'acme.com', 'tok-1', 'nerve', 'cname')
		`, orgID)
		if err != nil {
			t.Fatalf("insert valid domain: %v", err)
		}

		// Uppercase domain should fail check constraint
		_, err = db.ExecContext(ctx, `
			INSERT INTO org_domains (org_id, domain, verification_token, dkim_selector, dkim_method)
			VALUES ($1, 'ACME.COM', 'tok-2', 'nerve', 'cname')
		`, orgID)
		if err == nil {
			t.Fatal("expected CHECK constraint to reject uppercase domain")
		}

		// Trailing dot should fail check constraint
		_, err = db.ExecContext(ctx, `
			INSERT INTO org_domains (org_id, domain, verification_token, dkim_selector, dkim_method)
			VALUES ($1, 'acme.com.', 'tok-3', 'nerve', 'cname')
		`, orgID)
		if err == nil {
			t.Fatal("expected CHECK constraint to reject trailing dot domain")
		}
	})
}

func TestOrgDomainsPartialUniqueIndex(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)

		orgA := uuid.NewString()
		orgB := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'org-a'), ($2, 'org-b')`, orgA, orgB); err != nil {
			t.Fatalf("insert orgs: %v", err)
		}

		// Multiple pending claims for the same domain should be allowed
		_, err := db.ExecContext(ctx, `
			INSERT INTO org_domains (org_id, domain, verification_token, dkim_selector, dkim_method)
			VALUES ($1, 'shared.com', 'tok-a', 'nerve', 'cname')
		`, orgA)
		if err != nil {
			t.Fatalf("insert pending domain A: %v", err)
		}
		_, err = db.ExecContext(ctx, `
			INSERT INTO org_domains (org_id, domain, verification_token, dkim_selector, dkim_method)
			VALUES ($1, 'shared.com', 'tok-b', 'nerve', 'cname')
		`, orgB)
		if err != nil {
			t.Fatalf("insert pending domain B: %v", err)
		}

		// But verified domains should enforce uniqueness
		_, err = db.ExecContext(ctx, `
			INSERT INTO org_domains (org_id, domain, verification_token, dkim_selector, dkim_method, status)
			VALUES ($1, 'unique.com', 'tok-1', 'nerve', 'cname', 'active')
		`, orgA)
		if err != nil {
			t.Fatalf("insert active domain A: %v", err)
		}
		_, err = db.ExecContext(ctx, `
			INSERT INTO org_domains (org_id, domain, verification_token, dkim_selector, dkim_method, status)
			VALUES ($1, 'unique.com', 'tok-2', 'nerve', 'cname', 'active')
		`, orgB)
		if err == nil {
			t.Fatal("expected partial unique index to reject duplicate active domain")
		}
	})
}

func TestOrgDomainsPendingExpiry(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)

		orgID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'acme')`, orgID); err != nil {
			t.Fatalf("insert org: %v", err)
		}

		// Insert a domain with expired expires_at
		_, err := db.ExecContext(ctx, `
			INSERT INTO org_domains (org_id, domain, verification_token, dkim_selector, dkim_method, expires_at)
			VALUES ($1, 'expired.com', 'tok-exp', 'nerve', 'cname', now() - interval '1 day')
		`, orgID)
		if err != nil {
			t.Fatalf("insert expired domain: %v", err)
		}

		// Insert a domain with future expires_at
		_, err = db.ExecContext(ctx, `
			INSERT INTO org_domains (org_id, domain, verification_token, dkim_selector, dkim_method, expires_at)
			VALUES ($1, 'fresh.com', 'tok-fresh', 'nerve', 'cname', now() + interval '6 days')
		`, orgID)
		if err != nil {
			t.Fatalf("insert fresh domain: %v", err)
		}

		// ExpirePendingDomains should delete the expired one
		result, err := db.ExecContext(ctx, `DELETE FROM org_domains WHERE status = 'pending' AND expires_at < now()`)
		if err != nil {
			t.Fatalf("expire pending domains: %v", err)
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			t.Fatalf("expected 1 expired domain deleted, got %d", n)
		}

		// The fresh domain should still exist
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM org_domains WHERE org_id = $1`, orgID).Scan(&count); err != nil {
			t.Fatalf("count remaining: %v", err)
		}
		if count != 1 {
			t.Fatalf("expected 1 remaining domain, got %d", count)
		}
	})
}

func TestOrgDomainsStoreCreateAndGet(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)

		orgID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'acme')`, orgID); err != nil {
			t.Fatalf("insert org: %v", err)
		}

		st := &Store{db: db, q: db}

		id, err := st.CreateOrgDomain(ctx, orgID, "acme.com", "nerve-verification=abc123", "nerve2026a", "encrypted-key", "public-key", "cname")
		if err != nil {
			t.Fatalf("create domain: %v", err)
		}
		if id == "" {
			t.Fatal("expected non-empty domain ID")
		}

		// Get by ID
		d, err := st.GetOrgDomainByID(ctx, id)
		if err != nil {
			t.Fatalf("get domain by ID: %v", err)
		}
		if d.Domain != "acme.com" {
			t.Fatalf("expected domain 'acme.com', got %q", d.Domain)
		}
		if d.Status != "pending" {
			t.Fatalf("expected status 'pending', got %q", d.Status)
		}
		if d.VerificationToken != "nerve-verification=abc123" {
			t.Fatalf("expected token 'nerve-verification=abc123', got %q", d.VerificationToken)
		}
		if d.DKIMSelector != "nerve2026a" {
			t.Fatalf("expected selector 'nerve2026a', got %q", d.DKIMSelector)
		}
		if d.DKIMMethod != "cname" {
			t.Fatalf("expected method 'cname', got %q", d.DKIMMethod)
		}
		if !d.ExpiresAt.Valid {
			t.Fatal("expected expires_at to be set for pending domain")
		}

		// Get by domain name
		d2, err := st.GetOrgDomain(ctx, "acme.com")
		if err != nil {
			t.Fatalf("get domain by name: %v", err)
		}
		if d2.ID != id {
			t.Fatalf("expected ID %q, got %q", id, d2.ID)
		}

		// List
		domains, err := st.ListOrgDomains(ctx, orgID)
		if err != nil {
			t.Fatalf("list domains: %v", err)
		}
		if len(domains) != 1 {
			t.Fatalf("expected 1 domain, got %d", len(domains))
		}

		// Update verification
		if err := st.UpdateOrgDomainVerification(ctx, id, false, true, true, true, "verified_dns"); err != nil {
			t.Fatalf("update verification: %v", err)
		}
		d3, _ := st.GetOrgDomainByID(ctx, id)
		if d3.Status != "verified_dns" {
			t.Fatalf("expected status 'verified_dns', got %q", d3.Status)
		}
		if !d3.SPFVerified || !d3.DKIMVerified || !d3.DMARCVerified {
			t.Fatal("expected SPF, DKIM, DMARC all verified")
		}
		if d3.MXVerified {
			t.Fatal("expected MX not verified")
		}

		// Update status
		if err := st.UpdateOrgDomainStatus(ctx, id, "active"); err != nil {
			t.Fatalf("update status: %v", err)
		}
		d4, _ := st.GetOrgDomainByID(ctx, id)
		if d4.Status != "active" {
			t.Fatalf("expected status 'active', got %q", d4.Status)
		}

		// GetOrgDomainForSending
		d5, err := st.GetOrgDomainForSending(ctx, "acme.com")
		if err != nil {
			t.Fatalf("get domain for sending: %v", err)
		}
		if d5.ID != id {
			t.Fatalf("expected ID %q, got %q", id, d5.ID)
		}

		// Count
		count, err := st.CountDomainsByOrg(ctx, orgID)
		if err != nil {
			t.Fatalf("count domains: %v", err)
		}
		if count != 1 {
			t.Fatalf("expected count 1, got %d", count)
		}

		// Delete
		if err := st.DeleteOrgDomain(ctx, id); err != nil {
			t.Fatalf("delete domain: %v", err)
		}
		domains2, _ := st.ListOrgDomains(ctx, orgID)
		if len(domains2) != 0 {
			t.Fatalf("expected 0 domains after delete, got %d", len(domains2))
		}
	})
}

func assertColumnExists(t *testing.T, db *sql.DB, table, column string) {
	t.Helper()
	var colName string
	if err := db.QueryRow(`
		SELECT column_name
		FROM information_schema.columns
		WHERE table_schema = 'public'
		  AND table_name = $1
		  AND column_name = $2
	`, table, column).Scan(&colName); err != nil {
		t.Fatalf("expected column %s.%s to exist: %v", table, column, err)
	}
}

func migrateToLatest(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	goose.SetDialect("postgres")
	goose.SetTableName("schema_migrations")
	if err := goose.UpContext(ctx, db, migrationDir(t)); err != nil {
		t.Fatalf("apply latest migrations: %v", err)
	}
}

func migrateToVersion(t *testing.T, ctx context.Context, db *sql.DB, version int64) {
	t.Helper()
	goose.SetDialect("postgres")
	goose.SetTableName("schema_migrations")
	if err := goose.UpToContext(ctx, db, migrationDir(t), version); err != nil {
		t.Fatalf("apply migrations to version %d: %v", version, err)
	}
}

func assertTableExists(t *testing.T, db *sql.DB, table string) {
	t.Helper()
	var regclass sql.NullString
	if err := db.QueryRow(`SELECT to_regclass($1)`, "public."+table).Scan(&regclass); err != nil {
		t.Fatalf("lookup table %s: %v", table, err)
	}
	if !regclass.Valid {
		t.Fatalf("expected table %s to exist", table)
	}
}

func assertColumnNotNull(t *testing.T, db *sql.DB, table, column string) {
	t.Helper()
	var nullable string
	if err := db.QueryRow(`
		SELECT is_nullable
		FROM information_schema.columns
		WHERE table_schema = 'public'
		  AND table_name = $1
		  AND column_name = $2
	`, table, column).Scan(&nullable); err != nil {
		t.Fatalf("lookup %s.%s nullability: %v", table, column, err)
	}
	if nullable != "NO" {
		t.Fatalf("expected %s.%s to be NOT NULL, got %s", table, column, nullable)
	}
}

func withTempDatabase(t *testing.T, run func(ctx context.Context, db *sql.DB)) {
	t.Helper()

	baseDSN := os.Getenv("NM_TEST_DB_DSN")
	if baseDSN == "" {
		baseDSN = "postgres://neuralmail:neuralmail@127.0.0.1:54320/neuralmail?sslmode=disable"
	}
	adminDSN, err := dsnWithDatabase(baseDSN, "postgres")
	if err != nil {
		t.Fatalf("build admin dsn: %v", err)
	}

	adminDB, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatalf("open admin database: %v", err)
	}
	defer adminDB.Close()

	pingCtx, pingCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer pingCancel()
	if err := adminDB.PingContext(pingCtx); err != nil {
		t.Skipf("postgres unavailable for migration tests (%s): %v", adminDSN, err)
	}

	dbName := "nerve_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := adminDB.ExecContext(context.Background(), fmt.Sprintf(`CREATE DATABASE %s`, dbName)); err != nil {
		t.Fatalf("create temp database %s: %v", dbName, err)
	}

	testDSN, err := dsnWithDatabase(baseDSN, dbName)
	if err != nil {
		t.Fatalf("build test dsn: %v", err)
	}
	db, err := sql.Open("pgx", testDSN)
	if err != nil {
		t.Fatalf("open temp database: %v", err)
	}

	t.Cleanup(func() {
		_ = db.Close()
		_, _ = adminDB.ExecContext(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, dbName)
		_, _ = adminDB.ExecContext(context.Background(), fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, dbName))
	})

	run(context.Background(), db)
}

func dsnWithDatabase(rawDSN, dbName string) (string, error) {
	parsed, err := url.Parse(rawDSN)
	if err != nil {
		return "", err
	}
	parsed.Path = "/" + dbName
	return parsed.String(), nil
}

func dsnWithCredentials(rawDSN, user, password string) (string, error) {
	parsed, err := url.Parse(rawDSN)
	if err != nil {
		return "", err
	}
	parsed.User = url.UserPassword(user, password)
	return parsed.String(), nil
}

func migrationDir(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("resolve migration directory: missing caller info")
	}
	return filepath.Join(filepath.Dir(currentFile), "migrations")
}
