package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"neuralmail/internal/auth"
	"neuralmail/internal/config"
	"neuralmail/internal/entitlements"
)

func TestModernContractRejectsMetadataAndHeaderFailuresBeforeDispatch(t *testing.T) {
	runtime := NewServer(config.Default(), nil, nil, nil)
	handler := NewSDKHandler(runtime, true)
	validMeta := map[string]any{
		sdkmcp.MetaKeyProtocolVersion: ModernProtocolVersion,
		sdkmcp.MetaKeyClientCapabilities: map[string]any{
			"extensions": map[string]any{oauthClientCredentialsExtension: map[string]any{}},
		},
	}

	tests := []struct {
		name       string
		meta       map[string]any
		headerName string
		headerTool string
		wantCode   int
	}{
		{name: "missing protocol", meta: map[string]any{sdkmcp.MetaKeyClientCapabilities: map[string]any{}}, headerName: "tools/list", wantCode: sdkmcp.CodeHeaderMismatch},
		{name: "unsupported protocol", meta: map[string]any{sdkmcp.MetaKeyProtocolVersion: "2099-01-01", sdkmcp.MetaKeyClientCapabilities: map[string]any{}}, headerName: "tools/list", wantCode: sdkmcp.CodeUnsupportedProtocolVersion},
		{name: "missing capabilities", meta: map[string]any{sdkmcp.MetaKeyProtocolVersion: ModernProtocolVersion}, headerName: "tools/list", wantCode: sdkmcp.CodeMissingRequiredClientCapabilities},
		{name: "malformed client info", meta: mergeModernMeta(validMeta, sdkmcp.MetaKeyClientInfo, map[string]any{"name": "", "version": "0.3.0"}), headerName: "tools/list", wantCode: sdkmcp.CodeHeaderMismatch},
		{name: "method mismatch", meta: validMeta, headerName: "resources/list", wantCode: sdkmcp.CodeHeaderMismatch},
		{name: "name mismatch", meta: validMeta, headerName: "tools/call", headerTool: "wrong-tool", wantCode: sdkmcp.CodeHeaderMismatch},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			method := "tools/list"
			params := map[string]any{"_meta": test.meta}
			if test.name == "name mismatch" {
				method = "tools/call"
				params["name"] = "list_threads"
				params["arguments"] = map[string]any{}
			}
			request := modernContractRequest(t, method, params)
			request.Header.Set("Mcp-Method", test.headerName)
			if test.headerTool != "" {
				request.Header.Set("Mcp-Name", test.headerTool)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			var response Response
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode response: %v body=%s", err, recorder.Body.String())
			}
			if response.Error == nil || response.Error.Code != test.wantCode {
				t.Fatalf("error=%#v want code=%d", response.Error, test.wantCode)
			}
			if runtime.MemoryBudget.Used() != 0 {
				t.Fatalf("contract failure leaked %d bytes", runtime.MemoryBudget.Used())
			}
		})
	}
}

func TestModernContractRequiresOAuthExtensionOnlyForM2M(t *testing.T) {
	runtime := NewServer(config.Default(), nil, nil, nil)
	handler := NewSDKHandler(runtime, true)
	meta := map[string]any{
		sdkmcp.MetaKeyProtocolVersion:    ModernProtocolVersion,
		sdkmcp.MetaKeyClientCapabilities: map[string]any{},
	}

	m2mRequest := modernContractRequest(t, "tools/list", map[string]any{"_meta": meta})
	m2mRequest = m2mRequest.WithContext(auth.WithPrincipal(m2mRequest.Context(), auth.Principal{Kind: auth.PrincipalM2MOrg, OrgID: "org-1"}))
	m2mRecorder := httptest.NewRecorder()
	handler.ServeHTTP(m2mRecorder, m2mRequest)
	var denied Response
	if err := json.Unmarshal(m2mRecorder.Body.Bytes(), &denied); err != nil {
		t.Fatalf("decode M2M response: %v", err)
	}
	if denied.Error == nil || denied.Error.Code != sdkmcp.CodeMissingRequiredClientCapabilities {
		t.Fatalf("M2M missing extension response=%s", m2mRecorder.Body.String())
	}

	legacyRequest := modernContractRequest(t, "tools/list", map[string]any{"_meta": meta})
	legacyRequest = legacyRequest.WithContext(auth.WithPrincipal(legacyRequest.Context(), auth.Principal{Kind: auth.PrincipalCloudAPIKey, OrgID: "org-1"}))
	legacyRecorder := httptest.NewRecorder()
	handler.ServeHTTP(legacyRecorder, legacyRequest)
	if legacyRecorder.Code != http.StatusOK {
		t.Fatalf("non-M2M base metadata status=%d body=%s", legacyRecorder.Code, legacyRecorder.Body.String())
	}
}

