package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"neuralmail/internal/auth"
)

type recordingOnboardingProvisioner struct {
	operation string
	caller    OnboardingCaller
	start     OnboardingStartInput
	close     OnboardingCloseInput
	calls     int
	result    *OnboardingResult
	err       error
}

type rejectOnboardingFeatureLookup struct {
	t *testing.T
}

func (gate rejectOnboardingFeatureLookup) Enabled(_ context.Context, flag string, orgID string) (bool, error) {
	gate.t.Helper()
	gate.t.Fatalf("onboarding profile queried org feature flag %q for org %q", flag, orgID)
	return false, nil
}

func (provisioner *recordingOnboardingProvisioner) record(operation string, caller OnboardingCaller) OnboardingResult {
	provisioner.operation = operation
	provisioner.caller = caller
	provisioner.calls++
	if provisioner.result != nil {
		return *provisioner.result
	}
	return OnboardingResult{
		ResultType: "complete", OnboardingID: "11111111-1111-4111-8111-111111111111", Generation: caller.Principal.Generation,
		State: "provisioning", Mode: OnboardingMailboxManaged,
	}
}

func (provisioner *recordingOnboardingProvisioner) Start(_ context.Context, caller OnboardingCaller, input OnboardingStartInput) (OnboardingResult, error) {
	provisioner.start = input
	return provisioner.record("start", caller), provisioner.err
}

func (provisioner *recordingOnboardingProvisioner) Status(_ context.Context, caller OnboardingCaller) (OnboardingResult, error) {
	return provisioner.record("status", caller), provisioner.err
}

func (provisioner *recordingOnboardingProvisioner) VerifyDomain(_ context.Context, caller OnboardingCaller) (OnboardingResult, error) {
	return provisioner.record("verify_domain", caller), provisioner.err
}

func (provisioner *recordingOnboardingProvisioner) Close(_ context.Context, caller OnboardingCaller, input OnboardingCloseInput) (OnboardingResult, error) {
	provisioner.close = input
	return provisioner.record("close", caller), provisioner.err
}

