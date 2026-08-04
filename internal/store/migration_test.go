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

		assertTableExists(t, db, "schema_migrations_core")
		assertTableExists(t, db, "schema_migrations_cloud")

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
		assertColumnExists(t, db, "orgs", "mcp_endpoint")
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
	if err := MigrateAll(ctx, db); err != nil {
		t.Fatalf("apply latest core/cloud migrations: %v", err)
	}
}

func migrateToVersion(t *testing.T, ctx context.Context, db *sql.DB, version int64) {
	t.Helper()
	goose.SetDialect("postgres")
	goose.SetTableName(migrationTableCore)
	if err := goose.UpToContext(ctx, db, coreMigrationDir(t), version); err != nil {
		t.Fatalf("apply core migrations to version %d: %v", version, err)
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

func coreMigrationDir(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("resolve migration directory: missing caller info")
	}
	return filepath.Join(filepath.Dir(currentFile), "migrations", "core")
}

// TestMigrateUpToCore_StopsAtTarget is the guard for the staged rollout: the
// expand step must be able to land without dragging the relax step in behind
// it. A plain MigrateCore applies everything, so a target-version path is the
// only way to deploy readers between two migrations.
func TestMigrateUpToCore_StopsAtTarget(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		const target = int64(5)

		if err := MigrateUpToCore(ctx, db, target); err != nil {
			t.Fatalf("migrate core to %d: %v", target, err)
		}
		got, err := CurrentVersionCore(ctx, db)
		if err != nil {
			t.Fatalf("current core version: %v", err)
		}
		if got != target {
			t.Fatalf("expected core version %d, got %d", target, got)
		}

		// A later migration must still be pending, not silently applied.
		if tableExists(ctx, t, db, "outbox_messages") {
			t.Fatal("outbox_messages exists at version 5; a later migration was applied")
		}

		// Completing the run reaches the head and creates it.
		if err := MigrateCore(ctx, db); err != nil {
			t.Fatalf("migrate core to head: %v", err)
		}
		head, err := CurrentVersionCore(ctx, db)
		if err != nil {
			t.Fatalf("current core version after head: %v", err)
		}
		if head <= target {
			t.Fatalf("expected head > %d, got %d", target, head)
		}
		if !tableExists(ctx, t, db, "outbox_messages") {
			t.Fatal("outbox_messages missing after migrating to head")
		}

		// UpToContext itself treats both an already-passed target and a target
		// beyond the available migrations as successful no-ops. The store API
		// must reject both because its contract is to reach the exact target.
		for _, tc := range []struct {
			target  int64
			message string
		}{
			{target: head - 1, message: "already passed"},
			{target: head + 1, message: "is not available"},
		} {
			err := MigrateUpToCore(ctx, db, tc.target)
			if err == nil {
				t.Fatalf("expected exact-target migration to %d to fail from version %d", tc.target, head)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("migration target %d", tc.target)) ||
				!strings.Contains(err.Error(), tc.message) {
				t.Fatalf("unexpected exact-target error for %d: %v", tc.target, err)
			}
		}

		migrations, err := goose.CollectMigrations(coreMigrationDir(t), 0, head)
		if err != nil {
			t.Fatalf("collect core migrations through head: %v", err)
		}
		if len(migrations) < 2 || migrations[len(migrations)-1].Version != head {
			t.Fatalf("expected at least two core migrations ending at head %d, got %v", head, migrations)
		}
		previous := migrations[len(migrations)-2].Version

		// Down must undo exactly one step.
		if err := MigrateDownCore(ctx, db); err != nil {
			t.Fatalf("migrate down core: %v", err)
		}
		afterDown, err := CurrentVersionCore(ctx, db)
		if err != nil {
			t.Fatalf("current core version after down: %v", err)
		}
		if afterDown != previous {
			t.Fatalf("expected one-step rollback from %d to %d, got %d", head, previous, afterDown)
		}
	})
}

