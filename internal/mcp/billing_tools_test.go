package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"neuralmail/internal/auth"
)

type recordingBillingProvisioner struct {
	caller BillingCaller
	input  BillingSubscribeInput
	calls  int
	result *BillingSubscribeResult
	err    error
}

func (provisioner *recordingBillingProvisioner) Subscribe(
	_ context.Context,
	caller BillingCaller,
	input BillingSubscribeInput,
) (BillingSubscribeResult, error) {
	provisioner.caller = caller
	provisioner.input = input
	provisioner.calls++
	if provisioner.result != nil {
		return *provisioner.result, provisioner.err
	}
	return BillingSubscribeResult{
		ResultType: "complete", State: BillingSubscribeActive,
		PlanCode: input.PlanCode, ComposeEnabled: true,
	}, provisioner.err
}

func activeBillingPrincipal(scopes ...string) auth.Principal {
	return auth.Principal{
		Kind: auth.PrincipalM2MOrg, OrgID: "org-1", ClientID: "client-1", Generation: 7,
		TokenID: "token-1", Scopes: scopes, AuthMethod: "m2m_bearer",
	}
}

func TestBillingToolRegistersOnlyForConfiguredScopedM2MOrgAndDelegatesAuthority(t *testing.T) {
	cfg := hostedRouterConfig()
	runtime := NewServer(cfg, nil, auth.NewService(cfg, nil), nil)
	provisioner := &recordingBillingProvisioner{}
	runtime.Billing = provisioner
	principal := activeBillingPrincipal("nerve:billing.subscribe")
	handler := NewSDKHandler(runtime, true)

	listRecorder := httptest.NewRecorder()
	handler.ServeHTTP(listRecorder, billingModernRequest(t, principal, "tools/list", map[string]any{
		"_meta": modernOAuthMeta(),
	}, ""))
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("tools/list status=%d body=%s", listRecorder.Code, listRecorder.Body.String())
	}
	var listed struct {
		Result struct {
			Tools []struct {
				Name         string         `json:"name"`
				InputSchema  map[string]any `json:"inputSchema"`
				OutputSchema map[string]any `json:"outputSchema"`
			} `json:"tools"`
			CacheScope string `json:"cacheScope"`
		} `json:"result"`
	}
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode tools/list: %v body=%s", err, listRecorder.Body.String())
	}
	if listed.Result.CacheScope != "private" || len(listed.Result.Tools) != 1 || listed.Result.Tools[0].Name != billingSubscribeToolName {
		t.Fatalf("billing tools/list=%+v", listed.Result)
	}
	descriptor := listed.Result.Tools[0]
	properties := descriptor.InputSchema["properties"].(map[string]any)
	if len(properties) != 2 || properties["plan_code"] == nil || properties["idempotency_key"] == nil ||
		descriptor.InputSchema["additionalProperties"] != false {
		t.Fatalf("billing input schema=%v", descriptor.InputSchema)
	}
	oneOf := descriptor.OutputSchema["oneOf"].([]any)
	resultShape := oneOf[0].(map[string]any)
	resultProperties := resultShape["properties"].(map[string]any)
	stateValues := resultProperties["state"].(map[string]any)["enum"].([]any)
	if len(stateValues) != 4 || resultProperties["compose_enabled"] == nil || resultProperties["retry_at"] == nil {
		t.Fatalf("billing result schema=%v", resultShape)
	}
	errorShape := oneOf[1].(map[string]any)
	errorProperties := errorShape["properties"].(map[string]any)["error"].(map[string]any)["properties"].(map[string]any)
	errorCodes := errorProperties["code"].(map[string]any)["enum"].([]any)
	if len(errorCodes) != len(billingBusinessErrorCodes()) {
		t.Fatalf("billing error schema=%v", errorProperties["code"])
	}

	callRecorder := httptest.NewRecorder()
	handler.ServeHTTP(callRecorder, billingModernRequest(t, principal, "tools/call", map[string]any{
		"_meta": modernOAuthMeta(), "name": billingSubscribeToolName,
		"arguments": map[string]any{"plan_code": "pro-v1", "idempotency_key": "subscribe-once"},
	}, billingSubscribeToolName))
	if callRecorder.Code != http.StatusOK || bytes.Contains(callRecorder.Body.Bytes(), []byte(`"isError":true`)) {
		t.Fatalf("billing call status=%d body=%s", callRecorder.Code, callRecorder.Body.String())
	}
	if provisioner.calls != 1 || provisioner.input != (BillingSubscribeInput{PlanCode: "pro-v1", IdempotencyKey: "subscribe-once"}) {
		t.Fatalf("billing provisioner calls/input=%d/%+v", provisioner.calls, provisioner.input)
	}
	if provisioner.caller.Authorization != "Bearer original-token" ||
		provisioner.caller.Principal.Kind != principal.Kind || provisioner.caller.Principal.ClientID != principal.ClientID ||
		provisioner.caller.Principal.OrgID != principal.OrgID || provisioner.caller.Principal.Generation != principal.Generation ||
		!slices.Equal(provisioner.caller.Principal.Scopes, principal.Scopes) {
		t.Fatalf("billing caller=%+v", provisioner.caller)
	}
	for _, forbidden := range []string{"client_id", "org_id", "generation", "onboarding_id", "customer", "payment_method", "mandate", "client_secret", "checkout"} {
		if strings.Contains(callRecorder.Body.String(), forbidden) {
			t.Fatalf("billing result exposed %q: %s", forbidden, callRecorder.Body.String())
		}
	}
}

