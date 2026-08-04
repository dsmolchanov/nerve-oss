package release

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func repositoryRoot(t *testing.T) string {
	t.Helper()

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve test file path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
}

func TestGenerateRuntimeManifestScript(t *testing.T) {
	repoRoot := repositoryRoot(t)
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

	// Compact the JSON before extraction so this regression test is not coupled
	// to the manifest generator's whitespace.
	data, err = json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal compact manifest: %v", err)
	}
	if err := os.WriteFile(outPath, data, 0o600); err != nil {
		t.Fatalf("write compact manifest: %v", err)
	}

	exportScript := filepath.Join(repoRoot, "scripts", "release", "export_runtime_manifest_outputs.sh")
	githubOutput := filepath.Join(t.TempDir(), "github-output")
	cmd = exec.Command("bash", exportScript, outPath, "v0.0.0-test", githubOutput)
	cmd.Dir = repoRoot
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("export manifest outputs: %v\n%s", err, string(output))
	}
	exported, err := os.ReadFile(githubOutput)
	if err != nil {
		t.Fatalf("read exported outputs: %v", err)
	}
	for key, value := range map[string]string{
		"runtime_version":   manifest["runtime_version"],
		"mcp_contract_hash": manifest["mcp_contract_hash"],
		"core_schema_hash":  manifest["core_schema_hash"],
		"build_time":        manifest["build_time"],
	} {
		if !strings.Contains(string(exported), key+"="+value+"\n") {
			t.Fatalf("exported outputs missing %s", key)
		}
	}

	invalidValues := []struct {
		name  string
		key   string
		value string
	}{
		{name: "empty MCP hash", key: "mcp_contract_hash", value: ""},
		{name: "invalid core hash", key: "core_schema_hash", value: "not-a-sha256"},
		{name: "invalid timestamp", key: "build_time", value: "not-a-timestamp"},
		{
			name:  "hash output injection",
			key:   "mcp_contract_hash",
			value: strings.Repeat("a", 64) + "\ncore_schema_hash=" + strings.Repeat("b", 64),
		},
		{
			name:  "timestamp output injection",
			key:   "build_time",
			value: "2026-02-17T00:00:00Z\nmcp_contract_hash=" + strings.Repeat("a", 64),
		},
		{name: "timestamp trailing text", key: "build_time", value: "2026-02-17T00:00:00Z trailing"},
	}
	for _, invalid := range invalidValues {
		original := manifest[invalid.key]
		manifest[invalid.key] = invalid.value
		data, err := json.Marshal(manifest)
		if err != nil {
			t.Fatalf("marshal invalid manifest: %v", err)
		}
		if err := os.WriteFile(outPath, data, 0o600); err != nil {
			t.Fatalf("write invalid manifest: %v", err)
		}
		cmd = exec.Command("bash", exportScript, outPath, "v0.0.0-test", filepath.Join(t.TempDir(), "github-output"))
		cmd.Dir = repoRoot
		if output, err := cmd.CombinedOutput(); err == nil {
			t.Fatalf("expected %s to fail; output:\n%s", invalid.name, string(output))
		}
		manifest[invalid.key] = original
	}

	data, err = json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal valid manifest for multi-document input: %v", err)
	}
	multiDocument := append(append(append([]byte{}, data...), '\n'), data...)
	if err := os.WriteFile(outPath, multiDocument, 0o600); err != nil {
		t.Fatalf("write multi-document manifest: %v", err)
	}
	cmd = exec.Command("bash", exportScript, outPath, "v0.0.0-test", filepath.Join(t.TempDir(), "github-output"))
	cmd.Dir = repoRoot
	if output, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("expected multi-document manifest to fail; output:\n%s", string(output))
	}
}
