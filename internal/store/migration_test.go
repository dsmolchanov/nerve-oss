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
