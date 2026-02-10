package reconcile

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
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"neuralmail/internal/store"
)

func TestRunRepairsUsageDrift(t *testing.T) {
	withTempStore(t, func(ctx context.Context, st *store.Store) {
		orgID := uuid.NewString()
		now := time.Date(2026, 2, 7, 12, 0, 0, 0, time.UTC)
		start := now.Add(-24 * time.Hour)
		end := now.Add(24 * time.Hour)

		insertOrgAndEntitlement(t, ctx, st, orgID, start, end)
		if _, err := st.DB().ExecContext(ctx, `
			INSERT INTO org_usage_counters (org_id, meter_name, period_start, period_end, used)
			VALUES ($1, 'mcp_units', $2, $3, 10)
		`, orgID, start, end); err != nil {
			t.Fatalf("insert usage counter: %v", err)
		}
		if _, err := st.DB().ExecContext(ctx, `
			INSERT INTO usage_events (org_id, meter_name, quantity, tool_name, status, created_at)
			VALUES
			  ($1, 'mcp_units', 3, 'list_threads', 'success', $2),
			  ($1, 'mcp_units', 1, 'get_thread', 'success', $2),
			  ($1, 'mcp_units', 9, 'send_reply', 'failed', $2)
		`, orgID, now); err != nil {
			t.Fatalf("insert usage events: %v", err)
		}

		svc := NewService(st)
		svc.Now = func() time.Time { return now }
		report, err := svc.Run(ctx)
		if err != nil {
			t.Fatalf("run reconciliation: %v", err)
		}
		if report.CountersRepaired != 1 {
			t.Fatalf("expected 1 repaired counter, got %d", report.CountersRepaired)
		}

		used, err := st.GetOrgUsageCounterUsed(ctx, orgID, "mcp_units", start)
		if err != nil {
			t.Fatalf("query repaired usage: %v", err)
		}
		if used != 4 {
			t.Fatalf("expected repaired usage=4, got %d", used)
		}
	})
}

func TestRunBackstopRolloverCreatesNewPeriodCounter(t *testing.T) {
	withTempStore(t, func(ctx context.Context, st *store.Store) {
		orgID := uuid.NewString()
		now := time.Date(2026, 2, 7, 12, 0, 0, 0, time.UTC)
		start := now.Add(-60 * 24 * time.Hour)
		end := now.Add(-30 * 24 * time.Hour)

		insertOrgAndEntitlement(t, ctx, st, orgID, start, end)
		if _, err := st.DB().ExecContext(ctx, `
			INSERT INTO org_usage_counters (org_id, meter_name, period_start, period_end, used)
			VALUES ($1, 'mcp_units', $2, $3, 0)
		`, orgID, start, end); err != nil {
			t.Fatalf("insert old usage counter: %v", err)
		}

		svc := NewService(st)
		svc.Now = func() time.Time { return now }
		report, err := svc.Run(ctx)
		if err != nil {
			t.Fatalf("run reconciliation: %v", err)
		}
		if report.PeriodsRolled != 1 {
			t.Fatalf("expected 1 rolled period, got %d", report.PeriodsRolled)
		}

		ent, err := st.GetOrgEntitlement(ctx, orgID)
		if err != nil {
			t.Fatalf("query rolled entitlement: %v", err)
		}
		if !ent.UsagePeriodEnd.After(now) && !ent.UsagePeriodEnd.Equal(now) {
			t.Fatalf("expected rolled usage period end >= now, got %s", ent.UsagePeriodEnd)
		}
		if _, err := st.GetOrgUsageCounterUsed(ctx, orgID, "mcp_units", ent.UsagePeriodStart); err != nil {
			t.Fatalf("expected counter for rolled period start: %v", err)
		}
	})
}

func insertOrgAndEntitlement(t *testing.T, ctx context.Context, st *store.Store, orgID string, periodStart, periodEnd time.Time) {
	t.Helper()
	if _, err := st.DB().ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'reconcile-org')`, orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `
		INSERT INTO org_entitlements (
			org_id, plan_code, subscription_status, mcp_rpm, monthly_units, max_inboxes,
			usage_period_start, usage_period_end
		) VALUES ($1, 'pro', 'active', 1000, 100, 10, $2, $3)
	`, orgID, periodStart, periodEnd); err != nil {
		t.Fatalf("insert org entitlement: %v", err)
	}
}

func withTempStore(t *testing.T, run func(ctx context.Context, st *store.Store)) {
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
		t.Fatalf("open admin db: %v", err)
	}
	defer adminDB.Close()

	pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := adminDB.PingContext(pingCtx); err != nil {
		t.Skipf("postgres unavailable for reconcile tests: %v", err)
	}

	dbName := "nerve_rec_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := adminDB.ExecContext(context.Background(), fmt.Sprintf(`CREATE DATABASE %s`, dbName)); err != nil {
		t.Fatalf("create test db: %v", err)
	}
	testDSN, err := dsnWithDatabase(baseDSN, dbName)
	if err != nil {
		t.Fatalf("build test dsn: %v", err)
	}
	st, err := store.Open(testDSN)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	goose.SetDialect("postgres")
	goose.SetTableName("schema_migrations")
	if err := goose.UpContext(context.Background(), st.DB(), migrationDir(t)); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	t.Cleanup(func() {
		_, _ = adminDB.ExecContext(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, dbName)
		_, _ = adminDB.ExecContext(context.Background(), fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, dbName))
	})

	run(context.Background(), st)
}

func dsnWithDatabase(rawDSN, dbName string) (string, error) {
	parsed, err := url.Parse(rawDSN)
	if err != nil {
		return "", err
	}
	parsed.Path = "/" + dbName
	return parsed.String(), nil
}

func migrationDir(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("resolve migration dir: missing caller")
	}
	return filepath.Join(filepath.Dir(currentFile), "..", "store", "migrations")
}
