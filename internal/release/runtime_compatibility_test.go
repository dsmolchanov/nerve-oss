package release

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRuntimeCompatibilityMetadataValidation(t *testing.T) {
	variables := []*string{&RuntimeVersion, &MCPContractHash, &CoreSchemaHash, &OutboundPolicyVersion, &OutboundPolicySHA256, &BuildCommit, &BuildTime}
	valid := []string{"v0.0.0-test", strings.Repeat("a", 64), strings.Repeat("b", 64), "policy-v1", strings.Repeat("c", 64), strings.Repeat("d", 40), "2026-09-09T00:00:00Z"}
	old := make([]string, len(variables))
	for i, v := range variables {
		old[i] = *v
		*v = valid[i]
	}
	t.Cleanup(func() {
		for i, v := range variables {
			*v = old[i]
		}
	})
	if _, err := compiledRuntimeManifest(); err != nil {
		t.Fatal(err)
	}
	for i, values := range [][]string{{"", "dev", "unknown", "bad\nversion"}, {"unknown", strings.Repeat("A", 64)}, {"bad", ""}, {"unknown", "bad policy"}, {"bad", ""}, {"short", strings.Repeat("D", 40)}, {"2026-02-30T00:00:00Z", "2026-09-09T00:00:00+00:00"}} {
		for _, value := range values {
			*variables[i] = value
			if _, err := compiledRuntimeManifest(); err == nil {
				t.Fatalf("invalid metadata field %d accepted", i)
			}
		}
		*variables[i] = valid[i]
	}
	for _, args := range [][]string{{"compatibility"}, {"compatibility", "--json", "extra"}, {"compatibility", "--text"}} {
		var out bytes.Buffer
		handled, err := HandleRuntimeCompatibility(args, &out)
		if !handled || err == nil || out.Len() != 0 {
			t.Fatalf("invalid flags accepted: %v", args)
		}
	}
	if handled, err := HandleRuntimeCompatibility([]string{"serve"}, nil); handled || err != nil {
		t.Fatal("ordinary startup intercepted")
	}
}

func TestRuntimeCompatibilityExecutableMatchesManifest(t *testing.T) {
	root := repositoryRoot(t)
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.json")
	generate := exec.Command("bash", "scripts/release/generate_runtime_manifest.sh", "v0.0.0-test", manifestPath)
	generate.Dir = root
	generate.Env = isolatedCommandEnvironment("GIT_COMMIT="+strings.Repeat("d", 40), "BUILD_TIME=2026-09-09T00:00:00Z",
		"MCP_CONTRACT_PATH=docs/MCP_Contract.md", "CORE_MIGRATIONS_PATH=internal/store/migrations/core",
		"OUTBOUND_POLICY_PATH=configs/policy/autonomous-outbound-v1.yaml")
	if out, err := generate.CombinedOutput(); err != nil {
		t.Fatalf("generate: %v: %s", err, out)
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]string
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	flags := []string{}
	for field, symbol := range map[string]string{"runtime_version": "RuntimeVersion", "mcp_contract_hash": "MCPContractHash", "core_schema_hash": "CoreSchemaHash", "outbound_policy_version": "OutboundPolicyVersion", "outbound_policy_sha256": "OutboundPolicySHA256", "build_commit": "BuildCommit", "build_time": "BuildTime"} {
		flags = append(flags, "-X", "neuralmail/internal/release."+symbol+"="+manifest[field])
	}
	binary := filepath.Join(dir, "nerve-runtime")
	build := exec.Command("go", "build", "-ldflags", strings.Join(flags, " "), "-o", binary, "./cmd/neuralmaild")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	raw, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	expectedHash := fmt.Sprintf("%x", sha256.Sum256(raw))
	run := exec.Command(binary, "compatibility", "--json")
	run.Env = isolatedCommandEnvironment("NM_CONFIG="+filepath.Join(dir, "missing.yaml"), "NM_DB_DSN=invalid", "NM_CLOUD_MODE=true",
		"NERVE_SCHEMA_TRANSITION_MODE=invalid", "CORE_SCHEMA_MIN_REQUIRED=1", "CORE_SCHEMA_MAX_SUPPORTED=9999", "BUILD_COMMIT=forged")
	out, err := run.Output()
	if err != nil {
		t.Fatalf("offline report: %v", err)
	}
	var report RuntimeCompatibilityReport
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("JSON report: %v: %s", err, out)
	}
	if report.SchemaVersion != 1 || report.Kind != "nerve-runtime-compatibility" || report.DatabaseVerified || report.AdmissionVerified || report.ExecutableSHA256 != expectedHash || report.Executable != "nerve-runtime" || !reflect.DeepEqual(report.Manifest, manifest) {
		t.Fatalf("report does not bind executable/manifest: %+v", report)
	}
	// A dev binary cannot report a release identity using environment overrides.
	dev := filepath.Join(dir, "dev-runtime")
	build = exec.Command("go", "build", "-o", dev, "./cmd/neuralmaild")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("dev build: %v: %s", err, out)
	}
	run = exec.Command(dev, "compatibility", "--json")
	run.Env = generate.Env
	if out, err := run.CombinedOutput(); err == nil || !bytes.Contains(out, []byte("runtime release identity is not compiled")) {
		t.Fatalf("dev accepted: %v %s", err, out)
	}
}