func TestSDKServerRegistersOnlyFourOnboardingToolsAndPreservesBearer(t *testing.T) {
	cfg := hostedRouterConfig()
	runtime := NewServer(cfg, nil, auth.NewService(cfg, nil), nil)
	provisioner := &recordingOnboardingProvisioner{}
	runtime.Onboarding = provisioner
	runtime.FeatureFlags = rejectOnboardingFeatureLookup{t: t}
	handler := NewSDKHandler(runtime, true)
	principal := auth.Principal{
		Kind: auth.PrincipalM2MOnboarding, ClientID: "client-1", Generation: 7, TokenID: "token-1",
		Scopes: []string{"nerve:onboarding"}, AuthMethod: "m2m_bearer",
	}

	listRequest := onboardingModernRequest(t, principal, "tools/list", map[string]any{"_meta": modernOAuthMeta()}, "")
	listRecorder := httptest.NewRecorder()
	handler.ServeHTTP(listRecorder, listRequest)
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("tools/list status=%d body=%s", listRecorder.Code, listRecorder.Body.String())
	}
	var listed struct {
		Result struct {
			Tools []struct {
				Name         string         `json:"name"`
				OutputSchema map[string]any `json:"outputSchema"`
			} `json:"tools"`
			CacheScope string `json:"cacheScope"`
		} `json:"result"`
	}
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode tools/list: %v body=%s", err, listRecorder.Body.String())
	}
	wantNames := []string{"nerve_onboarding_close", "nerve_onboarding_start", "nerve_onboarding_status", "nerve_onboarding_verify_domain"}
	gotNames := make([]string, 0, len(listed.Result.Tools))
	for _, tool := range listed.Result.Tools {
		gotNames = append(gotNames, tool.Name)
		oneOf, ok := tool.OutputSchema["oneOf"].([]any)
		if !ok || len(oneOf) != 2 {
			t.Fatalf("%s output schema=%v", tool.Name, tool.OutputSchema)
		}
		resultShape, ok := oneOf[0].(map[string]any)
		if !ok {
			t.Fatalf("%s result output shape=%v", tool.Name, oneOf[0])
		}
		properties, ok := resultShape["properties"].(map[string]any)
		if !ok || properties["onboarding_id"].(map[string]any)["format"] != "uuid" {
			t.Fatalf("%s onboarding_id output schema=%v", tool.Name, properties["onboarding_id"])
		}
		state := properties["state"].(map[string]any)
		if states, ok := state["enum"].([]any); !ok || len(states) != 5 {
			t.Fatalf("%s state output schema=%v", tool.Name, state)
		}
		errorShape, ok := oneOf[1].(map[string]any)
		if !ok {
			t.Fatalf("%s error output shape=%v", tool.Name, oneOf[1])
		}
		errorProperties := errorShape["properties"].(map[string]any)["error"].(map[string]any)["properties"].(map[string]any)
		codes, ok := errorProperties["code"].(map[string]any)["enum"].([]any)
		if !ok || len(codes) != len(onboardingBusinessErrorCodes()) {
			t.Fatalf("%s closed error-code schema=%v", tool.Name, errorProperties["code"])
		}
	}
	if !slices.Equal(gotNames, wantNames) {
		t.Fatalf("onboarding tools=%v want=%v", gotNames, wantNames)
	}
	if listed.Result.CacheScope != "private" {
		t.Fatalf("tools/list cacheScope=%q want=private", listed.Result.CacheScope)
	}
	withoutScope := principal
	withoutScope.Scopes = nil
	noScopeRequest := onboardingModernRequest(t, withoutScope, "tools/list", map[string]any{"_meta": modernOAuthMeta()}, "")
	noScopeRecorder := httptest.NewRecorder()
	handler.ServeHTTP(noScopeRecorder, noScopeRequest)
	if noScopeRecorder.Code != http.StatusOK || strings.Contains(noScopeRecorder.Body.String(), "nerve_onboarding_") {
		t.Fatalf("scope-less onboarding principal listed lifecycle tools: status=%d body=%s", noScopeRecorder.Code, noScopeRecorder.Body.String())
	}

	operations := []struct {
		name      string
		operation string
		arguments map[string]any
	}{
		{name: "nerve_onboarding_start", operation: "start", arguments: map[string]any{
			"idempotency_key": "start-1", "organization_name": "  Example   Org  ", "mailbox_mode": "managed_mailbox",
		}},
		{name: "nerve_onboarding_status", operation: "status", arguments: map[string]any{}},
		{name: "nerve_onboarding_verify_domain", operation: "verify_domain", arguments: map[string]any{}},
		{name: "nerve_onboarding_close", operation: "close", arguments: map[string]any{
			"idempotency_key": "close-1", "expected_generation": 7,
		}},
	}
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			request := onboardingModernRequest(t, principal, "tools/call", map[string]any{
				"_meta": modernOAuthMeta(), "name": operation.name, "arguments": operation.arguments,
			}, operation.name)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("call status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if provisioner.operation != operation.operation {
				t.Fatalf("operation=%q want=%q", provisioner.operation, operation.operation)
			}
			gotPrincipal := provisioner.caller.Principal
			if gotPrincipal.Kind != principal.Kind || gotPrincipal.ClientID != principal.ClientID ||
				gotPrincipal.Generation != principal.Generation || gotPrincipal.TokenID != principal.TokenID ||
				!slices.Equal(gotPrincipal.Scopes, principal.Scopes) || provisioner.caller.Authorization != "Bearer original-token" {
				t.Fatalf("caller=%+v", provisioner.caller)
			}
		})
	}
	if provisioner.start.OrganizationName != "Example Org" {
		t.Fatalf("normalized organization=%q", provisioner.start.OrganizationName)
	}
	if provisioner.close.ExpectedGeneration != principal.Generation {
		t.Fatalf("close input=%+v", provisioner.close)
	}
}