func TestBillingToolIsHiddenAndDeniedOutsideExactProfile(t *testing.T) {
	tests := []struct {
		name       string
		principal  auth.Principal
		configured bool
		wantStatus int
	}{
		{name: "nil provisioner", principal: activeBillingPrincipal("nerve:billing.subscribe"), wantStatus: http.StatusBadRequest},
		{name: "missing scope", principal: activeBillingPrincipal("nerve:email.read"), configured: true, wantStatus: http.StatusForbidden},
		{name: "wrong kind", principal: auth.Principal{Kind: auth.PrincipalLegacyJWT, OrgID: "org-1", ClientID: "client-1", Generation: 7, Scopes: []string{"nerve:billing.subscribe"}, AuthMethod: "jwt"}, configured: true, wantStatus: http.StatusBadRequest},
		{name: "missing active binding", principal: auth.Principal{Kind: auth.PrincipalM2MOrg, ClientID: "client-1", Generation: 7, Scopes: []string{"nerve:billing.subscribe"}, AuthMethod: "m2m_bearer"}, configured: true, wantStatus: http.StatusBadRequest},
		{name: "wrong auth method", principal: auth.Principal{Kind: auth.PrincipalM2MOrg, OrgID: "org-1", ClientID: "client-1", Generation: 7, Scopes: []string{"nerve:billing.subscribe"}, AuthMethod: "jwt"}, configured: true, wantStatus: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := hostedRouterConfig()
			runtime := NewServer(cfg, nil, auth.NewService(cfg, nil), nil)
			provisioner := &recordingBillingProvisioner{}
			if test.configured {
				runtime.Billing = provisioner
			}
			handler := NewSDKHandler(runtime, true)
			listRecorder := httptest.NewRecorder()
			handler.ServeHTTP(listRecorder, billingModernRequest(t, test.principal, "tools/list", map[string]any{
				"_meta": modernOAuthMeta(),
			}, ""))
			if listRecorder.Code != http.StatusOK || strings.Contains(listRecorder.Body.String(), billingSubscribeToolName) {
				t.Fatalf("hidden billing list status=%d body=%s", listRecorder.Code, listRecorder.Body.String())
			}

			callRecorder := httptest.NewRecorder()
			handler.ServeHTTP(callRecorder, billingModernRequest(t, test.principal, "tools/call", map[string]any{
				"_meta": modernOAuthMeta(), "name": billingSubscribeToolName,
				"arguments": map[string]any{"plan_code": "pro", "idempotency_key": "subscribe-once"},
			}, billingSubscribeToolName))
			if callRecorder.Code != test.wantStatus {
				t.Fatalf("hidden billing call status=%d want=%d body=%s", callRecorder.Code, test.wantStatus, callRecorder.Body.String())
			}
			if provisioner.calls != 0 {
				t.Fatalf("hidden billing call reached provisioner %d times", provisioner.calls)
			}
		})
	}
}

