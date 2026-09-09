package store

import (
	"bytes"
	"github.com/pressly/goose/v3"
	"io/fs"
	"os"
	"testing"
)

func TestEmbeddedMigrationInventory(t *testing.T) {
	for _, scope := range []string{"core", "cloud"} {
		names, err := fs.Glob(embeddedMigrations, "migrations/"+scope+"/*.sql")
		if err != nil || len(names) == 0 {
			t.Fatalf("%s: %v", scope, err)
		}
		disk, err := fs.Glob(os.DirFS("."), "migrations/"+scope+"/*.sql")
		if err != nil || len(disk) != len(names) {
			t.Fatal("embedded migration inventory drift")
		}
		for _, name := range names {
			a, _ := embeddedMigrations.ReadFile(name)
			b, err := os.ReadFile(name)
			if err != nil || !bytes.Equal(a, b) {
				t.Fatalf("migration differs: %s", name)
			}
		}
	}
}

func TestMigrationOverrideDoesNotFallBack(t *testing.T) {
	root := t.TempDir()
	t.Setenv("NERVE_MIGRATIONS_DIR", root)
	err := withGoose(migrationTableCore, func() error { _, err := goose.CollectMigrations(migrationDir("core"), 0, goose.MaxVersion); return err })
	if err == nil {
		t.Fatal("missing explicit migration directory silently fell back")
	}
	if err := os.Mkdir(root+"/core", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root+"/core/0001_override.sql", []byte("-- +goose Up\nSELECT 1;\n-- +goose Down\nSELECT 1;\n"), 0600); err != nil {
		t.Fatal(err)
	}
	err = withGoose(migrationTableCore, func() error {
		migrations, err := goose.CollectMigrations(migrationDir("core"), 0, goose.MaxVersion)
		if err == nil && len(migrations) != 1 {
			t.Fatal("override mixed with embedded migrations")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
