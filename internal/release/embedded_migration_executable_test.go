package release

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEmbeddedMigrationExecutablesEnforceOwner(t *testing.T) {
	root := repositoryRoot(t)
	dir := t.TempDir()
	config := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(config, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	environment := []string{}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "NERVE_") && !strings.HasPrefix(entry, "NM_") {
			environment = append(environment, entry)
		}
	}
	environment = append(environment, "NM_CONFIG="+config, "NM_DB_DSN=not a valid database DSN", "NM_CLOUD_MODE=true", "NERVE_SCHEMA_TRANSITION_MODE=", "NM_MIGRATE_ON_START=off")
	for _, name := range []string{"neuralmail", "neuralmaild"} {
		for _, window := range []string{"2:29:30", "invalid", ""} {
			binary := filepath.Join(dir, name)
			build := exec.Command("go", "build", "-ldflags", "-X neuralmail/internal/startup.CompiledSchemaWindow="+window, "-o", binary, "./cmd/"+name)
			build.Dir = root
			if out, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build: %v %s", err, out)
			}
			for _, command := range []string{"migrate-core", "migrate-cloud", "migrate-all"} {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				run := exec.CommandContext(ctx, binary, command)
				run.Env = environment
				out, err := run.CombinedOutput()
				timedOut := ctx.Err() != nil
				cancel()
				if err == nil || timedOut {
					t.Fatalf("%s %s window=%q: err=%v output=%s", name, command, window, err, out)
				}
				expected := "successor migrations require nerve-migrate"
				if window == "invalid" {
					expected = "invalid compiled runtime schema window"
				}
				if window == "" {
					if !bytes.Contains(out, []byte("not a valid database DSN")) {
						t.Fatalf("legacy negative control did not reach configured invalid DB: %s", out)
					}
					if bytes.Contains(out, []byte("successor migrations")) || bytes.Contains(out, []byte("compiled runtime schema")) {
						t.Fatalf("legacy migration incorrectly refused: %s", out)
					}
				} else if !bytes.Contains(out, []byte(expected)) {
					t.Fatalf("%s %s window=%q did not refuse before DB access: %s", name, command, window, out)
				}
			}
		}
	}
}
