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
