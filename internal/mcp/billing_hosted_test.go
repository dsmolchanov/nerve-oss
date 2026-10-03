package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"neuralmail/internal/auth"
	"neuralmail/internal/entitlements"
	"neuralmail/internal/localauth"
)

const hostedTestIntent = "11111111-1111-4111-8111-111111111111"

type recordingHostedBilling struct {
	caller BillingCaller
	input  BillingUpgradeInput
	calls  int
	result BillingUpgradeResult
	err    error
}

func (stub *recordingHostedBilling) Upgrade(_ context.Context, caller BillingCaller,
	input BillingUpgradeInput) (BillingUpgradeResult, error) {
	stub.caller, stub.input = caller, input
	stub.calls++
	return stub.result, stub.err
}

func (stub *recordingHostedBilling) BillingStatus(_ context.Context, caller BillingCaller) (BillingStatusResult, error) {
	stub.caller = caller
	stub.calls++
	return BillingStatusResult{ResultType: "complete", HostedState: "none"}, stub.err
}

func hostedBillingResult() BillingUpgradeResult {
	return BillingUpgradeResult{ResultType: "complete", State: "awaiting_owner",
		OfferID:    starterOfferID,
		UpgradeURL: "https://nerve.example/billing/upgrade?intent=" + hostedTestIntent}
}

