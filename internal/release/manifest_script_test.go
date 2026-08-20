package release

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"neuralmail/internal/startup"
)

func repositoryRoot(t *testing.T) string {
	t.Helper()

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve test file path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
}

func isolatedCommandEnvironment(overrides ...string) []string {
	overridden := make(map[string]struct{}, len(overrides))
	for _, override := range overrides {
		key, _, _ := strings.Cut(override, "=")
		overridden[key] = struct{}{}
	}
	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, skip := overridden[key]; !skip {
			environment = append(environment, entry)
		}
	}
	return append(environment, overrides...)
}

func TestGenerateRuntimeManifestScript(t *testing.T) {
	repoRoot := repositoryRoot(t)
	scriptPath := filepath.Join(repoRoot, "scripts", "release", "generate_runtime_manifest.sh")
	outPath := filepath.Join(t.TempDir(), "runtime-manifest.json")

	cmd := exec.Command("bash", scriptPath, "v0.0.0-test", outPath)
	cmd.Dir = repoRoot
	cmd.Env = isolatedCommandEnvironment(
		"GIT_COMMIT=testcommit",
		"BUILD_TIME=2026-02-17T00:00:00Z",
		"MCP_CONTRACT_PATH="+filepath.Join(repoRoot, "docs", "MCP_Contract.md"),
		"CORE_MIGRATIONS_PATH="+filepath.Join(repoRoot, "internal", "store", "migrations", "core"),
		"OUTBOUND_POLICY_PATH="+filepath.Join(repoRoot, "configs", "policy", "autonomous-outbound-v1.yaml"),
	)
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

	for _, key := range []string{"runtime_version", "mcp_contract_hash", "core_schema_hash", "core_schema_min_required", "core_schema_max_supported", "outbound_policy_version", "outbound_policy_sha256", "build_commit", "build_time"} {
		if manifest[key] == "" {
			t.Fatalf("manifest missing %s", key)
		}
	}
	if manifest["runtime_version"] != "v0.0.0-test" {
		t.Fatalf("unexpected runtime version: %s", manifest["runtime_version"])
	}
	if manifest["core_schema_min_required"] != strconv.FormatInt(startup.CoreMinRequired, 10) {
		t.Fatalf("manifest core minimum %q differs from compiled value %d", manifest["core_schema_min_required"], startup.CoreMinRequired)
	}
	if manifest["core_schema_max_supported"] != strconv.FormatInt(startup.CoreMaxSupported, 10) {
		t.Fatalf("manifest core maximum %q differs from compiled value %d", manifest["core_schema_max_supported"], startup.CoreMaxSupported)
	}

	exactMirrorSources := map[string]string{
		"mcp_contract_hash":      "docs/MCP_Contract.md",
		"outbound_policy_sha256": "configs/policy/autonomous-outbound-v1.yaml",
	}
	syncManifestData, err := os.ReadFile(filepath.Join(repoRoot, "sync-manifest.yaml"))
	if err != nil {
		t.Fatalf("read sync manifest: %v", err)
	}
	var syncManifest struct {
		ExactMirror []string `json:"exact-mirror"`
	}
	if err := json.Unmarshal(syncManifestData, &syncManifest); err != nil {
		t.Fatalf("parse sync manifest: %v", err)
	}
	for field, relativePath := range exactMirrorSources {
		sourcePath := filepath.Join(repoRoot, filepath.FromSlash(relativePath))
		source, err := os.ReadFile(sourcePath)
		if err != nil {
			t.Fatalf("read %s source: %v", field, err)
		}
		expectedHash := fmt.Sprintf("%x", sha256.Sum256(source))
		if manifest[field] != expectedHash {
			t.Fatalf("manifest %s=%q differs from exact-mirror source hash %q", field, manifest[field], expectedHash)
		}
		found := false
		for _, mirroredPath := range syncManifest.ExactMirror {
			if mirroredPath == relativePath {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("manifest %s source %q is not exact-mirrored", field, relativePath)
		}
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
		"runtime_version":           manifest["runtime_version"],
		"mcp_contract_hash":         manifest["mcp_contract_hash"],
		"core_schema_hash":          manifest["core_schema_hash"],
		"core_schema_min_required":  manifest["core_schema_min_required"],
		"core_schema_max_supported": manifest["core_schema_max_supported"],
		"outbound_policy_version":   manifest["outbound_policy_version"],
		"outbound_policy_sha256":    manifest["outbound_policy_sha256"],
		"build_time":                manifest["build_time"],
	} {
		if !strings.Contains(string(exported), key+"="+value+"\n") {
			t.Fatalf("exported outputs missing %s", key)
		}
	}

	invalidValues := []struct {
		name            string
		key             string
		value           string
		expectedVersion string
		remove          bool
	}{
		{name: "missing runtime version", key: "runtime_version", remove: true},
		{name: "missing MCP hash", key: "mcp_contract_hash", remove: true},
		{name: "missing core hash", key: "core_schema_hash", remove: true},
		{name: "missing core minimum", key: "core_schema_min_required", remove: true},
		{name: "missing core maximum", key: "core_schema_max_supported", remove: true},
		{name: "missing outbound policy version", key: "outbound_policy_version", remove: true},
		{name: "missing outbound policy hash", key: "outbound_policy_sha256", remove: true},
		{name: "missing build time", key: "build_time", remove: true},
		{name: "unexpected runtime version", key: "runtime_version", value: "v0.0.1"},
		{name: "empty MCP hash", key: "mcp_contract_hash", value: ""},
		{name: "uppercase MCP hash", key: "mcp_contract_hash", value: strings.Repeat("A", 64)},
		{name: "invalid core hash", key: "core_schema_hash", value: "not-a-sha256"},
		{name: "uppercase core hash", key: "core_schema_hash", value: strings.Repeat("B", 64)},
		{name: "invalid core minimum", key: "core_schema_min_required", value: "-1"},
		{name: "inverted core window", key: "core_schema_min_required", value: strconv.FormatInt(startup.CoreMaxSupported+1, 10)},
		{name: "invalid core maximum", key: "core_schema_max_supported", value: "latest"},
		{name: "invalid outbound policy version", key: "outbound_policy_version", value: "Autonomous Outbound V1"},
		{name: "invalid outbound policy hash", key: "outbound_policy_sha256", value: "not-a-sha256"},
		{name: "invalid timestamp", key: "build_time", value: "not-a-timestamp"},
		{name: "noncanonical UTC timestamp", key: "build_time", value: "2026-02-17T00:00:00+00:00"},
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
		{
			name:            "C1 control",
			key:             "runtime_version",
			value:           "v0.0.0-test\u0085",
			expectedVersion: "v0.0.0-test\u0085",
		},
		{
			name:            "DEL control",
			key:             "runtime_version",
			value:           "v0.0.0-test\u007f",
			expectedVersion: "v0.0.0-test\u007f",
		},
	}
	for _, invalid := range invalidValues {
		original := manifest[invalid.key]
		if invalid.remove {
			delete(manifest, invalid.key)
		} else {
			manifest[invalid.key] = invalid.value
		}
		data, err := json.Marshal(manifest)
		if err != nil {
			t.Fatalf("marshal invalid manifest: %v", err)
		}
		if err := os.WriteFile(outPath, data, 0o600); err != nil {
			t.Fatalf("write invalid manifest: %v", err)
		}
		expectedVersion := "v0.0.0-test"
		if invalid.expectedVersion != "" {
			expectedVersion = invalid.expectedVersion
		}
		cmd = exec.Command("bash", exportScript, outPath, expectedVersion, filepath.Join(t.TempDir(), "github-output"))
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

	invalidDocuments := []struct {
		name string
		data []byte
	}{
		{name: "empty document", data: nil},
		{name: "non-object document", data: []byte(`[]`)},
		{name: "malformed document", data: []byte(`{"runtime_version"`)},
	}
	for _, invalid := range invalidDocuments {
		if err := os.WriteFile(outPath, invalid.data, 0o600); err != nil {
			t.Fatalf("write %s: %v", invalid.name, err)
		}
		cmd = exec.Command("bash", exportScript, outPath, "v0.0.0-test", filepath.Join(t.TempDir(), "github-output"))
		cmd.Dir = repoRoot
		if output, err := cmd.CombinedOutput(); err == nil {
			t.Fatalf("expected %s to fail; output:\n%s", invalid.name, string(output))
		}
	}
}

func TestGenerateRuntimeManifestScriptHonorsSourceOverrides(t *testing.T) {
	repoRoot := repositoryRoot(t)
	tempDir := t.TempDir()
	contractPath := filepath.Join(tempDir, "contract.md")
	policyPath := filepath.Join(tempDir, "policy.yaml")
	contract := []byte("alternate contract\n")
	policy := []byte("version: alternate-v1\n")
	if err := os.WriteFile(contractPath, contract, 0o600); err != nil {
		t.Fatalf("write contract override: %v", err)
	}
	if err := os.WriteFile(policyPath, policy, 0o600); err != nil {
		t.Fatalf("write policy override: %v", err)
	}

	outPath := filepath.Join(tempDir, "runtime-manifest.json")
	cmd := exec.Command("bash", filepath.Join(repoRoot, "scripts", "release", "generate_runtime_manifest.sh"), "override-test", outPath)
	cmd.Dir = repoRoot
	cmd.Env = isolatedCommandEnvironment(
		"GIT_COMMIT=overridecommit",
		"BUILD_TIME=2026-02-17T00:00:00Z",
		"MCP_CONTRACT_PATH="+contractPath,
		"CORE_MIGRATIONS_PATH="+filepath.Join(repoRoot, "internal", "store", "migrations", "core"),
		"OUTBOUND_POLICY_PATH="+policyPath,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script with source overrides failed: %v\n%s", err, string(output))
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read override manifest: %v", err)
	}
	var manifest map[string]string
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse override manifest: %v", err)
	}
	if got, want := manifest["mcp_contract_hash"], fmt.Sprintf("%x", sha256.Sum256(contract)); got != want {
		t.Fatalf("override contract hash=%q want=%q", got, want)
	}
	if got, want := manifest["outbound_policy_sha256"], fmt.Sprintf("%x", sha256.Sum256(policy)); got != want {
		t.Fatalf("override policy hash=%q want=%q", got, want)
	}
	if got := manifest["outbound_policy_version"]; got != "alternate-v1" {
		t.Fatalf("override policy version=%q want=alternate-v1", got)
	}
}
