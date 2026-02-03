package store

import (
	"context"
	"database/sql"
	"path/filepath"

	"github.com/pressly/goose/v3"
)

func Migrate(ctx context.Context, db *sql.DB) error {
	goose.SetDialect("postgres")
	goose.SetTableName("schema_migrations")
	return goose.UpContext(ctx, db, filepath.Join("internal", "store", "migrations"))
}