func TestBillingProvisionerCannotBeReachedThroughLegacyInvoker(t *testing.T) {
	cfg := hostedRouterConfig()
	runtime := NewServer(cfg, nil, auth.NewService(cfg, nil), nil)
	provisioner := &recordingBillingProvisioner{}
	runtime.Billing = provisioner
	principal := auth.Principal{
		Kind: auth.PrincipalLegacyJWT, OrgID: "org-1", Scopes: []string{"*"}, AuthMethod: "jwt",
	}
	_, err := runtime.Invoker.Invoke(auth.WithPrincipal(context.Background(), principal), ToolInvocation{
		Name: billingSubscribeToolName, Arguments: json.RawMessage(`{"plan_code":"pro","idempotency_key":"key"}`),
	})
	if err == nil || !strings.Contains(err.Error(), "modern MCP protocol") {
		t.Fatalf("legacy billing invocation error=%v", err)
	}
	if provisioner.calls != 0 {
		t.Fatalf("legacy billing invocation reached provisioner %d times", provisioner.calls)
	}
}

func TestBillingToolRejectsUnboundedDuplicateAndAuthorityArguments(t *testing.T) {
	caller := BillingCaller{Principal: activeBillingPrincipal("nerve:billing.subscribe"), Authorization: "Bearer token"}
	tests := []struct {
		name      string
		arguments string
	}{
		{name: "empty plan", arguments: `{"plan_code":"","idempotency_key":"key"}`},
		{name: "oversized plan", arguments: `{"plan_code":"` + strings.Repeat("a", 65) + `","idempotency_key":"key"}`},
		{name: "uppercase plan", arguments: `{"plan_code":"Pro","idempotency_key":"key"}`},
		{name: "empty key", arguments: `{"plan_code":"pro","idempotency_key":""}`},
		{name: "oversized key", arguments: `{"plan_code":"pro","idempotency_key":"` + strings.Repeat("k", 129) + `"}`},
		{name: "trimmed key", arguments: `{"plan_code":"pro","idempotency_key":" key"}`},
		{name: "control key", arguments: `{"plan_code":"pro","idempotency_key":"key\u0000"}`},
		{name: "duplicate", arguments: `{"plan_code":"pro","plan_code":"basic","idempotency_key":"key"}`},
		{name: "scalar", arguments: `"pro"`},
		{name: "multiple values", arguments: `{"plan_code":"pro","idempotency_key":"key"} {}`},
	}
	for _, field := range []string{
		"client_id", "org_id", "generation", "onboarding_id", "stripe_customer_id",
		"payment_method_id", "mandate_id", "payment_intent", "checkout_url",
	} {
		tests = append(tests, struct {
			name      string
			arguments string
		}{name: "authority override " + field, arguments: `{"plan_code":"pro","idempotency_key":"key","` + field + `":"attacker"}`})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provisioner := &recordingBillingProvisioner{}
			_, err := invokeBillingTool(context.Background(), provisioner, caller, billingSubscribeToolName, json.RawMessage(test.arguments))
			var businessErr *BillingBusinessError
			if !errors.As(err, &businessErr) || businessErr.Code != BillingErrorInvalidRequest || businessErr.Retryable {
				t.Fatalf("billing argument error=%v", err)
			}
			if provisioner.calls != 0 {
				t.Fatalf("invalid arguments reached provisioner %d times", provisioner.calls)
			}
		})
	}

	valid := &recordingBillingProvisioner{}
	plan := strings.Repeat("a", 64)
	key := strings.Repeat("k", 128)
	if _, err := invokeBillingTool(context.Background(), valid, caller, billingSubscribeToolName,
		json.RawMessage(`{"plan_code":"`+plan+`","idempotency_key":"`+key+`"}`)); err != nil {
		t.Fatalf("exact input bounds rejected: %v", err)
	}
}