func TestOnboardingHTTPFailuresStayInsideAdvertisedErrorSchema(t *testing.T) {
	cfg := hostedRouterConfig()
	runtime := NewServer(cfg, nil, auth.NewService(cfg, nil), nil)
	provisioner := &recordingOnboardingProvisioner{}
	runtime.Onboarding = provisioner
	handler := NewSDKHandler(runtime, true)
	principal := auth.Principal{
		Kind: auth.PrincipalM2MOnboarding, ClientID: "client-1", Generation: 7, TokenID: "token-1",
		Scopes: []string{"nerve:onboarding"}, AuthMethod: "m2m_bearer",
	}

	listRequest := onboardingModernRequest(t, principal, "tools/list", map[string]any{"_meta": modernOAuthMeta()}, "")
	listRecorder := httptest.NewRecorder()
	handler.ServeHTTP(listRecorder, listRequest)
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("tools/list status=%d body=%s", listRecorder.Code, listRecorder.Body.String())
	}
	var listed struct {
		Result struct {
			Tools []struct {
				Name         string         `json:"name"`
				OutputSchema map[string]any `json:"outputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode tools/list: %v body=%s", err, listRecorder.Body.String())
	}
	advertised := make(map[string]map[string]struct{}, len(listed.Result.Tools))
	for _, tool := range listed.Result.Tools {
		oneOf := tool.OutputSchema["oneOf"].([]any)
		errorShape := oneOf[1].(map[string]any)
		errorProperties := errorShape["properties"].(map[string]any)["error"].(map[string]any)["properties"].(map[string]any)
		codes := errorProperties["code"].(map[string]any)["enum"].([]any)
		advertised[tool.Name] = make(map[string]struct{}, len(codes))
		for _, code := range codes {
			advertised[tool.Name][code.(string)] = struct{}{}
		}
	}

	tests := []struct {
		name           string
		tool           string
		arguments      map[string]any
		provisionerErr error
		wantCode       string
		wantRetryable  bool
	}{
		{name: "start generic", tool: "nerve_onboarding_start", arguments: managedStartArguments("start-1", "Example"), provisionerErr: errors.New("SYNTHETIC_PROVIDER_SECRET"), wantCode: OnboardingErrorTemporarilyUnavailable, wantRetryable: true},
		{name: "status generic", tool: "nerve_onboarding_status", arguments: map[string]any{}, provisionerErr: errors.New("SYNTHETIC_PROVIDER_SECRET"), wantCode: OnboardingErrorTemporarilyUnavailable, wantRetryable: true},
		{name: "verify generic", tool: "nerve_onboarding_verify_domain", arguments: map[string]any{}, provisionerErr: errors.New("SYNTHETIC_PROVIDER_SECRET"), wantCode: OnboardingErrorTemporarilyUnavailable, wantRetryable: true},
		{name: "close generic", tool: "nerve_onboarding_close", arguments: map[string]any{"idempotency_key": "close-1", "expected_generation": 7}, provisionerErr: errors.New("SYNTHETIC_PROVIDER_SECRET"), wantCode: OnboardingErrorTemporarilyUnavailable, wantRetryable: true},
		{name: "start semantic", tool: "nerve_onboarding_start", arguments: managedStartArguments("start-1", "   "), wantCode: OnboardingErrorInvalidRequest},
		{name: "close semantic", tool: "nerve_onboarding_close", arguments: map[string]any{"idempotency_key": "close-1", "expected_generation": 8}, wantCode: OnboardingErrorInvalidRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provisioner.err = test.provisionerErr
			request := onboardingModernRequest(t, principal, "tools/call", map[string]any{
				"_meta": modernOAuthMeta(), "name": test.tool, "arguments": test.arguments,
			}, test.tool)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("call status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			var response struct {
				Result struct {
					IsError           bool `json:"isError"`
					StructuredContent struct {
						Error modernBusinessError `json:"error"`
					} `json:"structuredContent"`
				} `json:"result"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode call: %v body=%s", err, recorder.Body.String())
			}
			got := response.Result.StructuredContent.Error
			if !response.Result.IsError || got.Code != test.wantCode || got.Retryable != test.wantRetryable {
				t.Fatalf("structured failure=%+v isError=%t body=%s", got, response.Result.IsError, recorder.Body.String())
			}
			if _, ok := advertised[test.tool][got.Code]; !ok {
				t.Fatalf("tool %s returned unadvertised code %q", test.tool, got.Code)
			}
			if strings.Contains(recorder.Body.String(), "SYNTHETIC_PROVIDER_SECRET") {
				t.Fatalf("provider detail escaped response: %s", recorder.Body.String())
			}
		})
	}
}

