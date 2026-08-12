package mcp

import (
	"testing"

	"neuralmail/internal/auth"
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
