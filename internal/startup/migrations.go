// Package startup owns process-start compatibility checks that must run before
// background workers or HTTP listeners become active.
package startup

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"neuralmail/internal/store"
)

const (
	CoreMinRequired          int64 = 19
	CoreMaxSupported         int64 = 19
	RuntimeCloudMinRequired  int64 = 3
	RuntimeCloudMaxSupported int64 = 3
)

var runtimeMigrationWindow = store.MigrationWindow{
	CoreMinRequired:  CoreMinRequired,
	CoreMaxSupported: CoreMaxSupported,
}

// Migrate applies the NM_MIGRATE_ON_START policy. Cloud processes default to
// read-only verification; local OSS processes default to a bounded migration.
func Migrate(ctx context.Context, db *sql.DB, cloudMode bool) error {
	mode, err := store.ParseStartupMigrationMode(os.Getenv("NM_MIGRATE_ON_START"), cloudMode)
	if err != nil {
		return err
	}
	window := runtimeMigrationWindow
	if !cloudMode {
		window.IncludeCloud = true
		window.CloudMinRequired = RuntimeCloudMinRequired
		window.CloudMaxSupported = RuntimeCloudMaxSupported
	}
	if err := store.MigrateOnStart(ctx, db, mode, window); err != nil {
		return fmt.Errorf("startup migration policy %s: %w", mode, err)
	}
	return nil
}
