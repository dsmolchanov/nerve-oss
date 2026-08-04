package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/pressly/goose/v3"
)

const (
	migrationTableCore  = "schema_migrations_core"
	migrationTableCloud = "schema_migrations_cloud"
)

var gooseMu sync.Mutex

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
	return currentVersion(ctx, db, migrationTableCore, migrationDir("core"))
}

// CurrentVersionCloud reports the highest applied cloud migration version.
func CurrentVersionCloud(ctx context.Context, db *sql.DB) (int64, error) {
	return currentVersion(ctx, db, migrationTableCloud, migrationDir("cloud"))
}

func migrateScope(ctx context.Context, db *sql.DB, tableName string, dir string) error {
	return withGoose(tableName, func() error {
		return goose.UpContext(ctx, db, dir)
	})
}

func migrateScopeTo(ctx context.Context, db *sql.DB, tableName string, dir string, version int64) error {
	return withGoose(tableName, func() error {
		if err := goose.UpToContext(ctx, db, dir, version); err != nil {
			return err
		}

		current, err := goose.GetDBVersionContext(ctx, db)
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

func currentVersion(ctx context.Context, db *sql.DB, tableName string, dir string) (int64, error) {
	var version int64
	err := withGoose(tableName, func() error {
		var err error
		version, err = goose.GetDBVersionContext(ctx, db)
		return err
	})
	return version, err
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