func TestMigrateUpToCloud_RejectsUnavailableTargetBeforeApplying(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		// Cloud migrations intentionally jump from 1 to 3. Target 2 must fail
		// before version 1 is applied rather than partially migrating the DB.
		err := MigrateUpToCloud(ctx, db, 2)
		if err == nil {
			t.Fatal("expected unavailable cloud target to fail")
		}
		if !strings.Contains(err.Error(), "migration target 2 is not available") {
			t.Fatalf("unexpected unavailable-target error: %v", err)
		}
		if tableExists(ctx, t, db, "plan_entitlements") {
			t.Fatal("cloud version 1 was applied before unavailable target 2 failed")
		}
		if tableExists(ctx, t, db, migrationTableCloud) {
			t.Fatal("cloud migration table was created before unavailable target 2 failed")
		}
	})
}

func TestCurrentVersionDoesNotInitializeMigrationTables(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		core, err := CurrentVersionCore(ctx, db)
		if err != nil {
			t.Fatalf("current core version: %v", err)
		}
		cloud, err := CurrentVersionCloud(ctx, db)
		if err != nil {
			t.Fatalf("current cloud version: %v", err)
		}
		if core != 0 || cloud != 0 {
			t.Fatalf("fresh database versions = core %d, cloud %d; want 0, 0", core, cloud)
		}
		if err := MigrateUpToCore(ctx, db, 0); err != nil {
			t.Fatalf("migrate core to current zero version: %v", err)
		}
		if err := MigrateUpToCloud(ctx, db, 0); err != nil {
			t.Fatalf("migrate cloud to current zero version: %v", err)
		}
		if tableExists(ctx, t, db, migrationTableCore) || tableExists(ctx, t, db, migrationTableCloud) {
			t.Fatal("read-only version inspection or up-to-current zero created a migration table")
		}
	})
}

func TestMigrationStatusOnFreshDatabaseIsReadOnly(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		coreVersions := migrationVersions(t, "core")
		cloudVersions := migrationVersions(t, "cloud")
		core, err := MigrationStatusCore(ctx, db)
		core = requireMigrationStatus(t, core, err)
		cloud, err := MigrationStatusCloud(ctx, db)
		cloud = requireMigrationStatus(t, cloud, err)

		assertMigrationStatus(t, core, 0, coreVersions)
		assertMigrationStatus(t, cloud, 0, cloudVersions)
		assertVersionAbsent(t, cloud.Pending, 2)
		if tableExists(ctx, t, db, migrationTableCore) || tableExists(ctx, t, db, migrationTableCloud) {
			t.Fatal("read-only migration status created a migration table")
		}
	})
}

func TestMigrationStatusCoreReportsActualPendingVersions(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		const target = int64(5)
		if err := MigrateUpToCore(ctx, db, target); err != nil {
			t.Fatalf("migrate core to %d: %v", target, err)
		}

		versions := migrationVersions(t, "core")
		status, err := MigrationStatusCore(ctx, db)
		status = requireMigrationStatus(t, status, err)
		assertMigrationStatus(t, status, target, versionsAfter(versions, target))

		if err := MigrateCore(ctx, db); err != nil {
			t.Fatalf("migrate core to head: %v", err)
		}
		atHead, err := MigrationStatusCore(ctx, db)
		atHead = requireMigrationStatus(t, atHead, err)
		assertMigrationStatus(t, atHead, versions[len(versions)-1], []int64{})
	})
}

func TestMigrationStatusCloudPreservesSparsePendingVersions(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if err := MigrateCore(ctx, db); err != nil {
			t.Fatalf("migrate core prerequisite: %v", err)
		}
		if err := MigrateUpToCloud(ctx, db, 1); err != nil {
			t.Fatalf("migrate cloud to 1: %v", err)
		}

		versions := migrationVersions(t, "cloud")
		status, err := MigrationStatusCloud(ctx, db)
		status = requireMigrationStatus(t, status, err)
		assertMigrationStatus(t, status, 1, versionsAfter(versions, 1))
		assertVersionAbsent(t, status.Pending, 2)
	})
}