func TestModernContractAcceptsOmittedClientInfo(t *testing.T) {
	runtime := NewServer(config.Default(), nil, nil, nil)
	meta := map[string]any{
		sdkmcp.MetaKeyProtocolVersion: ModernProtocolVersion,
		sdkmcp.MetaKeyClientCapabilities: map[string]any{
			"extensions": map[string]any{oauthClientCredentialsExtension: map[string]any{}},
		},
	}
	request := modernContractRequest(t, "tools/list", map[string]any{"_meta": meta})
	request = request.WithContext(auth.WithPrincipal(context.Background(), auth.Principal{Kind: auth.PrincipalM2MOrg, OrgID: "org-1"}))
	recorder := httptest.NewRecorder()
	NewSDKHandler(runtime, true).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("omitted clientInfo status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestModernContractSupportsJSONAndSSEResponses(t *testing.T) {
	meta := map[string]any{
		sdkmcp.MetaKeyProtocolVersion:    ModernProtocolVersion,
		sdkmcp.MetaKeyClientCapabilities: map[string]any{},
	}
	for _, test := range []struct {
		name         string
		jsonResponse bool
		contentType  string
	}{
		{name: "json", jsonResponse: true, contentType: "application/json"},
		{name: "sse", jsonResponse: false, contentType: "text/event-stream"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := NewServer(config.Default(), nil, nil, nil)
			request := modernContractRequest(t, "tools/list", map[string]any{"_meta": meta})
			recorder := httptest.NewRecorder()
			NewSDKHandler(runtime, test.jsonResponse).ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if got := recorder.Header().Get("Content-Type"); len(got) < len(test.contentType) || got[:len(test.contentType)] != test.contentType {
				t.Fatalf("content type=%q want base %q", got, test.contentType)
			}
			if !bytes.Contains(recorder.Body.Bytes(), []byte(`"jsonrpc":"2.0"`)) ||
				!bytes.Contains(recorder.Body.Bytes(), []byte(`"result"`)) {
				t.Fatalf("missing final JSON-RPC result: %s", recorder.Body.String())
			}
		})
	}
}