func TestHostedUpgradeRequiresPrincipalScopeAndExactServerOrigin(t *testing.T) {
	cfg := hostedRouterConfig()
	cfg.Cloud.DashboardBaseURL = "https://nerve.example"
	runtime := NewServer(cfg, nil, auth.NewService(cfg, nil), nil)
	quotaGate := &fakeEntitlementGate{preAuthErr: entitlements.ErrQuotaExceeded}
	runtime.Entitlements = quotaGate
	stub := &recordingHostedBilling{result: hostedBillingResult()}
	runtime.HostedBilling = stub
	principal := activeBillingPrincipal("nerve:billing.subscribe")
	if !billingUpgradeToolAvailable(runtime, principal) ||
		billingUpgradeToolAvailable(runtime, activeBillingPrincipal("nerve:email.read")) {
		t.Fatal("hosted tool visibility ignored billing scope")
	}
	if !billingUpgradeToolAvailable(runtime, principal) {
		t.Fatal("hosted tool unavailable for billing principal")
	}
	for _, base := range []string{"", "http://nerve.example", "https://nerve.example/other",
		"https://user@nerve.example", "https://nerve.example/?x=1"} {
		badConfig := cfg
		badConfig.Cloud.DashboardBaseURL = base
		badRuntime := NewServer(badConfig, nil, auth.NewService(badConfig, nil), nil)
		badRuntime.HostedBilling = stub
		if billingUpgradeToolAvailable(badRuntime, principal) {
			t.Fatalf("unsafe dashboard origin exposed hosted tool: %q", base)
		}
	}

	handler := NewSDKHandler(runtime, true)
	list := httptest.NewRecorder()
	handler.ServeHTTP(list, billingModernRequest(t, principal, "tools/list", map[string]any{
		"_meta": modernOAuthMeta(),
	}, ""))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"nerve_billing_upgrade"`) {
		t.Fatalf("hosted tool absent from scoped modern list: %d %s", list.Code, list.Body.String())
	}
	call := httptest.NewRecorder()
	handler.ServeHTTP(call, billingModernRequest(t, principal, "tools/call", map[string]any{
		"_meta": modernOAuthMeta(), "name": billingUpgradeToolName,
		"arguments": map[string]any{"idempotency_key": "upgrade-once"},
	}, billingUpgradeToolName))
	if call.Code != http.StatusOK || !strings.Contains(call.Body.String(), hostedTestIntent) ||
		stub.calls != 1 || stub.input.IdempotencyKey != "upgrade-once" ||
		stub.caller.Principal.OrgID != principal.OrgID || quotaGate.preAuthCalls != 0 {
		t.Fatalf("hosted call=%d %s; stub=%+v", call.Code, call.Body.String(), stub)
	}
}

func TestHostedUpgradeRejectsCallerAuthorityAndUntrustedResult(t *testing.T) {
	caller := BillingCaller{Principal: activeBillingPrincipal("nerve:billing.subscribe")}
	base := "https://nerve.example"
	for _, raw := range []string{
		`{"idempotency_key":"once","org_id":"foreign"}`,
		`{"idempotency_key":"once","offer_id":"scale"}`,
		`{"idempotency_key":"once","customer_id":"cus_other"}`,
		`{"idempotency_key":"once","idempotency_key":"twice"}`,
		`{"idempotency_key":"once"}{"idempotency_key":"twice"}`,
		`{"idempotency_key":""}`,
		`{"idempotency_key":"` + strings.Repeat("a", 1025) + `"}`,
	} {
		stub := &recordingHostedBilling{result: hostedBillingResult()}
		_, err := invokeBillingUpgradeTool(context.Background(), stub, caller, json.RawMessage(raw), base)
		var business *BillingBusinessError
		if !errors.As(err, &business) || business.Code != BillingErrorInvalidRequest || stub.calls != 0 {
			t.Fatalf("caller authority reached hosted provider: %q err=%v calls=%d", raw, err, stub.calls)
		}
	}
	for _, target := range []string{
		"https://evil.example/billing/upgrade?intent=" + hostedTestIntent,
		"http://nerve.example/billing/upgrade?intent=" + hostedTestIntent,
		"https://nerve.example/billing/upgrade?intent=not-a-uuid",
		"https://nerve.example/billing/upgrade?intent=" + hostedTestIntent + "&org_id=foreign",
		"https://nerve.example/billing/upgrade?intent=" + hostedTestIntent + "#token",
	} {
		stub := &recordingHostedBilling{result: hostedBillingResult()}
		stub.result.UpgradeURL = target
		_, err := invokeBillingUpgradeTool(context.Background(), stub, caller,
			json.RawMessage(`{"idempotency_key":"once"}`), base)
		var business *BillingBusinessError
		if !errors.As(err, &business) || business.Code != BillingErrorTemporarilyUnavailable ||
			strings.Contains(err.Error(), target) {
			t.Fatalf("unsafe provider URL escaped or was accepted: %q err=%v", target, err)
		}
	}
}

func TestHostedStatusIsScopedAndRejectsCallerAuthority(t *testing.T) {
	cfg := hostedRouterConfig()
	cfg.Cloud.DashboardBaseURL = "https://nerve.example"
	runtime := NewServer(cfg, nil, auth.NewService(cfg, nil), nil)
	quotaGate := &fakeEntitlementGate{preAuthErr: entitlements.ErrQuotaExceeded}
	runtime.Entitlements = quotaGate
	stub := &recordingHostedBilling{}
	runtime.HostedBilling = stub
	principal := activeBillingPrincipal("nerve:billing.subscribe")
	if !billingStatusToolAvailable(runtime, principal) ||
		billingStatusToolAvailable(runtime, activeBillingPrincipal("nerve:email.read")) {
		t.Fatal("hosted status ignored principal scope")
	}
	for _, arguments := range []string{`{"org_id":"foreign"}`, `{"generation":8}`,
		`{"customer_id":"cus_other"}`, `{} {}`} {
		_, err := invokeBillingStatusTool(context.Background(), stub,
			BillingCaller{Principal: principal}, json.RawMessage(arguments))
		var business *BillingBusinessError
		if !errors.As(err, &business) || business.Code != BillingErrorInvalidRequest || stub.calls != 0 {
			t.Fatalf("caller authority reached status provider: %q err=%v calls=%d", arguments, err, stub.calls)
		}
	}
	handler := NewSDKHandler(runtime, true)
	call := httptest.NewRecorder()
	handler.ServeHTTP(call, billingModernRequest(t, principal, "tools/call", map[string]any{
		"_meta": modernOAuthMeta(), "name": billingStatusToolName,
		"arguments": map[string]any{},
	}, billingStatusToolName))
	if call.Code != http.StatusOK || !strings.Contains(call.Body.String(), `"starter_active":false`) ||
		stub.calls != 1 || stub.caller.Principal.OrgID != principal.OrgID || quotaGate.preAuthCalls != 0 {
		t.Fatalf("hosted status call=%d %s; stub=%+v", call.Code, call.Body.String(), stub)
	}
}

func TestHostedBillingModernCallsRejectLocalIdentity(t *testing.T) {
	cfg := hostedRouterConfig()
	cfg.Cloud.DashboardBaseURL = "https://nerve.example"
	runtime := NewServer(cfg, nil, auth.NewService(cfg, nil), nil)
	stub := &recordingHostedBilling{result: hostedBillingResult()}
	runtime.HostedBilling = stub
	handler := NewSDKHandler(runtime, true)
	principal := activeBillingPrincipal("nerve:billing.subscribe")
	for _, item := range []struct {
		name string
		args map[string]any
	}{
		{billingUpgradeToolName, map[string]any{"idempotency_key": "once"}},
		{billingStatusToolName, map[string]any{}},
	} {
		request := billingModernRequest(t, principal, "tools/call", map[string]any{
			"_meta": modernOAuthMeta(), "name": item.name, "arguments": item.args,
		}, item.name)
		request = request.WithContext(localauth.WithIdentity(request.Context(), localauth.Identity{}))
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if stub.calls != 0 || !strings.Contains(recorder.Body.String(), `"isError":true`) {
			t.Fatalf("local identity reached %s: calls=%d status=%d body=%s", item.name,
				stub.calls, recorder.Code, recorder.Body.String())
		}
	}
}

type hostedStatusResultStub struct {
	recordingHostedBilling
	statusResult BillingStatusResult
}

func (s *hostedStatusResultStub) BillingStatus(_ context.Context, caller BillingCaller) (BillingStatusResult, error) {
	s.caller = caller
	s.calls++
	return s.statusResult, nil
}
func TestHostedStatusNativeAndDescriptorRequireConsistentPaidTier(t *testing.T) {
	for _, test := range []struct {
		name, state, tier string
		starter, valid    bool
	}{
		{"unpaid", "session_open", "", false, true},
		{"expired paid period", "active", "", false, true},
		{"Starter", "active", "starter", true, true},
		{"Growth", "active", "growth", false, true},
		{"Scale", "active", "scale", false, true},
		{"unproved Starter", "active", "", true, false},
		{"wrong Starter flag", "active", "growth", true, false},
		{"missing Starter flag", "active", "starter", false, false},
		{"unpaid Growth", "session_open", "growth", false, false},
		{"closed Scale", "terminal", "scale", false, false},
		{"unknown tier", "active", "legacy", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			stub := &hostedStatusResultStub{statusResult: BillingStatusResult{ResultType: "complete", HostedState: test.state, StarterActive: test.starter, ActiveTier: test.tier}}
			got, err := invokeBillingStatusTool(context.Background(), stub, BillingCaller{Principal: activeBillingPrincipal("nerve:billing.subscribe")}, json.RawMessage(`{}`))
			if (err == nil) != test.valid || stub.calls != 1 {
				t.Fatalf("result=%+v err=%v calls=%d", got, err, stub.calls)
			}
			if test.valid {
				encoded, err := json.Marshal(got)
				if err != nil || !strings.Contains(string(encoded), `"active_tier":`) {
					t.Fatalf("missing mandatory paid tier: %s %v", encoded, err)
				}
			} else {
				var business *BillingBusinessError
				if !errors.As(err, &business) || business.Code != BillingErrorTemporarilyUnavailable {
					t.Fatalf("invalid result escaped business boundary: %v", err)
				}
			}
		})
	}
	descriptor, _ := json.Marshal(billingStatusToolDescriptor())
	if !strings.Contains(string(descriptor), `"active_tier"`) {
		t.Fatalf("descriptor omitted paid tier %s", descriptor)
	}
}

func TestHostedStatusTierChangePreservesCurrentPaidAuthority(t *testing.T) {
	states := []string{"awaiting_owner", "attempt_prepared", "provider_unknown", "pending_payment", "operator_review", "applied", "terminal"}
	for _, offer := range []struct{ id, tier string }{{"growth_2026_09_v2", "growth"}, {"scale_2026_09_v2", "scale"}} {
		for _, state := range states {
			for _, tier := range []string{"", "starter", "growth", "scale"} {
				t.Run(offer.tier+"/"+state+"/"+tier, func(t *testing.T) {
					result := BillingStatusResult{ResultType: "complete", HostedState: "active", ActiveTier: tier, StarterActive: tier == "starter", TierChangeState: state, TierChangeOfferID: offer.id}
					valid := tier == "" || tier == "starter"
					if state == "applied" {
						valid = tier == "" || tier == offer.tier
					}
					if state == "terminal" {
						valid = true
					}
					stub := &hostedStatusResultStub{statusResult: result}
					got, err := invokeBillingStatusTool(context.Background(), stub, BillingCaller{Principal: activeBillingPrincipal("nerve:billing.subscribe")}, json.RawMessage(`{}`))
					if (err == nil) != valid || stub.calls != 1 {
						t.Fatalf("got=%+v err=%v expectedValid=%v", got, err, valid)
					}
					if valid && got != result {
						t.Fatalf("status replaced paid authority: %+v", got)
					}
				})
			}
		}
	}
	for _, pair := range []struct{ state, offer string }{{"pending_payment", ""}, {"", "growth_2026_09_v2"}, {"unknown", "growth_2026_09_v2"}, {"pending_payment", "starter_2026_09_v2"}, {"pending_payment", "scale"}} {
		result := BillingStatusResult{ResultType: "complete", HostedState: "active", ActiveTier: "starter", StarterActive: true, TierChangeState: pair.state, TierChangeOfferID: pair.offer}
		if ValidHostedBillingStatus(result) {
			t.Fatalf("accepted incomplete or unknown pair: %+v", result)
		}
	}
	body, _ := json.Marshal(BillingStatusResult{ResultType: "complete", HostedState: "none"})
	if strings.Contains(string(body), "tier_change_") {
		t.Fatalf("missing decision emitted fields: %s", body)
	}
	descriptor, _ := json.Marshal(billingStatusToolDescriptor())
	for _, field := range []string{"tier_change_state", "tier_change_offer_id", "dependentRequired"} {
		if !strings.Contains(string(descriptor), field) {
			t.Fatalf("schema missing field %s", field)
		}
	}
}

func TestHostedStatusTierChangeNativeOutputRemainsScopedRead(t *testing.T) {
	for _, tc := range []struct {
		state, offer, tier string
		valid              bool
	}{
		{"pending_payment", "growth_2026_09_v2", "starter", true},
		{"operator_review", "scale_2026_09_v2", "", true},
		{"applied", "scale_2026_09_v2", "scale", true},
		{"pending_payment", "growth_2026_09_v2", "growth", false},
		{"applied", "scale_2026_09_v2", "growth", false},
	} {
		t.Run(tc.state+"/"+tc.tier, func(t *testing.T) {
			cfg := hostedRouterConfig()
			runtime := NewServer(cfg, nil, auth.NewService(cfg, nil), nil)
			gate := &fakeEntitlementGate{preAuthErr: entitlements.ErrQuotaExceeded}
			runtime.Entitlements = gate
			stub := &hostedStatusResultStub{statusResult: BillingStatusResult{ResultType: "complete", HostedState: "active", StarterActive: tc.tier == "starter", ActiveTier: tc.tier, TierChangeState: tc.state, TierChangeOfferID: tc.offer}}
			runtime.HostedBilling = stub
			principal := activeBillingPrincipal("nerve:billing.subscribe")
			request := billingModernRequest(t, principal, "tools/call", map[string]any{"_meta": modernOAuthMeta(), "name": billingStatusToolName, "arguments": map[string]any{}}, billingStatusToolName)
			recorder := httptest.NewRecorder()
			NewSDKHandler(runtime, true).ServeHTTP(recorder, request)
			body := recorder.Body.String()
			if stub.calls != 1 || gate.preAuthCalls != 0 || strings.Contains(body, `"isError":true`) == tc.valid {
				t.Fatalf("calls=%d quota=%d response=%s", stub.calls, gate.preAuthCalls, body)
			}
			if tc.valid && (!strings.Contains(body, `"tier_change_state":"`+tc.state+`"`) || !strings.Contains(body, `"active_tier":"`+tc.tier+`"`)) {
				t.Fatalf("scoped read lost status: %s", body)
			}
		})
	}
}
