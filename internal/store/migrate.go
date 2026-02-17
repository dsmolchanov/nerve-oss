package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/pressly/goose/v3"
)

const (
	migrationTableCore  = "schema_migrations_core"
	migrationTableCloud = "schema_migrations_cloud"
)

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

func migrateScope(ctx context.Context, db *sql.DB, tableName string, dir string) error {
	goose.SetDialect("postgres")
	goose.SetTableName(tableName)
	return goose.UpContext(ctx, db, dir)
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
