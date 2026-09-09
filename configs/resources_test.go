package configs

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestBundledResourcesAndOverrides(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, path := range []string{"configs/policy/support-default-v1.yaml", "configs/policy/autonomous-outbound-v1.yaml", "configs/prompts/v1/draft.md", "configs/prompts/v1/extract.md", "configs/prompts/v1/triage.md", "configs/meters/tool_costs.yaml", "configs/schemas/support_v1.json"} {
		data, err := ReadFile(path)
		if err != nil || len(data) == 0 {
			t.Fatalf("%s: %v", path, err)
		}
	}
	override := filepath.Join(t.TempDir(), "custom.yaml")
	if err := os.WriteFile(override, []byte("custom"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFile(override)
	if err != nil || !bytes.Equal(got, []byte("custom")) {
		t.Fatalf("override %s: %v", got, err)
	}
	if _, err := ReadFile(override + "-missing"); err == nil {
		t.Fatal("missing override silently ignored")
	}
	if _, err := ReadFile("configs/../policy/support-default-v1.yaml"); err == nil {
		t.Fatal("noncanonical fallback")
	}
}