func TestOnboardingToolBoundaryNormalizesAndRejectsAuthorityOverrides(t *testing.T) {
	principal := auth.Principal{Kind: auth.PrincipalM2MOnboarding, ClientID: "client-1", Generation: 7}
	caller := OnboardingCaller{Principal: principal, Authorization: "Bearer token"}
	provisioner := &recordingOnboardingProvisioner{}
	arguments := json.RawMessage(`{"idempotency_key":"custom-1","organization_name":"Example","mailbox_mode":"custom_domain","custom_domain":"MAIL.Example.COM.","local_part":"Agent.One"}`)
	if _, err := invokeOnboardingTool(context.Background(), provisioner, caller, "nerve_onboarding_start", arguments); err != nil {
		t.Fatalf("custom-domain start: %v", err)
	}
	if provisioner.start.CustomDomain != "mail.example.com" || provisioner.start.LocalPart != "agent.one" {
		t.Fatalf("normalized custom input=%+v", provisioner.start)
	}

	invalid := []struct {
		name      string
		tool      string
		arguments string
	}{
		{name: "unknown authority field", tool: "nerve_onboarding_start", arguments: `{"idempotency_key":"start-1","organization_name":"Example","mailbox_mode":"managed_mailbox","client_id":"other"}`},
		{name: "managed custom field", tool: "nerve_onboarding_start", arguments: `{"idempotency_key":"start-1","organization_name":"Example","mailbox_mode":"managed_mailbox","custom_domain":"example.com"}`},
		{name: "generation mismatch", tool: "nerve_onboarding_close", arguments: `{"idempotency_key":"close-1","expected_generation":8}`},
		{name: "status override", tool: "nerve_onboarding_status", arguments: `{"generation":7}`},
		{name: "duplicate idempotency key", tool: "nerve_onboarding_start", arguments: `{"idempotency_key":"start-1","idempotency_key":"start-2","organization_name":"Example","mailbox_mode":"managed_mailbox"}`},
		{name: "case-folded idempotency alias", tool: "nerve_onboarding_start", arguments: `{"Idempotency_Key":"start-1","organization_name":"Example","mailbox_mode":"managed_mailbox"}`},
		{name: "case-folded duplicate idempotency alias", tool: "nerve_onboarding_start", arguments: `{"idempotency_key":"start-1","Idempotency_Key":"start-2","organization_name":"Example","mailbox_mode":"managed_mailbox"}`},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			before := provisioner.calls
			if _, err := invokeOnboardingTool(context.Background(), provisioner, caller, test.tool, json.RawMessage(test.arguments)); err == nil {
				t.Fatal("invalid authority-bearing arguments were accepted")
			}
			if provisioner.calls != before {
				t.Fatal("invalid arguments reached provisioner")
			}
		})
	}
}

