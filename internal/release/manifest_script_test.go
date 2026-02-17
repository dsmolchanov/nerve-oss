package release

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestGenerateRuntimeManifestScript(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve test file path")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
	scriptPath := filepath.Join(repoRoot, "scripts", "release", "generate_runtime_manifest.sh")
	outPath := filepath.Join(t.TempDir(), "runtime-manifest.json")

	cmd := exec.Command("bash", scriptPath, "v0.0.0-test", outPath)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "GIT_COMMIT=testcommit", "BUILD_TIME=2026-02-17T00:00:00Z")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, string(output))
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}

	var manifest map[string]string
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse manifest json: %v", err)
	}

	for _, key := range []string{"runtime_version", "mcp_contract_hash", "core_schema_hash", "build_commit", "build_time"} {
		if manifest[key] == "" {
			t.Fatalf("manifest missing %s", key)
		}
	}
	if manifest["runtime_version"] != "v0.0.0-test" {
		t.Fatalf("unexpected runtime version: %s", manifest["runtime_version"])
	}
}