func TestBillingToolSanitizesProvisionerResultsAndErrors(t *testing.T) {
	caller := BillingCaller{Principal: activeBillingPrincipal("nerve:billing.subscribe"), Authorization: "Bearer token"}
	arguments := json.RawMessage(`{"plan_code":"pro","idempotency_key":"key"}`)
	valid := BillingSubscribeResult{ResultType: "complete", State: BillingSubscribeActive, PlanCode: "pro", ComposeEnabled: true}
	retryAt := time.Now().UTC().Add(time.Minute)
	for _, result := range []BillingSubscribeResult{
		valid,
		{ResultType: "complete", State: BillingSubscribeProcessing, PlanCode: "pro", RetryAt: &retryAt},
		{ResultType: "complete", State: BillingSubscribeProviderUnknown, PlanCode: "pro", RetryAt: &retryAt},
		{ResultType: "complete", State: BillingSubscribeRequiresAction, PlanCode: "pro"},
	} {
		provisioner := &recordingBillingProvisioner{result: &result}
		if _, err := invokeBillingTool(context.Background(), provisioner, caller, billingSubscribeToolName, arguments); err != nil {
			t.Fatalf("valid billing state %q rejected: %v", result.State, err)
		}
	}
	resultTests := []struct {
		name   string
		mutate func(*BillingSubscribeResult)
	}{
		{name: "result type", mutate: func(result *BillingSubscribeResult) { result.ResultType = "input_required" }},
		{name: "unknown state", mutate: func(result *BillingSubscribeResult) { result.State = "terminal" }},
		{name: "plan substitution", mutate: func(result *BillingSubscribeResult) { result.PlanCode = "enterprise" }},
		{name: "active without evidence", mutate: func(result *BillingSubscribeResult) { result.ComposeEnabled = false }},
		{name: "pending with compose", mutate: func(result *BillingSubscribeResult) { result.State = BillingSubscribeProcessing }},
		{name: "active retry", mutate: func(result *BillingSubscribeResult) { retryAt := time.Now().UTC(); result.RetryAt = &retryAt }},
	}
	for _, test := range resultTests {
		t.Run(test.name, func(t *testing.T) {
			invalid := valid
			test.mutate(&invalid)
			provisioner := &recordingBillingProvisioner{result: &invalid}
			_, err := invokeBillingTool(context.Background(), provisioner, caller, billingSubscribeToolName, arguments)
			var businessErr *BillingBusinessError
			if !errors.As(err, &businessErr) || businessErr.Code != BillingErrorTemporarilyUnavailable || !businessErr.Retryable {
				t.Fatalf("invalid result error=%v", err)
			}
		})
	}

	secret := errors.New("sk_live_SYNTHETIC_PROVIDER_SECRET")
	_, err := invokeBillingTool(context.Background(), &recordingBillingProvisioner{err: secret}, caller, billingSubscribeToolName, arguments)
	var sanitized *BillingBusinessError
	if !errors.As(err, &sanitized) || sanitized.Code != BillingErrorTemporarilyUnavailable || strings.Contains(err.Error(), "SYNTHETIC") {
		t.Fatalf("generic billing error was not sanitized: %v", err)
	}
	_, err = invokeBillingTool(context.Background(), &recordingBillingProvisioner{err: &BillingBusinessError{Code: "sk_live_SYNTHETIC_PROVIDER_SECRET"}}, caller, billingSubscribeToolName, arguments)
	if !errors.As(err, &sanitized) || sanitized.Code != BillingErrorTemporarilyUnavailable || strings.Contains(err.Error(), "SYNTHETIC") {
		t.Fatalf("unknown billing business error was reflected: %v", err)
	}
	_, err = invokeBillingTool(context.Background(), &recordingBillingProvisioner{err: &BillingBusinessError{Code: BillingErrorInvalidRequest, Retryable: true}}, caller, billingSubscribeToolName, arguments)
	if !errors.As(err, &sanitized) || sanitized.Code != BillingErrorTemporarilyUnavailable {
		t.Fatalf("inconsistent billing business error was reflected: %v", err)
	}
	public := &BillingBusinessError{Code: BillingErrorRateLimited, Retryable: true, RetryAt: &retryAt}
	_, err = invokeBillingTool(context.Background(), &recordingBillingProvisioner{err: public}, caller, billingSubscribeToolName, arguments)
	if err != public {
		t.Fatalf("public billing error changed: got=%v want=%v", err, public)
	}
	translated := translateModernBusinessError(err)
	if translated.Code != BillingErrorRateLimited || !translated.Retryable || translated.RetryAt != retryAt.Format(time.RFC3339) {
		t.Fatalf("translated billing error=%+v", translated)
	}

	_, err = invokeBillingTool(context.Background(), nil, caller, billingSubscribeToolName, arguments)
	if !errors.As(err, &sanitized) || sanitized.Code != BillingErrorTemporarilyUnavailable {
		t.Fatalf("nil billing provisioner error=%v", err)
	}
}

