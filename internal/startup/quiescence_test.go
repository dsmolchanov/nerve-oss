package startup

import (
	"context"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

func TestSchemaQuiescenceRejectsBeforeApplicationStartup(t *testing.T) {
	for _, tc := range []struct {
		mode          string
		cloud         bool
		command       string
		handled, fail bool
	}{
		{"", true, "serve", false, false}, {"invalid", true, "serve", true, true},
		{"quiescent", false, "serve", true, true}, {"quiescent", true, "mcp-stdio", true, true},
		{"quiescent", true, "migrate-core", true, true},
	} {
		got, err := SchemaQuiescence(context.Background(), tc.mode, tc.cloud, tc.command, "")
		if got != tc.handled || (err != nil) != tc.fail {
			t.Fatalf("%+v => %v, %v", tc, got, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if handled, err := SchemaQuiescence(ctx, "quiescent", true, "worker", ""); !handled || err != nil {
		t.Fatalf("worker: %v %v", handled, err)
	}
}

// An invalid address makes an accidental listener construction fail deterministically.
func TestSchemaQuiescenceOpensNoListener(t *testing.T) {
	for _, command := range []string{"serve", "worker"} {
		t.Run(command, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			handled, err := SchemaQuiescence(ctx, "quiescent", true, command, "not a TCP address")
			if !handled || err != nil {
				t.Fatalf("quiescence attempted initialization: %v %v", handled, err)
			}
		})
	}
}

// The maintenance path cannot quietly regain a public handler or socket.
func TestSchemaQuiescenceHasNoNetworkDependency(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "quiescence.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, dependency := range file.Imports {
		name, err := strconv.Unquote(dependency.Path.Value)
		if err != nil {
			t.Fatal(err)
		}
		if name != "context" && name != "errors" && name != "log" {
			t.Fatalf("quiescence dependency requires review: %s", name)
		}
	}
}
