package startup

import (
	"context"
	"strings"
	"testing"
)

func TestCompiledSchemaWindow(t *testing.T) {
	old := CompiledSchemaWindow
	t.Cleanup(func() { CompiledSchemaWindow = old })
	t.Setenv("CORE_SCHEMA_MIN_REQUIRED", "1")
	t.Setenv("CORE_SCHEMA_MAX_SUPPORTED", "9999")
	for _, tc := range []struct {
		raw      string
		min, max int64
		valid    bool
	}{
		{"", 29, 29, true}, {"2:29:30", 29, 30, true}, {"2:30:30", 30, 30, true},
		{"1:29:30", 0, 0, false}, {"3:29:30", 0, 0, false}, {"2:28:30", 0, 0, false},
		{"2:31:30", 0, 0, false}, {"2:029:30", 0, 0, false}, {"2:+29:30", 0, 0, false},
		{"2:29:10000", 0, 0, false}, {"2:29:30:31", 0, 0, false}, {"unknown", 0, 0, false},
	} {
		CompiledSchemaWindow = tc.raw
		w, err := EffectiveMigrationWindow()
		if tc.valid {
			if err != nil || w.CoreMinRequired != tc.min || w.CoreMaxSupported != tc.max || w.IncludeCloud {
				t.Fatalf("%q: %+v %v", tc.raw, w, err)
			}
		} else if err == nil {
			t.Fatalf("accepted %q", tc.raw)
		}
	}
}

func TestSuccessorStartupRejectsMutationModesBeforeDatabase(t *testing.T) {
	old := CompiledSchemaWindow
	t.Cleanup(func() { CompiledSchemaWindow = old })
	CompiledSchemaWindow = "2:29:30"
	for _, cloud := range []bool{false, true} {
		for _, mode := range []string{"off", "apply-to-max"} {
			t.Setenv("NM_MIGRATE_ON_START", mode)
			err := Migrate(context.Background(), nil, cloud)
			if err == nil || !strings.Contains(err.Error(), "require NM_MIGRATE_ON_START=verify") {
				t.Fatalf("mode=%s cloud=%t: %v", mode, cloud, err)
			}
		}
	}
	CompiledSchemaWindow = "broken"
	t.Setenv("NM_MIGRATE_ON_START", "verify")
	if err := Migrate(context.Background(), nil, true); err == nil {
		t.Fatal("invalid compiled window accepted")
	}
}