func TestOnboardingToolInputBoundaries(t *testing.T) {
	principal := auth.Principal{Kind: auth.PrincipalM2MOnboarding, ClientID: "client-1", Generation: 7}
	caller := OnboardingCaller{Principal: principal, Authorization: "Bearer token"}
	for _, test := range []struct {
		name      string
		arguments map[string]any
		wantErr   bool
	}{
		{name: "idempotency N-1", arguments: managedStartArguments(strings.Repeat("k", 127), "Example")},
		{name: "idempotency N", arguments: managedStartArguments(strings.Repeat("k", 128), "Example")},
		{name: "idempotency N+1", arguments: managedStartArguments(strings.Repeat("k", 129), "Example"), wantErr: true},
		{name: "idempotency UTF-8 bytes N", arguments: managedStartArguments(strings.Repeat("😀", 32), "Example")},
		{name: "idempotency UTF-8 bytes N+1", arguments: managedStartArguments(strings.Repeat("😀", 32)+"a", "Example"), wantErr: true},
		{name: "idempotency control", arguments: managedStartArguments("key\u0000value", "Example"), wantErr: true},
		{name: "organization scalars N-1", arguments: managedStartArguments("key", strings.Repeat("o", 159))},
		{name: "organization scalars N", arguments: managedStartArguments("key", strings.Repeat("o", 160))},
		{name: "organization UTF-8 bytes N", arguments: managedStartArguments("key", strings.Repeat("😀", 160))},
		{name: "organization scalars N+1", arguments: managedStartArguments("key", strings.Repeat("o", 161)), wantErr: true},
		{name: "domain N-1", arguments: customStartArguments(domainOfLength(252), "a")},
		{name: "domain N", arguments: customStartArguments(domainOfLength(253), "a")},
		{name: "domain N+1", arguments: customStartArguments(domainOfLength(254), "a"), wantErr: true},
		{name: "local part N-1", arguments: customStartArguments("mail.example.com", "a"+strings.Repeat("b", 62))},
		{name: "local part N", arguments: customStartArguments("mail.example.com", "a"+strings.Repeat("b", 63))},
		{name: "local part N+1", arguments: customStartArguments("mail.example.com", "a"+strings.Repeat("b", 64)), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			arguments, err := json.Marshal(test.arguments)
			if err != nil {
				t.Fatalf("marshal arguments: %v", err)
			}
			provisioner := &recordingOnboardingProvisioner{}
			_, err = invokeOnboardingTool(context.Background(), provisioner, caller, "nerve_onboarding_start", arguments)
			if (err != nil) != test.wantErr {
				t.Fatalf("invoke error=%v wantErr=%t", err, test.wantErr)
			}
			if test.wantErr && provisioner.calls != 0 {
				t.Fatal("invalid boundary input reached provisioner")
			}
		})
	}
}

func TestOnboardingToolsRejectInvalidProvisionerResultsForEveryOperation(t *testing.T) {
	caller := OnboardingCaller{
		Principal:     auth.Principal{Kind: auth.PrincipalM2MOnboarding, ClientID: "client-1", Generation: 7},
		Authorization: "Bearer token",
	}
	valid := func() OnboardingResult {
		return OnboardingResult{
			ResultType: "complete", OnboardingID: "11111111-1111-4111-8111-111111111111",
			Generation: 7, State: "provisioning", Mode: OnboardingMailboxManaged,
		}
	}
	for _, test := range []struct {
		name   string
		mutate func(*OnboardingResult)
	}{
		{name: "nil UUID", mutate: func(result *OnboardingResult) { result.OnboardingID = uuid.Nil.String() }},
		{name: "wrong generation", mutate: func(result *OnboardingResult) { result.Generation = 8 }},
		{name: "invalid state", mutate: func(result *OnboardingResult) { result.State = "failed" }},
		{name: "invalid mode", mutate: func(result *OnboardingResult) { result.Mode = "mailbox_pool" }},
		{name: "invalid DNS record", mutate: func(result *OnboardingResult) { result.DNSRecords = []OnboardingDNSRecord{{}} }},
		{name: "invalid DNS check", mutate: func(result *OnboardingResult) { result.DNSChecks = []OnboardingDNSCheck{{}} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := valid()
			test.mutate(&result)
			provisioner := &recordingOnboardingProvisioner{result: &result}
			for tool, arguments := range map[string]json.RawMessage{
				"nerve_onboarding_start":         json.RawMessage(`{"idempotency_key":"start-1","organization_name":"Example","mailbox_mode":"managed_mailbox"}`),
				"nerve_onboarding_status":        json.RawMessage(`{}`),
				"nerve_onboarding_verify_domain": json.RawMessage(`{}`),
				"nerve_onboarding_close":         json.RawMessage(`{"idempotency_key":"close-1","expected_generation":7}`),
			} {
				t.Run(tool, func(t *testing.T) {
					if _, err := invokeOnboardingTool(context.Background(), provisioner, caller, tool, arguments); err == nil {
						t.Fatal("invalid provisioner result accepted")
					}
				})
			}
		})
	}
}

