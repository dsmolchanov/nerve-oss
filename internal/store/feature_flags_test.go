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

func TestMigration26FeatureFlagUniquenessAndDownGuard(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if err := MigrateUpToCore(ctx, db, 25); err != nil {
			t.Fatal(err)
		}
		orgID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'feature-flags')`, orgID); err != nil {
			t.Fatal(err)
		}
		if err := MigrateUpToCore(ctx, db, 26); err != nil {
			t.Fatal(err)
		}
		assertTableExists(t, db, "org_feature_flags")
		assertTenantRLSState(t, db, "org_feature_flags", true)

		if _, err := db.ExecContext(ctx, `
			INSERT INTO org_feature_flags (org_id, flag, enabled, updated_by)
			VALUES (NULL, 'attachments', false, 'test')
		`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO org_feature_flags (org_id, flag, enabled, updated_by)
			VALUES (NULL, 'attachments', true, 'duplicate')
		`); err == nil {
			t.Fatal("duplicate global feature flag unexpectedly succeeded")
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO org_feature_flags (org_id, flag, enabled, updated_by)
			VALUES ($1, 'attachments', true, 'test')
		`, orgID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO org_feature_flags (org_id, flag, enabled, updated_by)
			VALUES ($1, 'attachments', false, 'duplicate')
		`, orgID); err == nil {
			t.Fatal("duplicate org feature flag unexpectedly succeeded")
		}

		if err := MigrateDownCore(ctx, db); err == nil || !strings.Contains(err.Error(), "feature flag rows exist") {
			t.Fatalf("populated migration down err=%v", err)
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM org_feature_flags`); err != nil {
			t.Fatal(err)
		}
		if err := MigrateDownCore(ctx, db); err != nil {
			t.Fatal(err)
		}
		version, err := CurrentVersionCore(ctx, db)
		if err != nil || version != 25 {
			t.Fatalf("version=%d err=%v after migration down", version, err)
		}
	})
}

func TestFeatureFlagStoreIsIdempotentAndPreservesExplicitFalse(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'feature-store')`, orgID); err != nil {
			t.Fatal(err)
		}

		changed, err := st.SetFeatureFlag(ctx, nil, "attachments", true, "first")
		if err != nil || !changed {
			t.Fatalf("insert global changed=%t err=%v", changed, err)
		}
		var firstUpdatedAt time.Time
		if err := db.QueryRowContext(ctx, `SELECT updated_at FROM org_feature_flags WHERE org_id IS NULL AND flag = 'attachments'`).Scan(&firstUpdatedAt); err != nil {
			t.Fatal(err)
		}
		changed, err = st.SetFeatureFlag(ctx, nil, "attachments", true, "second")
		if err != nil || changed {
			t.Fatalf("idempotent global changed=%t err=%v", changed, err)
		}
		var secondUpdatedAt time.Time
		var updatedBy string
		if err := db.QueryRowContext(ctx, `SELECT updated_at, updated_by FROM org_feature_flags WHERE org_id IS NULL AND flag = 'attachments'`).Scan(&secondUpdatedAt, &updatedBy); err != nil {
			t.Fatal(err)
		}
		if !secondUpdatedAt.Equal(firstUpdatedAt) || updatedBy != "first" {
			t.Fatalf("idempotent set mutated row: updated_at %s -> %s, updated_by=%q", firstUpdatedAt, secondUpdatedAt, updatedBy)
		}

		changed, err = st.SetFeatureFlag(ctx, &orgID, "attachments", false, "org-writer")
		if err != nil || !changed {
			t.Fatalf("insert org changed=%t err=%v", changed, err)
		}
		values, err := st.LookupFeatureFlag(ctx, orgID, "attachments")
		if err != nil {
			t.Fatal(err)
		}
		if values.Org == nil || *values.Org || values.Global == nil || !*values.Global {
			t.Fatalf("unexpected explicit values: org=%v global=%v", values.Org, values.Global)
		}
		flags, err := st.ListFeatureFlags(ctx, &orgID)
		if err != nil || len(flags) != 1 || flags[0].Enabled || flags[0].UpdatedBy != "org-writer" {
			t.Fatalf("org flags=%#v err=%v", flags, err)
		}
	})
}