func TestModernToolArgumentsFailSchemaBeforeInvokerSideEffects(t *testing.T) {
	cfg := hostedRouterConfig()
	authService := auth.NewService(cfg, nil)
	entitlementGate := &fakeEntitlementGate{preAuthErr: entitlements.ErrQuotaExceeded}
	runtime := NewServer(cfg, nil, authService, entitlementGate)
	runtime.OutboundPolicy = allowOutboundPolicyGate{}
	principal := auth.Principal{
		Kind: auth.PrincipalM2MOrg, OrgID: "org-1", ClientID: "client-1", Generation: 1,
		Scopes: []string{"nerve:email.compose"}, AuthMethod: "m2m_bearer",
	}
	meta := map[string]any{
		sdkmcp.MetaKeyProtocolVersion: ModernProtocolVersion,
		sdkmcp.MetaKeyClientCapabilities: map[string]any{
			"extensions": map[string]any{oauthClientCredentialsExtension: map[string]any{}},
		},
	}
	for _, arguments := range []map[string]any{
		{"inbox_id": "inbox-1", "to": "not-an-email", "subject": "subject", "body": "body"},
		{"inbox_id": strings.Repeat("i", 129), "to": "valid@example.com", "subject": "subject", "body": "body"},
	} {
		request := modernContractRequest(t, "tools/call", map[string]any{
			"_meta": meta, "name": "compose_email", "arguments": arguments,
		})
		request.Header.Set("Mcp-Name", "compose_email")
		request = request.WithContext(auth.WithPrincipal(request.Context(), principal))
		recorder := httptest.NewRecorder()
		NewSDKHandler(runtime, true).ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("schema failure status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		if !bytes.Contains(recorder.Body.Bytes(), []byte(`"isError":true`)) ||
			!bytes.Contains(recorder.Body.Bytes(), []byte(`validating`)) {
			t.Fatalf("schema failure did not return a tool error: %s", recorder.Body.String())
		}
	}
	if entitlementGate.preAuthCalls != 0 {
		t.Fatalf("invalid arguments reached entitlement gate %d times", entitlementGate.preAuthCalls)
	}
}

func TestModernHiddenToolCallStillReachesInvoker(t *testing.T) {
	cfg := hostedRouterConfig()
	authService := auth.NewService(cfg, nil)
	runtime := NewServer(cfg, nil, authService, nil)
	runtime.OutboundPolicy = denyOutboundPolicyGate{code: "test_policy_denied"}
	principal := auth.Principal{
		Kind: auth.PrincipalM2MOrg, OrgID: "org-1", ClientID: "client-1", Generation: 1,
		Scopes: []string{"nerve:email.compose"}, AuthMethod: "m2m_bearer",
	}
	meta := map[string]any{
		sdkmcp.MetaKeyProtocolVersion: ModernProtocolVersion,
		sdkmcp.MetaKeyClientCapabilities: map[string]any{
			"extensions": map[string]any{oauthClientCredentialsExtension: map[string]any{}},
		},
	}

	listRequest := modernContractRequest(t, "tools/list", map[string]any{"_meta": meta})
	listRequest = listRequest.WithContext(auth.WithPrincipal(listRequest.Context(), principal))
	listRecorder := httptest.NewRecorder()
	NewSDKHandler(runtime, true).ServeHTTP(listRecorder, listRequest)
	if listRecorder.Code != http.StatusOK || bytes.Contains(listRecorder.Body.Bytes(), []byte(`"compose_email"`)) {
		t.Fatalf("denied tool remained discoverable: status=%d body=%s", listRecorder.Code, listRecorder.Body.String())
	}

	callRequest := modernContractRequest(t, "tools/call", map[string]any{
		"_meta": meta,
		"name":  "compose_email",
		"arguments": map[string]any{
			"inbox_id": "inbox-1", "to": "recipient@example.net", "subject": "subject", "body": "body",
		},
	})
	callRequest.Header.Set("Mcp-Name", "compose_email")
	callRequest = callRequest.WithContext(auth.WithPrincipal(callRequest.Context(), principal))
	callRecorder := httptest.NewRecorder()
	NewSDKHandler(runtime, true).ServeHTTP(callRecorder, callRequest)
	if callRecorder.Code != http.StatusOK ||
		!bytes.Contains(callRecorder.Body.Bytes(), []byte(`"isError":true`)) ||
		!bytes.Contains(callRecorder.Body.Bytes(), []byte(`test_policy_denied`)) ||
		bytes.Contains(callRecorder.Body.Bytes(), []byte(`unknown tool`)) {
		t.Fatalf("hidden call bypassed invoker policy: status=%d body=%s", callRecorder.Code, callRecorder.Body.String())
	}
}

type denyOutboundPolicyGate struct {
	code string
}

func (gate denyOutboundPolicyGate) Authorize(context.Context, auth.Principal, string, json.RawMessage) error {
	return &outboundPolicyError{Code: gate.code}
}

func modernContractRequest(t *testing.T, method string, params map[string]any) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("MCP-Protocol-Version", ModernProtocolVersion)
	request.Header.Set("Mcp-Method", method)
	return request
}

func mergeModernMeta(base map[string]any, key string, value any) map[string]any {
	copy := make(map[string]any, len(base)+1)
	for name, item := range base {
		copy[name] = item
	}
	copy[key] = value
	return copy
}