func managedStartArguments(key, organization string) map[string]any {
	return map[string]any{
		"idempotency_key": key, "organization_name": organization, "mailbox_mode": "managed_mailbox",
	}
}

func customStartArguments(domain, localPart string) map[string]any {
	return map[string]any{
		"idempotency_key": "key", "organization_name": "Example", "mailbox_mode": "custom_domain",
		"custom_domain": domain, "local_part": localPart,
	}
}

func domainOfLength(length int) string {
	return strings.Join([]string{
		strings.Repeat("a", 63), strings.Repeat("b", 63), strings.Repeat("c", 63), strings.Repeat("d", length-192),
	}, ".")
}

func TestM2MOrgCannotListOrCallConfiguredOnboardingTools(t *testing.T) {
	cfg := hostedRouterConfig()
	runtime := NewServer(cfg, nil, auth.NewService(cfg, nil), nil)
	runtime.Onboarding = &recordingOnboardingProvisioner{}
	handler := NewSDKHandler(runtime, true)
	principal := auth.Principal{
		Kind: auth.PrincipalM2MOrg, OrgID: "org-1", ClientID: "client-1", Generation: 7,
		// Include the lifecycle scope defensively: profile kind must still prevent
		// registration and dispatch even if an impossible mixed token is injected.
		Scopes: []string{"nerve:email.read", "nerve:onboarding"}, AuthMethod: "m2m_bearer",
	}
	listRequest := onboardingModernRequest(t, principal, "tools/list", map[string]any{"_meta": modernOAuthMeta()}, "")
	listRecorder := httptest.NewRecorder()
	handler.ServeHTTP(listRecorder, listRequest)
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("tools/list status=%d body=%s", listRecorder.Code, listRecorder.Body.String())
	}
	for _, toolName := range []string{"nerve_onboarding_start", "nerve_onboarding_status", "nerve_onboarding_verify_domain", "nerve_onboarding_close"} {
		if strings.Contains(listRecorder.Body.String(), `"name":"`+toolName+`"`) {
			t.Fatalf("m2m_org listed lifecycle tool %s", toolName)
		}
		request := onboardingModernRequest(t, principal, "tools/call", map[string]any{
			"_meta": modernOAuthMeta(), "name": toolName, "arguments": map[string]any{},
		}, toolName)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("hidden lifecycle call %s status=%d body=%s", toolName, recorder.Code, recorder.Body.String())
		}
	}
	if provisioner := runtime.Onboarding.(*recordingOnboardingProvisioner); provisioner.calls != 0 {
		t.Fatalf("hidden lifecycle calls reached provisioner %d times", provisioner.calls)
	}
}

func onboardingModernRequest(t *testing.T, principal auth.Principal, method string, params map[string]any, name string) *http.Request {
	t.Helper()
	request := modernContractRequest(t, method, params)
	request.Header.Set("Authorization", "Bearer original-token")
	if name != "" {
		request.Header.Set("Mcp-Name", name)
	}
	return request.WithContext(auth.WithPrincipal(request.Context(), principal))
}
