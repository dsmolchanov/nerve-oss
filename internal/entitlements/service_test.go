package entitlements

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"neuralmail/internal/auth"
	"neuralmail/internal/config"
	"neuralmail/internal/store"
)

func TestAtomicReserveNoOvershootUnderConcurrency(t *testing.T) {
	withTempStore(t, func(ctx context.Context, st *store.Store) {
		orgID := uuid.NewString()
		periodStart := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
		periodEnd := periodStart.Add(30 * 24 * time.Hour)
		monthlyUnits := int64(20)

		insertEntitlementFixture(t, ctx, st, orgID, periodStart, periodEnd, monthlyUnits, 100000)

		svc := NewService(config.Default(), st, nil)
		svc.Now = func() time.Time { return time.Date(2026, 2, 7, 12, 0, 0, 0, time.UTC) }

		var successCount atomic.Int64
		var quotaCount atomic.Int64
		var otherErrCount atomic.Int64

		var wg sync.WaitGroup
		for i := 0; i < 100; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := svc.PreAuthorizeTool(ctx, auth.Principal{OrgID: orgID}, "list_threads", "")
				switch {
				case err == nil:
					successCount.Add(1)
				case errors.Is(err, ErrQuotaExceeded):
					quotaCount.Add(1)
				default:
					otherErrCount.Add(1)
				}
			}()
		}
		wg.Wait()

		if successCount.Load() != monthlyUnits {
			t.Fatalf("expected %d successful reservations, got %d", monthlyUnits, successCount.Load())
		}
		if quotaCount.Load() != 80 {
			t.Fatalf("expected 80 quota denials, got %d", quotaCount.Load())
		}
		if otherErrCount.Load() != 0 {
			t.Fatalf("expected 0 non-quota errors, got %d", otherErrCount.Load())
		}

		used, err := st.GetOrgUsageCounterUsed(ctx, orgID, meterMCPUnits, periodStart)
		if err != nil {
			t.Fatalf("query usage counter: %v", err)
		}
		if used != monthlyUnits {
			t.Fatalf("expected used=%d, got %d", monthlyUnits, used)
		}
	})
}

func TestPreAuthorizeToolRollsUsagePeriodForward(t *testing.T) {
	withTempStore(t, func(ctx context.Context, st *store.Store) {
		orgID := uuid.NewString()
		now := time.Date(2026, 2, 7, 12, 0, 0, 0, time.UTC)
		oldStart := now.Add(-60 * 24 * time.Hour)
		oldEnd := now.Add(-30 * 24 * time.Hour)

		insertEntitlementFixture(t, ctx, st, orgID, oldStart, oldEnd, 100, 100000)
		if _, err := st.DB().ExecContext(ctx, `
			INSERT INTO org_usage_counters (org_id, meter_name, period_start, period_end, used)
			VALUES ($1, $2, $3, $4, 5)
		`, orgID, meterMCPUnits, oldStart, oldEnd); err != nil {
			t.Fatalf("insert old usage counter: %v", err)
		}

		svc := NewService(config.Default(), st, nil)
		svc.Now = func() time.Time { return now }

		reservation, err := svc.PreAuthorizeTool(ctx, auth.Principal{OrgID: orgID}, "list_threads", "replay-1")
		if err != nil {
			t.Fatalf("pre-authorize tool: %v", err)
		}
		if !reservation.PeriodStart.After(oldStart) {
			t.Fatalf("expected rolled period start after old start: old=%s new=%s", oldStart, reservation.PeriodStart)
		}

		ent, err := st.GetOrgEntitlement(ctx, orgID)
		if err != nil {
			t.Fatalf("fetch entitlement after rollover: %v", err)
		}
		if !ent.UsagePeriodStart.Equal(reservation.PeriodStart) {
			t.Fatalf("expected entitlement usage_period_start to match reservation")
		}

		oldUsed, err := st.GetOrgUsageCounterUsed(ctx, orgID, meterMCPUnits, oldStart)
		if err != nil {
			t.Fatalf("query old usage counter: %v", err)
		}
		if oldUsed != 5 {
			t.Fatalf("expected old period usage to remain 5, got %d", oldUsed)
		}

		newUsed, err := st.GetOrgUsageCounterUsed(ctx, orgID, meterMCPUnits, reservation.PeriodStart)
		if err != nil {
			t.Fatalf("query new usage counter: %v", err)
		}
		if newUsed != 1 {
			t.Fatalf("expected new period usage to be 1, got %d", newUsed)
		}
	})
}

func insertEntitlementFixture(t *testing.T, ctx context.Context, st *store.Store, orgID string, periodStart, periodEnd time.Time, monthlyUnits int64, mcpRPM int) {
	t.Helper()
	if _, err := st.DB().ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, $2)`, orgID, "entitlements-test"); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `
		INSERT INTO org_entitlements (
			org_id, plan_code, subscription_status, mcp_rpm, monthly_units, max_inboxes,
			usage_period_start, usage_period_end
		) VALUES ($1, 'pro', 'active', $2, $3, 10, $4, $5)
	`, orgID, mcpRPM, monthlyUnits, periodStart, periodEnd); err != nil {
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
		t.Skipf("postgres unavailable for entitlement tests: %v", err)
	}

	dbName := "nerve_ent_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := adminDB.ExecContext(context.Background(), fmt.Sprintf(`CREATE DATABASE %s`, dbName)); err != nil {
		t.Fatalf("create test db %s: %v", dbName, err)
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
