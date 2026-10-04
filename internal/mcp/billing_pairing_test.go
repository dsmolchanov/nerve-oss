package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"neuralmail/internal/auth"
	"neuralmail/internal/entitlements"
)

func TestPairingModernToolPinsCallerProofAndBypassesExhaustedMailQuota(t *testing.T) {
	cfg := hostedRouterConfig()
	cfg.Cloud.DashboardBaseURL = "https://nerve.example"
	runtime := NewServer(cfg, nil, auth.NewService(cfg, nil), nil)
	gate := &fakeEntitlementGate{preAuthErr: entitlements.ErrQuotaExceeded}
	runtime.Entitlements = gate
	stub := &recordingHostedBilling{}
	runtime.HostedBilling = stub
	principal := activeBillingPrincipal("nerve:billing.subscribe")
	args := map[string]any{"pairing_id": hostedTestIntent, "browser_session_sha256": strings.Repeat("a", 64), "challenge": base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))}
	h := NewSDKHandler(runtime, true)
	list := httptest.NewRecorder()
	h.ServeHTTP(list, billingModernRequest(t, principal, "tools/list", map[string]any{"_meta": modernOAuthMeta()}, ""))
	if !strings.Contains(list.Body.String(), `"nerve_billing_pairing_confirm"`) {
		t.Fatalf("pairing absent: %s", list.Body.String())
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, billingModernRequest(t, principal, "tools/call", map[string]any{"_meta": modernOAuthMeta(), "name": billingPairingConfirmToolName, "arguments": args}, billingPairingConfirmToolName))
	if stub.calls != 1 || stub.caller.Principal.OrgID != principal.OrgID || gate.preAuthCalls != 0 || !strings.Contains(w.Body.String(), `"state":"completed"`) || strings.Contains(w.Body.String(), args["challenge"].(string)) {
		t.Fatalf("pairing leaked or charged quota calls=%d body=%s", stub.calls, w.Body.String())
	}
	raw, _ := json.Marshal(args)
	for _, invalid := range []string{
		strings.TrimSuffix(string(raw), "}") + `,"org_id":"foreign"}`,
		strings.TrimSuffix(string(raw), "}") + `,"identity_subject":"caller"}`,
		strings.TrimSuffix(string(raw), "}") + `,"pairing_id":"duplicate"}`,
		`null`, `[]`, string(raw) + ` {}`,
		strings.Replace(string(raw), strings.Repeat("a", 64), strings.Repeat("A", 64), 1),
	} {
		_, e := invokeBillingPairingConfirmTool(context.Background(), stub, BillingCaller{Principal: principal}, json.RawMessage(invalid), cfg.Cloud.DashboardBaseURL)
		if e == nil || stub.calls != 1 {
			t.Fatal("caller authority reached pairing service")
		}
	}
	for _, target := range []string{"https://evil.example/billing/pairing?pairing_id=" + hostedTestIntent, "https://nerve.example/billing/pairing?pairing_id=" + hostedTestIntent + "&org_id=caller", "https://nerve.example/billing/upgrade?intent=" + hostedTestIntent, "https://nerve.example/billing/pairing?pairing_id=caller"} {
		if ValidBillingPairingConfirmResult(BillingPairingConfirmResult{ResultType: "complete", State: "completed", OfferID: starterOfferID, UpgradeURL: target}, cfg.Cloud.DashboardBaseURL) {
			t.Fatal("untrusted pairing target accepted")
		}
	}
}
