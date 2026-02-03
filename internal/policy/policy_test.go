package policy

import "testing"

func TestPolicyForbiddenPhrase(t *testing.T) {
	p := Policy{ForbiddenPhrases: []string{"guarantee"}}
	_, res := Evaluate("We guarantee success", p)
	if res.Allowed {
		t.Fatalf("expected forbidden phrase to block")
	}
	if res.ViolationLevel != "critical" {
		t.Fatalf("expected critical violation")
	}
}
