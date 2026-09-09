package release

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeSchemaWindowBuildContract(t *testing.T) {
	root := repositoryRoot(t)
	catalog := t.TempDir()
	// Synthetic catalog tests derivation; this does not allocate a real migration.
	for _, name := range []string{"0029_prior.sql", "0030_fixture.sql"} {
		if err := os.WriteFile(filepath.Join(catalog, name), []byte("-- fixture\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ version, role, min, max, want string }{
		{"1", "", "", "", ""}, {"2", "B", "29", "30", "2:29:30"}, {"2", "C", "", "30", "2:30:30"},
		{"3", "B", "29", "30", "error"}, {"2", "A", "29", "30", "error"},
		{"2", "B", "", "30", "error"}, {"2", "B", "28", "30", "error"},
		{"2", "B", "31", "30", "error"}, {"2", "B", "029", "30", "error"},
		{"2", "B", "29", "31", "error"},
	} {
		env := isolatedCommandEnvironment("RUNTIME_MANIFEST_VERSION="+tc.version, "ARTIFACT_ROLE="+tc.role, "CORE_SCHEMA_MIN_REQUIRED="+tc.min, "CORE_SCHEMA_MAX_SUPPORTED="+tc.max, "CORE_MIGRATIONS_PATH="+catalog)
		helper := exec.Command("bash", "scripts/release/runtime_schema_window.sh")
		helper.Dir = root
		helper.Env = env
		out, err := helper.CombinedOutput()
		if tc.want == "error" {
			if err == nil {
				t.Fatalf("accepted %+v", tc)
			}
			continue
		}
		if err != nil || strings.TrimSpace(string(out)) != tc.want {
			t.Fatalf("%+v: %s %v", tc, out, err)
		}
		manifestPath := filepath.Join(t.TempDir(), "manifest.json")
		generate := exec.Command("bash", "scripts/release/generate_runtime_manifest.sh", "v0.0.0-test", manifestPath)
		generate.Dir = root
		generate.Env = env
		if out, err := generate.CombinedOutput(); err != nil {
			t.Fatalf("manifest: %s %v", out, err)
		}
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		var manifest map[string]string
		if err := json.Unmarshal(data, &manifest); err != nil {
			t.Fatal(err)
		}
		min, max := "29", "29"
		if tc.want != "" {
			parts := strings.Split(tc.want, ":")
			min, max = parts[1], parts[2]
		}
		if manifest["core_schema_min_required"] != min || manifest["core_schema_max_supported"] != max {
			t.Fatalf("manifest disagrees with linker input: %+v", manifest)
		}
	}
}