func TestMigrationStatusRejectsUnknownCurrentVersion(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if err := MigrateCore(ctx, db); err != nil {
			t.Fatalf("migrate core prerequisite: %v", err)
		}
		if err := MigrateUpToCloud(ctx, db, 1); err != nil {
			t.Fatalf("migrate cloud to 1: %v", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations_cloud (version_id, is_applied) VALUES (2, true)`); err != nil {
			t.Fatalf("inject unknown cloud migration version: %v", err)
		}

		_, err := MigrationStatusCloud(ctx, db)
		if err == nil || !strings.Contains(err.Error(), "current migration version 2 is not available") {
			t.Fatalf("expected unknown-current error, got %v", err)
		}
	})
}

func TestMigrationStatusRejectsCurrentVersionAheadOfHead(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if err := MigrateCore(ctx, db); err != nil {
			t.Fatalf("migrate core to head: %v", err)
		}
		versions := migrationVersions(t, "core")
		head := versions[len(versions)-1]
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations_core (version_id, is_applied) VALUES ($1, true)`, head+1); err != nil {
			t.Fatalf("inject core migration version ahead of head: %v", err)
		}

		_, err := MigrationStatusCore(ctx, db)
		want := fmt.Sprintf("current migration version %d is ahead of migration head %d", head+1, head)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("expected ahead-of-head error %q, got %v", want, err)
		}
	})
}

func requireMigrationStatus(t *testing.T, status MigrationStatus, err error) MigrationStatus {
	t.Helper()
	if err != nil {
		t.Fatalf("migration status: %v", err)
	}
	return status
}

func assertMigrationStatus(t *testing.T, status MigrationStatus, current int64, pending []int64) {
	t.Helper()
	if status.Current != current {
		t.Fatalf("current migration version = %d; want %d", status.Current, current)
	}
	if status.Pending == nil {
		t.Fatal("pending migration versions are nil; want a non-nil slice")
	}
	if len(status.Pending) != len(pending) {
		t.Fatalf("pending migration versions = %v; want %v", status.Pending, pending)
	}
	for i := range pending {
		if status.Pending[i] != pending[i] {
			t.Fatalf("pending migration versions = %v; want %v", status.Pending, pending)
		}
	}

	wantHead := int64(0)
	allVersions := append([]int64{current}, pending...)
	for _, version := range allVersions {
		if version > wantHead {
			wantHead = version
		}
	}
	if status.Head != wantHead {
		t.Fatalf("migration head = %d; want %d", status.Head, wantHead)
	}
}

func migrationVersions(t *testing.T, scope string) []int64 {
	t.Helper()
	migrations, err := goose.CollectMigrations(migrationDir(scope), 0, goose.MaxVersion)
	if err != nil {
		t.Fatalf("collect %s migrations: %v", scope, err)
	}
	versions := make([]int64, 0, len(migrations))
	for _, migration := range migrations {
		versions = append(versions, migration.Version)
	}
	return versions
}

func versionsAfter(versions []int64, current int64) []int64 {
	pending := make([]int64, 0, len(versions))
	for _, version := range versions {
		if version > current {
			pending = append(pending, version)
		}
	}
	return pending
}

func assertVersionAbsent(t *testing.T, versions []int64, absent int64) {
	t.Helper()
	for _, version := range versions {
		if version == absent {
			t.Fatalf("migration versions = %v; synthetic gap version %d must be absent", versions, absent)
		}
	}
}

func TestWithGooseSerializesConfigurationAndOperation(t *testing.T) {
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)

	go func() {
		firstDone <- withGoose(migrationTableCore, func() error {
			close(firstEntered)
			<-releaseFirst
			if got := goose.TableName(); got != migrationTableCore {
				return fmt.Errorf("first operation inherited table %q", got)
			}
			return nil
		})
	}()
	<-firstEntered

	secondStarted := make(chan struct{})
	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		close(secondStarted)
		secondDone <- withGoose(migrationTableCloud, func() error {
			close(secondEntered)
			if got := goose.TableName(); got != migrationTableCloud {
				return fmt.Errorf("second operation inherited table %q", got)
			}
			return nil
		})
	}()
	<-secondStarted

	select {
	case <-secondEntered:
		close(releaseFirst)
		<-firstDone
		<-secondDone
		t.Fatal("second goose operation entered before the first completed")
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-secondEntered:
	case <-time.After(time.Second):
		t.Fatal("second goose operation did not enter after the first completed")
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
}

func tableExists(ctx context.Context, t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var exists bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1)`,
		name).Scan(&exists); err != nil {
		t.Fatalf("check table %s: %v", name, err)
	}
	return exists
}
