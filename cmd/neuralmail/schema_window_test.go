package main

import (
	"context"
	"database/sql"
	"neuralmail/internal/startup"
	"testing"
)

func TestSuccessorEmbeddedMigrationsRefuseBeforeDatabase(t *testing.T) {
	old := startup.CompiledSchemaWindow
	t.Cleanup(func() { startup.CompiledSchemaWindow = old })
	for _, raw := range []string{"2:29:30", "broken"} {
		startup.CompiledSchemaWindow = raw
		for _, migrate := range []func(context.Context, *sql.DB) error{migrateCoreToRuntimeWindow, migrateCloudToRuntimeWindow, migrateAllToRuntimeWindow} {
			if err := migrate(context.Background(), nil); err == nil {
				t.Fatal("embedded migration accepted")
			}
		}
	}
}
