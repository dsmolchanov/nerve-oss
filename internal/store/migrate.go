package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/pressly/goose/v3"
)

const (
	migrationTableCore  = "schema_migrations_core"
	migrationTableCloud = "schema_migrations_cloud"
)

var gooseMu sync.Mutex

// MigrationStatus describes the current and available migration versions for
// one scope. Pending contains the actual unapplied migration versions in
// ascending order; it is non-nil even when the scope is at head.
type MigrationStatus struct {
	Current int64
	Head    int64
	Pending []int64
}

// StartupMigrationMode controls whether a long-running process may change the
// database schema while it starts.
type StartupMigrationMode string

const (
	StartupMigrationVerify     StartupMigrationMode = "verify"
	StartupMigrationApplyToMax StartupMigrationMode = "apply-to-max"
	StartupMigrationOff        StartupMigrationMode = "off"
)

// MigrationWindow is the schema compatibility contract compiled into a
// binary. Core and cloud have independent Goose version histories.
type MigrationWindow struct {
	CoreMinRequired   int64
	CoreMaxSupported  int64
	IncludeCloud      bool
	CloudMinRequired  int64
	CloudMaxSupported int64
}

// ParseStartupMigrationMode validates an NM_MIGRATE_ON_START value. An empty
// value is safe-by-default in cloud mode and convenient for local OSS use.
func ParseStartupMigrationMode(value string, cloudMode bool) (StartupMigrationMode, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		if cloudMode {
			return StartupMigrationVerify, nil
		}
		return StartupMigrationApplyToMax, nil
	}

	mode := StartupMigrationMode(value)
	switch mode {
	case StartupMigrationVerify, StartupMigrationApplyToMax, StartupMigrationOff:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid NM_MIGRATE_ON_START %q: want verify, apply-to-max, or off", value)
	}
}

// MigrateOnStart enforces the migration policy for a long-running process.
// Verify is strictly read-only. Apply-to-max never advances beyond the
// binary's declared compatibility window.
func MigrateOnStart(ctx context.Context, db *sql.DB, mode StartupMigrationMode, window MigrationWindow) error {
	if err := validateMigrationWindow(window); err != nil {
		return err
	}

	switch mode {
	case StartupMigrationVerify:
		if err := verifyMigrationVersion(ctx, db, "core", window.CoreMinRequired, window.CoreMaxSupported, CurrentVersionCore); err != nil {
			return err
		}
		if window.IncludeCloud {
			return verifyMigrationVersion(ctx, db, "cloud", window.CloudMinRequired, window.CloudMaxSupported, CurrentVersionCloud)
		}
		return nil
	case StartupMigrationApplyToMax:
		if err := MigrateUpToCore(ctx, db, window.CoreMaxSupported); err != nil {
			return fmt.Errorf("apply core startup migrations through %d: %w", window.CoreMaxSupported, err)
		}
		if window.IncludeCloud {
			if err := MigrateUpToCloud(ctx, db, window.CloudMaxSupported); err != nil {
				return fmt.Errorf("apply cloud startup migrations through %d: %w", window.CloudMaxSupported, err)
			}
		}
		return nil
	case StartupMigrationOff:
		return nil
	default:
		return fmt.Errorf("invalid startup migration mode %q", mode)
	}
}

func validateMigrationWindow(window MigrationWindow) error {
	type scopeWindow struct {
		name string
		min  int64
		max  int64
	}
	scopes := []scopeWindow{
		{name: "core", min: window.CoreMinRequired, max: window.CoreMaxSupported},
	}
	if window.IncludeCloud {
		scopes = append(scopes, scopeWindow{name: "cloud", min: window.CloudMinRequired, max: window.CloudMaxSupported})
	}
	for _, scope := range scopes {
		if scope.min < 0 || scope.max < 0 {
			return fmt.Errorf("invalid %s migration window [%d,%d]: versions must be non-negative", scope.name, scope.min, scope.max)
		}
		if scope.min > scope.max {
			return fmt.Errorf("invalid %s migration window [%d,%d]: minimum exceeds maximum", scope.name, scope.min, scope.max)
		}
	}
	return nil
}

