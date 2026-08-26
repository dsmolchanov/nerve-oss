package store

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Every statement wrapped in outboxSQL must lose all four Core 0029 columns
// once the pre-fence replacer runs. The replacements are literal, so this test
// is what makes editing the fenced SQL safe: a change that escapes a
// replacement fails here instead of at run time on Core 28.
func TestPreFenceOutboxStatementsDropEveryFenceColumn(t *testing.T) {
	source, err := os.ReadFile("outbox.go")
	if err != nil {
		t.Fatalf("read outbox.go: %v", err)
	}
	pattern := regexp.MustCompile("(?s)adaptOutboxSQL\\([A-Za-z]+, `(.*?)`\\)")
	matches := pattern.FindAllStringSubmatch(string(source), -1)
	if len(matches) == 0 {
		t.Fatal("no adaptOutboxSQL statements found; the wrapper or this test drifted")
	}
	for _, match := range matches {
		fenced := match[1]
		adapted := preFenceOutboxSQL.Replace(fenced)
		for _, column := range coreOutboundFenceColumns {
			if strings.Contains(adapted, column) {
				t.Errorf("pre-fence statement still references %s:\n%s", column, adapted)
			}
		}
		if strings.Contains(adapted, "org_outbound_policy_state") {
			t.Errorf("pre-fence statement still references org_outbound_policy_state:\n%s", adapted)
		}
	}
	t.Logf("verified %d adapted statements", len(matches))
}

// The fenced form must keep the columns. A replacer that fired unconditionally
// would silently disable the policy fence on Core 29.
func TestFencedOutboxStatementsRetainTheFence(t *testing.T) {
	claim := adaptOutboxSQL(true, "SELECT coalesce(o.autonomous_policy_epoch, 0)")
	if !strings.Contains(claim, "autonomous_policy_epoch") {
		t.Fatal("fenced statement lost its fence column")
	}
}

// A dropped clause can remove the last reference to a placeholder while the
// caller still passes its argument, which pgx rejects at execution rather than
// at build. Every adapted statement that loses trailing placeholders must
// therefore route its arguments through outboxArgs, and this fails when one
// does not -- a test that merely logged would leave the next occurrence green
// until it failed in production.
func TestPreFenceStatementsTrimStrandedPlaceholderArguments(t *testing.T) {
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, "outbox.go", nil, 0)
	if err != nil {
		t.Fatalf("parse outbox.go: %v", err)
	}
	placeholder := regexp.MustCompile(`\$([0-9]+)`)
	highest := func(sql string) int {
		max := 0
		for _, m := range placeholder.FindAllStringSubmatch(sql, -1) {
			n := 0
			fmt.Sscanf(m[1], "%d", &n)
			if n > max {
				max = n
			}
		}
		return max
	}
	checked := 0
	ast.Inspect(parsed, func(node ast.Node) bool {
		outer, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		// Find the query literal this call passes through outboxSQL.
		var fenced string
		usesOutboxArgs := false
		for _, arg := range outer.Args {
			inner, ok := arg.(*ast.CallExpr)
			if !ok {
				continue
			}
			name := ""
			switch fn := inner.Fun.(type) {
			case *ast.SelectorExpr:
				name = fn.Sel.Name
			case *ast.Ident:
				name = fn.Name
			default:
				continue
			}
			switch name {
			case "adaptOutboxSQL":
				if len(inner.Args) == 2 {
					if lit, ok := inner.Args[1].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						fenced = strings.Trim(lit.Value, "`")
					}
				}
			case "trimOutboxArgs":
				usesOutboxArgs = true
			}
		}
		if fenced == "" {
			return true
		}
		checked++
		adapted := preFenceOutboxSQL.Replace(fenced)
		dropped := highest(fenced) - highest(adapted)
		if dropped > 0 && !usesOutboxArgs {
			t.Errorf("adapted statement drops %d trailing placeholder(s) but its caller does not use trimOutboxArgs; "+
				"pgx will reject it with a parameter-count error on the pre-fence path:\n%s", dropped, adapted)
		}
		return true
	})
	if checked == 0 {
		t.Fatal("no adaptOutboxSQL call sites found; the wrapper or this test drifted")
	}
	t.Logf("verified %d adaptOutboxSQL call sites", checked)
}

func TestUndeterminedSchemaDefaultsToFenced(t *testing.T) {
	store := &Store{fence: newEnabledFence()}
	if !store.OutboundFenceEnabled() {
		t.Fatal("fence capability must default to enabled so an unknown schema fails closed")
	}
	if err := store.requireOutboundFence(context.Background(), "op"); err != nil {
		t.Fatalf("fenced store rejected an autonomous operation: %v", err)
	}
	store.fence.Store(false)
	if err := store.requireOutboundFence(context.Background(), "op"); err == nil {
		t.Fatal("pre-fence store admitted an autonomous operation")
	}
}

// Any SQL literal in outbox.go that names a fence column must either go
// through outboxSQL or live in a function guarded by requireOutboundFence.
// This is the check that catches a new or edited statement which forgot both --
// the failure mode that reached Core 28 unnoticed.
func TestEveryFenceReferencingStatementIsAdaptedOrGuarded(t *testing.T) {
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, "outbox.go", nil, 0)
	if err != nil {
		t.Fatalf("parse outbox.go: %v", err)
	}
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		guarded := false
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			if sel, ok := node.(*ast.SelectorExpr); ok && sel.Sel.Name == "requireOutboundFence" {
				guarded = true
			}
			return true
		})
		if guarded {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "adaptOutboxSQL" {
					return false
				}
			}
			lit, ok := node.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			for _, column := range coreOutboundFenceColumns {
				if strings.Contains(lit.Value, column) {
					t.Errorf("%s: statement references %s without adaptOutboxSQL or a requireOutboundFence guard",
						fn.Name.Name, column)
					return false
				}
			}
			return true
		})
	}
}
