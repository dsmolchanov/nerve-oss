package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"neuralmail/internal/auth"
)

func freeTestResult() FreeSetupResult {
	return FreeSetupResult{ResultType: "complete", SetupID: "abcdef11-1111-4111-8111-111111111111", OnboardingID: "22222222-2222-4222-8222-222222222222", Generation: 7, ApexDomain: "example.com", SetupState: "pending", State: "provisioning", SetupExpiresAt: time.Unix(1723000000, 0).UTC(), OwnershipTXT: OnboardingDNSRecord{Type: "TXT", Name: "_nerve-verify.example.com", Value: "nerve-free-verification=" + strings.Repeat("a", 64)}, NextAction: "configure_ownership_dns_then_verify"}
}
func freeTestCaller() OnboardingCaller {
	return OnboardingCaller{Principal: auth.Principal{Kind: auth.PrincipalM2MOnboarding, ClientID: "client-1", Generation: 7, TokenID: "token-1", AuthMethod: "m2m_bearer", Scopes: []string{"nerve:onboarding"}}, Authorization: "Bearer original-token"}
}

type recordingFreeProvisioner struct {
	calls  int
	caller OnboardingCaller
	op     string
	input  json.RawMessage
	result FreeSetupResult
	err    error
}

func (p *recordingFreeProvisioner) FreeSetup(_ context.Context, c OnboardingCaller, op string, i json.RawMessage) (FreeSetupResult, error) {
	p.calls++
	p.caller = c
	p.op = op
	p.input = i
	return p.result, p.err
}
func TestFreeSetupInputsAreClosedAndCanonical(t *testing.T) {
	base := `{"idempotency_key":"free-1","organization_name":" Free   Org ","apex_domain":"EXAMPLE.COM.","local_part":"agent"}`
	got, err := DecodeFreeSetupInput("setup", 7, json.RawMessage(base))
	if err != nil || string(got) != `{"idempotency_key":"free-1","organization_name":"Free Org","apex_domain":"example.com","local_part":"agent"}` {
		t.Fatalf("normalized=%s err=%v", got, err)
	}
	invalid := []string{`null`, `{}`, base + ` {}`, strings.Replace(base, `"agent"`, `null`, 1), strings.Replace(base, `"agent"`, `"agent","LOCAL_PART":"other"`, 1)}
	for _, field := range []string{"org_id", "client_id", "generation", "owner_id", "mailbox_mode", "subscription_id", "recipient_limit", "proof_observed_at", "plan_code"} {
		invalid = append(invalid, strings.TrimSuffix(base, "}")+`,"`+field+`":"override"}`)
	}
	for _, domain := range []string{"mail.example.com", "com", "localhost", "127.0.0.1", "x.blogspot.com", "example.invalid"} {
		invalid = append(invalid, strings.Replace(base, "EXAMPLE.COM.", domain, 1))
	}
	for _, raw := range invalid {
		if _, err := DecodeFreeSetupInput("setup", 7, json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	for _, op := range []string{"status", "verify-domain"} {
		for _, raw := range []string{`{"org_id":"foreign"}`, `{"generation":7}`, `{"proof_observed_at":"now"}`} {
			if _, err := DecodeFreeSetupInput(op, 7, json.RawMessage(raw)); err == nil {
				t.Fatalf("%s accepted %s", op, raw)
			}
		}
	}
	for _, raw := range []string{`{"idempotency_key":"close","expected_generation":8}`, `{"idempotency_key":"close","expected_generation":7,"org_id":"foreign"}`, `{"idempotency_key":"close"}`} {
		if _, err := DecodeFreeSetupInput("close", 7, json.RawMessage(raw)); err == nil {
			t.Fatalf("close accepted %s", raw)
		}
	}
}
func TestFreeSetupResultStrictWireAndPreProofMail(t *testing.T) {
	result := freeTestResult()
	raw, _ := json.Marshal(result)
	if _, err := DecodeFreeSetupResult(raw, 7); err != nil {
		t.Fatal(err)
	}
	invalid := []string{strings.Replace(string(raw), `"reauthorize":false`, `"reauthorize":null`, 1), strings.Replace(string(raw), `"reauthorize":false`, `"reauthorize":false,"Reauthorize":false`, 1), strings.Replace(string(raw), `"reauthorize":false`, `"reauthorize":false,"provider_id":"private"`, 1), strings.Replace(string(raw), `"reauthorize":false`, `"reauthorize":false,"address":"agent@example.com"`, 1), strings.Replace(string(raw), `"reauthorize":false`, `"reauthorize":true`, 1), strings.Replace(string(raw), `"generation":7`, `"generation":8`, 1), strings.Replace(string(raw), `"type":"TXT"`, `"type":"TXT","ttl":"secret"`, 1), strings.Replace(string(raw), `"reauthorize":false`, `"wrong":false`, 1)}
	for _, body := range invalid {
		if _, err := DecodeFreeSetupResult([]byte(body), 7); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
	result.State = "active"
	result.SetupState = "proof_verified"
	result.Address = "agent@example.com"
	result.Reauthorize = true
	result.NextAction = "reauthorize_org"
	raw, _ = json.Marshal(result)
	if _, err := DecodeFreeSetupResult(raw, 7); err != nil {
		t.Fatal(err)
	}
	result.Address = "agent@foreign.com"
	raw, _ = json.Marshal(result)
	if _, err := DecodeFreeSetupResult(raw, 7); err == nil {
		t.Fatal("foreign active address accepted")
	}
}
func TestFreeSetupModernCatalogAndEveryOperationPreserveCaller(t *testing.T) {
	cfg := hostedRouterConfig()
	runtime := NewServer(cfg, nil, auth.NewService(cfg, nil), nil)
	runtime.Onboarding = &recordingOnboardingProvisioner{}
	p := &recordingFreeProvisioner{result: freeTestResult()}
	runtime.FreeSetup = p
	handler := NewSDKHandler(runtime, true)
	caller := freeTestCaller()
	request := onboardingModernRequest(t, caller.Principal, "tools/list", map[string]any{"_meta": modernOAuthMeta()}, "")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, request)
	if w.Code != 200 {
		t.Fatalf("list %d %s", w.Code, w.Body.String())
	}
	for _, name := range []string{"nerve_free_setup", "nerve_free_status", "nerve_free_verify_domain", "nerve_free_close", "nerve_free_resume"} {
		if !strings.Contains(w.Body.String(), `"name":"`+name+`"`) {
			t.Fatalf("missing %s: %s", name, w.Body.String())
		}
	}
	for name, arguments := range map[string]map[string]any{"nerve_free_setup": {"idempotency_key": "free-1", "organization_name": "Free Org", "apex_domain": "example.com", "local_part": "agent"}, "nerve_free_status": {}, "nerve_free_verify_domain": {}, "nerve_free_close": {"idempotency_key": "close", "expected_generation": 7}, "nerve_free_resume": {"idempotency_key": "resume-one"}} {
		p.result = freeTestResult()
		if name == "nerve_free_resume" {
			p.result = freeResumeTestResult()
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, onboardingModernRequest(t, caller.Principal, "tools/call", map[string]any{"_meta": modernOAuthMeta(), "name": name, "arguments": arguments}, name))
		if w.Code != 200 || p.op != freeSetupToolOperation(name) || p.caller.Authorization != caller.Authorization || p.caller.Principal.TokenID != caller.Principal.TokenID || strings.Contains(w.Body.String(), `"isError":true`) {
			t.Fatalf("call %s status%d op%s result%s", name, w.Code, p.op, w.Body.String())
		}
	}
	for _, mutate := range []func(*OnboardingCaller){func(c *OnboardingCaller) { c.Principal.Kind = auth.PrincipalM2MOrg; c.Principal.OrgID = "foreign" }, func(c *OnboardingCaller) { c.Principal.AuthMethod = "local" }, func(c *OnboardingCaller) { c.Principal.Scopes = []string{"nerve:email.read"} }, func(c *OnboardingCaller) { c.Authorization = "" }, func(c *OnboardingCaller) { c.Principal.Generation = 0 }} {
		c := caller
		mutate(&c)
		before := p.calls
		if _, err := invokeFreeSetupTool(context.Background(), p, c, "nerve_free_status", json.RawMessage(`{}`)); err == nil || p.calls != before {
			t.Fatal("invalid caller reached provider")
		}
	}
	runtime.FreeSetup = nil
	for _, tool := range modernToolCatalog(context.Background(), runtime, caller.Principal) {
		if freeSetupToolOperation(tool.Name) != "" {
			t.Fatal("unconfigured Free tool visible")
		}
	}
	runtime.FreeSetup = p
	runtime.Config.Cloud.Mode = false
	for _, tool := range modernToolCatalog(context.Background(), runtime, caller.Principal) {
		if freeSetupToolOperation(tool.Name) != "" {
			t.Fatal("self-host Free tool visible")
		}
	}
}

func freeResumeTestResult() FreeSetupResult {
	result := freeTestResult()
	result.State, result.SetupState = "active", "proof_verified"
	result.Address, result.NextAction, result.Reauthorize = "agent@example.com", "reauthorize_org", true
	result.ReturnReceiptID, result.ReturnIdempotencyKey = "33333333-3333-4333-8333-333333333333", "resume-one"
	return result
}

func TestFreeResumeRequiresExactDurableDecisionAndOriginalCaller(t *testing.T) {
	caller := freeTestCaller()
	p := &recordingFreeProvisioner{result: freeResumeTestResult()}
	input := json.RawMessage(`{"idempotency_key":"resume-one"}`)
	result, err := invokeFreeSetupTool(context.Background(), p, caller, "nerve_free_resume", input)
	if err != nil || result.ReturnReceiptID == "" || p.op != "resume" || p.caller.Authorization != caller.Authorization || p.calls != 1 {
		t.Fatalf("resume=%+v %v calls=%d", result, err, p.calls)
	}
	for _, raw := range []string{`{}`, `{"idempotency_key":null}`, `{"idempotency_key":""}`, `{"idempotency_key":"resume-one","org_id":"foreign"}`, `{"idempotency_key":"resume-one","expected_generation":8}`, `{"idempotency_key":"resume-one","idempotency_key":"other"}`} {
		before := p.calls
		if _, err := invokeFreeSetupTool(context.Background(), p, caller, "nerve_free_resume", json.RawMessage(raw)); err == nil || p.calls != before {
			t.Fatalf("invalid resume reached provisioner: %s", raw)
		}
	}
	for _, changed := range []FreeSetupResult{freeTestResult(), func() FreeSetupResult { r := freeResumeTestResult(); r.ReturnIdempotencyKey = "other"; return r }(), func() FreeSetupResult { r := freeResumeTestResult(); r.ReturnReceiptID = ""; return r }()} {
		p.result = changed
		if _, err := invokeFreeSetupTool(context.Background(), p, caller, "nerve_free_resume", input); !errors.Is(err, ErrOnboardingOutcomeUnknown) {
			t.Fatalf("uncorrelated mutation result: %v", err)
		}
	}
}

func TestFreeReturnProvenanceIsClosedPairedAndActiveOnly(t *testing.T) {
	active := freeResumeTestResult()
	raw, _ := json.Marshal(active)
	if _, err := DecodeFreeSetupResult(raw, 7); err != nil {
		t.Fatal(err)
	}
	for _, changed := range []FreeSetupResult{func() FreeSetupResult { r := active; r.ReturnReceiptID = ""; return r }(), func() FreeSetupResult { r := active; r.ReturnIdempotencyKey = ""; return r }(), func() FreeSetupResult {
		r := active
		r.ReturnReceiptID = "33333333-3333-4333-8333-33333333333A"
		return r
	}(), func() FreeSetupResult {
		r := freeTestResult()
		r.ReturnReceiptID = active.ReturnReceiptID
		r.ReturnIdempotencyKey = active.ReturnIdempotencyKey
		return r
	}()} {
		body, _ := json.Marshal(changed)
		if _, err := DecodeFreeSetupResult(body, 7); err == nil {
			t.Fatalf("invalid return provenance: %s", body)
		}
	}
	for _, field := range []string{"return_receipt_id", "return_idempotency_key"} {
		body := strings.Replace(string(raw), `"`+field+`":"`+map[string]string{"return_receipt_id": active.ReturnReceiptID, "return_idempotency_key": active.ReturnIdempotencyKey}[field]+`"`, `"`+field+`":null`, 1)
		if _, err := DecodeFreeSetupResult([]byte(body), 7); err == nil {
			t.Fatal("null return provenance admitted")
		}
	}
}