func verifyMigrationVersion(
	ctx context.Context,
	db *sql.DB,
	scope string,
	minRequired int64,
	maxSupported int64,
	currentVersion func(context.Context, *sql.DB) (int64, error),
) error {
	current, err := currentVersion(ctx, db)
	if err != nil {
		return fmt.Errorf("verify %s migration version: %w", scope, err)
	}
	if current < minRequired || current > maxSupported {
		return fmt.Errorf(
			"%s schema version %d is outside binary compatibility window [%d,%d]",
			scope,
			current,
			minRequired,
			maxSupported,
		)
	}
	return nil
}

// Migrate is a compatibility wrapper used by existing entrypoints.
// It applies core migrations first, then cloud migrations.
func Migrate(ctx context.Context, db *sql.DB) error {
	return MigrateAll(ctx, db)
}

func MigrateAll(ctx context.Context, db *sql.DB) error {
	if err := MigrateCore(ctx, db); err != nil {
		return err
	}
	return MigrateCloud(ctx, db)
}

func MigrateCore(ctx context.Context, db *sql.DB) error {
	return migrateScope(ctx, db, migrationTableCore, migrationDir("core"))
}

func MigrateCloud(ctx context.Context, db *sql.DB) error {
	return migrateScope(ctx, db, migrationTableCloud, migrationDir("cloud"))
}

// MigrateUpToCore applies core migrations up to and including version, leaving
// anything later pending. Required by the expand -> dual-read -> relax rollout:
// a plain Up would apply the relax step in the same run as the expand step,
// which is what the staged rollout exists to prevent.
func MigrateUpToCore(ctx context.Context, db *sql.DB, version int64) error {
	return migrateScopeTo(ctx, db, migrationTableCore, migrationDir("core"), version)
}

// MigrateUpToCloud is MigrateUpToCore for the cloud scope.
func MigrateUpToCloud(ctx context.Context, db *sql.DB, version int64) error {
	return migrateScopeTo(ctx, db, migrationTableCloud, migrationDir("cloud"), version)
}

// MigrateDownCore rolls back the most recent core migration. Destructive by
// definition: callers are expected to have checked that rolling back is safe
// for the data present.
func MigrateDownCore(ctx context.Context, db *sql.DB) error {
	return migrateScopeDown(ctx, db, migrationTableCore, migrationDir("core"))
}

// MigrateDownCloud is MigrateDownCore for the cloud scope.
func MigrateDownCloud(ctx context.Context, db *sql.DB) error {
	return migrateScopeDown(ctx, db, migrationTableCloud, migrationDir("cloud"))
}

// CurrentVersionCore reports the highest applied core migration version.
func CurrentVersionCore(ctx context.Context, db *sql.DB) (int64, error) {
	return currentVersion(ctx, db, migrationTableCore)
}

// CurrentVersionCloud reports the highest applied cloud migration version.
func CurrentVersionCloud(ctx context.Context, db *sql.DB) (int64, error) {
	return currentVersion(ctx, db, migrationTableCloud)
}

// MigrationStatusCore reports the read-only migration status for the core
// scope without creating or changing its migration table.
func MigrationStatusCore(ctx context.Context, db *sql.DB) (MigrationStatus, error) {
	return migrationStatus(ctx, db, migrationTableCore, migrationDir("core"))
}

// MigrationStatusCloud is MigrationStatusCore for the cloud scope.
func MigrationStatusCloud(ctx context.Context, db *sql.DB) (MigrationStatus, error) {
	return migrationStatus(ctx, db, migrationTableCloud, migrationDir("cloud"))
}

func migrateScope(ctx context.Context, db *sql.DB, tableName string, dir string) error {
	return withGoose(tableName, func() error {
		return goose.UpContext(ctx, db, dir)
	})
}