func TestBillingHTTPFailureStaysInsideAdvertisedClosedContract(t *testing.T) {
	invalidYear := time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC)
	invalidZone := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.FixedZone("invalid", 24*60*60))
	tests := []struct {
		name        string
		provisioner *recordingBillingProvisioner
		secret      string
	}{
		{name: "unknown provider error", provisioner: &recordingBillingProvisioner{err: errors.New("sk_live_SYNTHETIC_PROVIDER_SECRET")}, secret: "SYNTHETIC_PROVIDER_SECRET"},
		{name: "unencodable result year", provisioner: &recordingBillingProvisioner{result: &BillingSubscribeResult{ResultType: "complete", State: BillingSubscribeProcessing, PlanCode: "pro", RetryAt: &invalidYear}}},
		{name: "unencodable result zone", provisioner: &recordingBillingProvisioner{result: &BillingSubscribeResult{ResultType: "complete", State: BillingSubscribeProviderUnknown, PlanCode: "pro", RetryAt: &invalidZone}}},
		{name: "unencodable public error", provisioner: &recordingBillingProvisioner{err: &BillingBusinessError{Code: BillingErrorRateLimited, Retryable: true, RetryAt: &invalidYear}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := hostedRouterConfig()
			runtime := NewServer(cfg, nil, auth.NewService(cfg, nil), nil)
			runtime.Billing = test.provisioner
			principal := activeBillingPrincipal("nerve:billing.subscribe")
			recorder := httptest.NewRecorder()
			NewSDKHandler(runtime, true).ServeHTTP(recorder, billingModernRequest(t, principal, "tools/call", map[string]any{
				"_meta": modernOAuthMeta(), "name": billingSubscribeToolName,
				"arguments": map[string]any{"plan_code": "pro", "idempotency_key": "key"},
			}, billingSubscribeToolName))
			if recorder.Code != http.StatusOK || !bytes.Contains(recorder.Body.Bytes(), []byte(`"isError":true`)) ||
				!bytes.Contains(recorder.Body.Bytes(), []byte(BillingErrorTemporarilyUnavailable)) ||
				(test.secret != "" && bytes.Contains(recorder.Body.Bytes(), []byte(test.secret))) {
				t.Fatalf("billing error escaped contract: status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func billingModernRequest(t *testing.T, principal auth.Principal, method string, params map[string]any, name string) *http.Request {
	t.Helper()
	request := modernContractRequest(t, method, params)
	request.Header.Set("Authorization", "Bearer original-token")
	if name != "" {
		request.Header.Set("Mcp-Name", name)
	}
	return request.WithContext(auth.WithPrincipal(request.Context(), principal))
}
