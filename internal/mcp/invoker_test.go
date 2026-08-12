package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"neuralmail/internal/auth"
	"neuralmail/internal/config"
	"neuralmail/internal/store"
)

func TestRequiredToolScopePreservesLegacyAndSplitsM2M(t *testing.T) {
	legacy := auth.Principal{Kind: auth.PrincipalLegacyJWT}
	m2m := auth.Principal{Kind: auth.PrincipalM2MOrg}
	for _, test := range []struct {
		principal auth.Principal
		tool      string
		want      string
	}{
		{legacy, "send_reply", "nerve:email.send"},
		{legacy, "compose_email", "nerve:email.send"},
		{m2m, "send_reply", "nerve:email.reply"},
		{m2m, "compose_email", "nerve:email.compose"},
		{m2m, "get_thread", "nerve:email.read"},
	} {
		if got := requiredToolScope(test.principal, test.tool); got != test.want {
			t.Fatalf("%s/%s scope=%q want=%q", test.principal.Kind, test.tool, got, test.want)
		}
	}
}

func TestM2MOutboundInvocationFailsClosedWithoutLivePolicy(t *testing.T) {
	cfg := config.Default()
	cfg.Cloud.Mode = true
	authService := auth.NewService(cfg, nil)
	runtime := NewServer(cfg, nil, authService, nil)
	principal := auth.Principal{
		Kind: auth.PrincipalM2MOrg, OrgID: "org-1",
		Scopes: []string{"nerve:email.compose"},
	}
	_, err := runtime.Invoker.Invoke(auth.WithPrincipal(context.Background(), principal), ToolInvocation{
		Name: "compose_email", Arguments: json.RawMessage(`{}`),
	})
	var policyErr *outboundPolicyError
	if !errors.As(err, &policyErr) || policyErr.Code != "outbound_policy_unavailable" {
		t.Fatalf("compose did not fail closed without policy: %v", err)
	}
}

func TestStoreOutboundPolicyGateUsesExplicitLiveOrgFlags(t *testing.T) {
	trueValue := true
	falseValue := false
	principal := auth.Principal{Kind: auth.PrincipalM2MOrg, OrgID: "org-1"}
	allowed := &storeOutboundPolicyGate{store: fakeOutboundPolicyStore{values: map[string]store.FeatureFlagValues{
		"autonomous_outbound_policy": {Org: &trueValue},
		"email_outbound_suspended":   {Org: &falseValue},
		"email_compose_org_enabled":  {Org: &trueValue},
	}}}
	if err := allowed.Authorize(context.Background(), principal, "send_reply"); err != nil {
		t.Fatalf("explicit reply policy denied: %v", err)
	}
	if err := allowed.Authorize(context.Background(), principal, "compose_email"); err != nil {
		t.Fatalf("explicit compose policy denied: %v", err)
	}

	denied := &storeOutboundPolicyGate{store: fakeOutboundPolicyStore{values: map[string]store.FeatureFlagValues{
		"autonomous_outbound_policy": {Org: &trueValue},
		"email_outbound_suspended":   {Org: &trueValue},
		"email_compose_org_enabled":  {Org: &trueValue},
	}}}
	if err := denied.Authorize(context.Background(), principal, "compose_email"); err == nil {
		t.Fatal("suspended org compose was allowed")
	}
	missing := &storeOutboundPolicyGate{store: fakeOutboundPolicyStore{values: map[string]store.FeatureFlagValues{}}}
	if err := missing.Authorize(context.Background(), principal, "send_reply"); err == nil {
		t.Fatal("missing explicit policy was allowed")
	}
	failing := &storeOutboundPolicyGate{store: fakeOutboundPolicyStore{err: errors.New("database unavailable")}}
	var policyErr *outboundPolicyError
	if err := failing.Authorize(context.Background(), principal, "send_reply"); !errors.As(err, &policyErr) || policyErr.Code != "outbound_policy_unavailable" {
		t.Fatalf("policy read error did not fail closed: %v", err)
	}
}

type fakeOutboundPolicyStore struct {
	values map[string]store.FeatureFlagValues
	err    error
}

func (fake fakeOutboundPolicyStore) LookupFeatureFlagForOrg(_ context.Context, _ string, flag string) (store.FeatureFlagValues, error) {
	if fake.err != nil {
		return store.FeatureFlagValues{}, fake.err
	}
	return fake.values[flag], nil
}