func migrateScopeTo(ctx context.Context, db *sql.DB, tableName string, dir string, version int64) error {
	return withGoose(tableName, func() error {
		if version < 0 {
			return fmt.Errorf("migration target must be non-negative: %d", version)
		}

		targetAvailable := version == 0
		if !targetAvailable {
			migrations, err := goose.CollectMigrations(dir, 0, goose.MaxVersion)
			if err != nil {
				return fmt.Errorf("collect migrations for target %d: %w", version, err)
			}
			_, err = migrations.Current(version)
			targetAvailable = err == nil
		}

		current, err := currentVersion(ctx, db, tableName)
		if err != nil {
			return fmt.Errorf("read current migration version: %w", err)
		}
		if current == version {
			return nil
		}
		if !targetAvailable {
			return fmt.Errorf("migration target %d is not available", version)
		}
		if current > version {
			return fmt.Errorf("migration target %d already passed: current version is %d", version, current)
		}

		if err := goose.UpToContext(ctx, db, dir, version); err != nil {
			return err
		}

		current, err = goose.GetDBVersionContext(ctx, db)
		if err != nil {
			return fmt.Errorf("verify migration target %d: %w", version, err)
		}
		if current != version {
			return fmt.Errorf("migration target %d not reached: current version is %d", version, current)
		}
		return nil
	})
}

func migrateScopeDown(ctx context.Context, db *sql.DB, tableName string, dir string) error {
	return withGoose(tableName, func() error {
		return goose.DownContext(ctx, db, dir)
	})
}

func currentVersion(ctx context.Context, db *sql.DB, tableName string) (int64, error) {
	exists, err := migrationTableExists(ctx, db, tableName)
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}

	var query string
	switch tableName {
	case migrationTableCore:
		query = `SELECT version_id, is_applied FROM schema_migrations_core ORDER BY id DESC`
	case migrationTableCloud:
		query = `SELECT version_id, is_applied FROM schema_migrations_cloud ORDER BY id DESC`
	default:
		return 0, fmt.Errorf("unknown migration table %q", tableName)
	}

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	seen := make(map[int64]struct{})
	for rows.Next() {
		var version int64
		var applied bool
		if err := rows.Scan(&version, &applied); err != nil {
			return 0, err
		}
		if _, ok := seen[version]; ok {
			continue
		}
		seen[version] = struct{}{}
		if applied {
			return version, nil
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return 0, nil
}

func migrationStatus(ctx context.Context, db *sql.DB, tableName string, dir string) (MigrationStatus, error) {
	status := MigrationStatus{Pending: make([]int64, 0)}
	err := withGoose(tableName, func() error {
		migrations, err := goose.CollectMigrations(dir, 0, goose.MaxVersion)
		if err != nil {
			return fmt.Errorf("collect migrations: %w", err)
		}
		if len(migrations) > 0 {
			status.Head = migrations[len(migrations)-1].Version
			status.Pending = make([]int64, 0, len(migrations))
		}

		status.Current, err = currentVersion(ctx, db, tableName)
		if err != nil {
			return fmt.Errorf("read current migration version: %w", err)
		}
		if status.Current > status.Head {
			return fmt.Errorf("current migration version %d is ahead of migration head %d", status.Current, status.Head)
		}

		currentAvailable := status.Current == 0
		for _, migration := range migrations {
			if migration.Version == status.Current {
				currentAvailable = true
			}
			if migration.Version > status.Current {
				status.Pending = append(status.Pending, migration.Version)
			}
		}
		if !currentAvailable {
			return fmt.Errorf("current migration version %d is not available", status.Current)
		}
		return nil
	})
	return status, err
}

func migrationTableExists(ctx context.Context, db *sql.DB, tableName string) (bool, error) {
	var exists bool
	err := db.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, tableName).Scan(&exists)
	return exists, err
}

// withGoose serializes configuration and execution of goose's legacy API.
// Goose reads its dialect and table name from package globals throughout an
// operation, so another scope must not replace them until the operation ends.
func withGoose(tableName string, operation func() error) error {
	gooseMu.Lock()
	defer gooseMu.Unlock()

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("configure goose dialect: %w", err)
	}
	goose.SetTableName(tableName)
	return operation()
}

func migrationDir(scope string) string {
	// Prefer resolving from current working directory.
	if local := filepath.Join("internal", "store", "migrations", scope); dirExists(local) {
		return local
	}

	// Fallback to a path relative to this source file for tests run from package subdirs.
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		panic(fmt.Sprintf("resolve migration directory for %s: missing caller info", scope))
	}
	sourceRelative := filepath.Join(filepath.Dir(currentFile), "migrations", scope)
	if dirExists(sourceRelative) {
		return sourceRelative
	}

	panic(fmt.Sprintf("resolve migration directory for %s: directory not found", scope))
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