func TestSetFeatureFlagAuditedIsAtomicAndAuditsIdempotentCalls(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'feature-audit')`, orgID); err != nil {
			t.Fatal(err)
		}

		for call := 0; call < 2; call++ {
			changed, replayID, err := st.SetFeatureFlagAudited(ctx, &orgID, "attachments", true, "ci@example.com")
			if err != nil {
				t.Fatal(err)
			}
			if changed != (call == 0) {
				t.Fatalf("call %d changed=%t", call, changed)
			}
			if replayID == "" {
				t.Fatalf("call %d returned empty replay id", call)
			}
		}

		var flagRows, auditRows, nullToolCallRows int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM org_feature_flags WHERE org_id = $1 AND flag = 'attachments'`, orgID).Scan(&flagRows); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRowContext(ctx, `
			SELECT count(*), count(*) FILTER (WHERE tool_call_id IS NULL)
			FROM audit_log WHERE actor = 'ci@example.com'
		`).Scan(&auditRows, &nullToolCallRows); err != nil {
			t.Fatal(err)
		}
		if flagRows != 1 || auditRows != 2 || nullToolCallRows != 2 {
			t.Fatalf("flag rows=%d audit rows=%d null-tool-call rows=%d", flagRows, auditRows, nullToolCallRows)
		}

		if _, _, err := st.SetFeatureFlagAudited(ctx, &orgID, "rollback-check", true, ""); err == nil {
			t.Fatal("empty actor unexpectedly succeeded")
		}
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM org_feature_flags WHERE org_id = $1 AND flag = 'rollback-check'`, orgID).Scan(&flagRows); err != nil {
			t.Fatal(err)
		}
		if flagRows != 0 {
			t.Fatalf("failed audited write left %d flag rows", flagRows)
		}
	})
}

func TestFeatureFlagRLSExposesGlobalAndCurrentOrgOnly(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		orgA := uuid.NewString()
		orgB := uuid.NewString()
		for _, org := range []struct{ id, name string }{{orgA, "flags-a"}, {orgB, "flags-b"}} {
			if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, $2)`, org.id, org.name); err != nil {
				t.Fatal(err)
			}
		}
		for _, row := range []struct {
			orgID   any
			enabled bool
		}{{nil, false}, {orgA, true}, {orgB, false}} {
			if _, err := db.ExecContext(ctx, `
				INSERT INTO org_feature_flags (org_id, flag, enabled, updated_by)
				VALUES ($1, 'attachments', $2, 'seed')
			`, row.orgID, row.enabled); err != nil {
				t.Fatal(err)
			}
		}

		roleName := "feature_flag_rls_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD 'rls_feature_flag'`, roleName)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE, DELETE ON org_feature_flags TO %s`, roleName)); err != nil {
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
		appDSN, err = dsnWithCredentials(appDSN, roleName, "rls_feature_flag")
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

		err = st.RunAsOrg(ctx, orgA, func(scoped *Store) error {
			var visible int
			if err := scoped.q.QueryRowContext(ctx, `SELECT count(*) FROM org_feature_flags`).Scan(&visible); err != nil {
				return err
			}
			if visible != 2 {
				t.Fatalf("tenant A sees %d flag rows, want global + own", visible)
			}
			values, err := scoped.LookupFeatureFlag(ctx, orgA, "attachments")
			if err != nil {
				return err
			}
			if values.Org == nil || !*values.Org || values.Global == nil || *values.Global {
				t.Fatalf("tenant values org=%v global=%v", values.Org, values.Global)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}

		err = st.RunAsOrg(ctx, orgA, func(scoped *Store) error {
			_, innerErr := scoped.SetFeatureFlag(ctx, nil, "global-write", true, "tenant")
			return innerErr
		})
		if err == nil {
			t.Fatal("tenant unexpectedly wrote a global flag")
		}
		err = st.RunAsOrg(ctx, orgA, func(scoped *Store) error {
			_, innerErr := scoped.SetFeatureFlag(ctx, &orgB, "cross-org-write", true, "tenant")
			return innerErr
		})
		if err == nil {
			t.Fatal("tenant unexpectedly wrote another org flag")
		}
	})
}
