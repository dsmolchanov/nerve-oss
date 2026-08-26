package store

import (
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
	pattern := regexp.MustCompile("(?s)outboxSQL\\(`(.*?)`\\)")
	matches := pattern.FindAllStringSubmatch(string(source), -1)
	if len(matches) == 0 {
		t.Fatal("no outboxSQL statements found; the wrapper or this test drifted")
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
	store := &Store{}
	store.fence = newEnabledFence()
	claim := store.outboxSQL("SELECT coalesce(o.autonomous_policy_epoch, 0)")
	if !strings.Contains(claim, "autonomous_policy_epoch") {
		t.Fatal("fenced statement lost its fence column")
	}
}

// A dropped clause can remove the last reference to a placeholder while the
// caller still passes its argument, which pgx rejects at execution rather than
// at build. Compare the highest placeholder in each adapted statement against
// the fenced original so that mismatch surfaces here too.
func TestPreFenceStatementsDoNotStrandPlaceholders(t *testing.T) {
	source, err := os.ReadFile("outbox.go")
	if err != nil {
		t.Fatalf("read outbox.go: %v", err)
	}
	pattern := regexp.MustCompile("(?s)outboxSQL\\(`(.*?)`\\)")
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
	for _, match := range pattern.FindAllStringSubmatch(string(source), -1) {
		fenced := match[1]
		adapted := preFenceOutboxSQL.Replace(fenced)
		if got, want := highest(adapted), highest(fenced); got != want {
			t.Logf("adapted statement drops placeholders %d..%d; its caller must trim the same trailing arguments via outboxArgs:\n%s",
				got+1, want, adapted)
		}
	}
}

func TestUndeterminedSchemaDefaultsToFenced(t *testing.T) {
	store := &Store{fence: newEnabledFence()}
	if !store.OutboundFenceEnabled() {
		t.Fatal("fence capability must default to enabled so an unknown schema fails closed")
	}
	if err := store.requireOutboundFence("op"); err != nil {
		t.Fatalf("fenced store rejected an autonomous operation: %v", err)
	}
	store.fence.Store(false)
	if err := store.requireOutboundFence("op"); err == nil {
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
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "outboxSQL" {
					return false
				}
			}
			lit, ok := node.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			for _, column := range coreOutboundFenceColumns {
				if strings.Contains(lit.Value, column) {
					t.Errorf("%s: statement references %s without outboxSQL or a requireOutboundFence guard",
						fn.Name.Name, column)
					return false
				}
			}
			return true
		})
	}
}
