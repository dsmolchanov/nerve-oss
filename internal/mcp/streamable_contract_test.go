package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"neuralmail/internal/auth"
	"neuralmail/internal/config"
	"neuralmail/internal/entitlements"
)

const modernTestUUID = "11111111-1111-4111-8111-111111111111"

func TestModernContractRejectsMetadataAndHeaderFailuresBeforeDispatch(t *testing.T) {
	tests := []struct {
		name              string
		method            string
		params            map[string]any
		protocolHeaders   []string
		methodHeaders     []string
		nameHeaders       []string
		wantCode          int
		wantRequested     string
		wantSupported     []string
		wantOAuthRequired bool
	}{
		{name: "missing protocol header", method: "tools/call", params: modernListThreadsParams(modernOAuthMeta()), methodHeaders: []string{"tools/call"}, nameHeaders: []string{"list_threads"}, wantCode: sdkmcp.CodeHeaderMismatch},
		{name: "duplicate protocol header", method: "tools/call", params: modernListThreadsParams(modernOAuthMeta()), protocolHeaders: []string{ModernProtocolVersion, ModernProtocolVersion}, methodHeaders: []string{"tools/call"}, nameHeaders: []string{"list_threads"}, wantCode: sdkmcp.CodeHeaderMismatch},
		{name: "unsupported protocol header", method: "tools/call", params: modernListThreadsParams(modernOAuthMeta()), protocolHeaders: []string{"2099-01-01"}, methodHeaders: []string{"tools/call"}, nameHeaders: []string{"list_threads"}, wantCode: sdkmcp.CodeUnsupportedProtocolVersion, wantRequested: "2099-01-01", wantSupported: []string{LegacyProtocolVersion, ModernProtocolVersion}},
		{name: "missing protocol metadata", method: "tools/call", params: modernListThreadsParams(map[string]any{sdkmcp.MetaKeyClientCapabilities: map[string]any{}}), protocolHeaders: []string{ModernProtocolVersion}, methodHeaders: []string{"tools/call"}, nameHeaders: []string{"list_threads"}, wantCode: sdkmcp.CodeHeaderMismatch},
		{name: "null protocol metadata", method: "tools/call", params: modernListThreadsParams(map[string]any{sdkmcp.MetaKeyProtocolVersion: nil, sdkmcp.MetaKeyClientCapabilities: map[string]any{}}), protocolHeaders: []string{ModernProtocolVersion}, methodHeaders: []string{"tools/call"}, nameHeaders: []string{"list_threads"}, wantCode: sdkmcp.CodeHeaderMismatch},
		{name: "non-string protocol metadata", method: "tools/call", params: modernListThreadsParams(map[string]any{sdkmcp.MetaKeyProtocolVersion: 20260728, sdkmcp.MetaKeyClientCapabilities: map[string]any{}}), protocolHeaders: []string{ModernProtocolVersion}, methodHeaders: []string{"tools/call"}, nameHeaders: []string{"list_threads"}, wantCode: sdkmcp.CodeHeaderMismatch},
		{name: "empty protocol metadata", method: "tools/call", params: modernListThreadsParams(map[string]any{sdkmcp.MetaKeyProtocolVersion: "", sdkmcp.MetaKeyClientCapabilities: map[string]any{}}), protocolHeaders: []string{ModernProtocolVersion}, methodHeaders: []string{"tools/call"}, nameHeaders: []string{"list_threads"}, wantCode: sdkmcp.CodeHeaderMismatch},
		{name: "header body protocol mismatch", method: "tools/call", params: modernListThreadsParams(map[string]any{sdkmcp.MetaKeyProtocolVersion: LegacyProtocolVersion, sdkmcp.MetaKeyClientCapabilities: map[string]any{}}), protocolHeaders: []string{ModernProtocolVersion}, methodHeaders: []string{"tools/call"}, nameHeaders: []string{"list_threads"}, wantCode: sdkmcp.CodeUnsupportedProtocolVersion, wantRequested: LegacyProtocolVersion, wantSupported: []string{ModernProtocolVersion}},
		{name: "missing capabilities", method: "tools/call", params: modernListThreadsParams(map[string]any{sdkmcp.MetaKeyProtocolVersion: ModernProtocolVersion}), protocolHeaders: []string{ModernProtocolVersion}, methodHeaders: []string{"tools/call"}, nameHeaders: []string{"list_threads"}, wantCode: sdkmcp.CodeMissingRequiredClientCapabilities},
		{name: "null capabilities", method: "tools/call", params: modernListThreadsParams(map[string]any{sdkmcp.MetaKeyProtocolVersion: ModernProtocolVersion, sdkmcp.MetaKeyClientCapabilities: nil}), protocolHeaders: []string{ModernProtocolVersion}, methodHeaders: []string{"tools/call"}, nameHeaders: []string{"list_threads"}, wantCode: sdkmcp.CodeMissingRequiredClientCapabilities},
		{name: "array capabilities", method: "tools/call", params: modernListThreadsParams(map[string]any{sdkmcp.MetaKeyProtocolVersion: ModernProtocolVersion, sdkmcp.MetaKeyClientCapabilities: []any{}}), protocolHeaders: []string{ModernProtocolVersion}, methodHeaders: []string{"tools/call"}, nameHeaders: []string{"list_threads"}, wantCode: sdkmcp.CodeMissingRequiredClientCapabilities},
		{name: "string capabilities", method: "tools/call", params: modernListThreadsParams(map[string]any{sdkmcp.MetaKeyProtocolVersion: ModernProtocolVersion, sdkmcp.MetaKeyClientCapabilities: "invalid"}), protocolHeaders: []string{ModernProtocolVersion}, methodHeaders: []string{"tools/call"}, nameHeaders: []string{"list_threads"}, wantCode: sdkmcp.CodeMissingRequiredClientCapabilities},
		{name: "missing oauth extension", method: "tools/call", params: modernListThreadsParams(map[string]any{sdkmcp.MetaKeyProtocolVersion: ModernProtocolVersion, sdkmcp.MetaKeyClientCapabilities: map[string]any{}}), protocolHeaders: []string{ModernProtocolVersion}, methodHeaders: []string{"tools/call"}, nameHeaders: []string{"list_threads"}, wantCode: sdkmcp.CodeMissingRequiredClientCapabilities, wantOAuthRequired: true},
		{name: "null extensions", method: "tools/call", params: modernListThreadsParams(map[string]any{sdkmcp.MetaKeyProtocolVersion: ModernProtocolVersion, sdkmcp.MetaKeyClientCapabilities: map[string]any{"extensions": nil}}), protocolHeaders: []string{ModernProtocolVersion}, methodHeaders: []string{"tools/call"}, nameHeaders: []string{"list_threads"}, wantCode: sdkmcp.CodeMissingRequiredClientCapabilities, wantOAuthRequired: true},
		{name: "array extensions", method: "tools/call", params: modernListThreadsParams(map[string]any{sdkmcp.MetaKeyProtocolVersion: ModernProtocolVersion, sdkmcp.MetaKeyClientCapabilities: map[string]any{"extensions": []any{}}}), protocolHeaders: []string{ModernProtocolVersion}, methodHeaders: []string{"tools/call"}, nameHeaders: []string{"list_threads"}, wantCode: sdkmcp.CodeMissingRequiredClientCapabilities, wantOAuthRequired: true},
		{name: "null oauth settings", method: "tools/call", params: modernListThreadsParams(map[string]any{sdkmcp.MetaKeyProtocolVersion: ModernProtocolVersion, sdkmcp.MetaKeyClientCapabilities: map[string]any{"extensions": map[string]any{oauthClientCredentialsExtension: nil}}}), protocolHeaders: []string{ModernProtocolVersion}, methodHeaders: []string{"tools/call"}, nameHeaders: []string{"list_threads"}, wantCode: sdkmcp.CodeMissingRequiredClientCapabilities, wantOAuthRequired: true},
		{name: "array oauth settings", method: "tools/call", params: modernListThreadsParams(map[string]any{sdkmcp.MetaKeyProtocolVersion: ModernProtocolVersion, sdkmcp.MetaKeyClientCapabilities: map[string]any{"extensions": map[string]any{oauthClientCredentialsExtension: []any{}}}}), protocolHeaders: []string{ModernProtocolVersion}, methodHeaders: []string{"tools/call"}, nameHeaders: []string{"list_threads"}, wantCode: sdkmcp.CodeMissingRequiredClientCapabilities, wantOAuthRequired: true},
		{name: "null client info", method: "tools/call", params: modernListThreadsParams(mergeModernMeta(modernOAuthMeta(), sdkmcp.MetaKeyClientInfo, nil)), protocolHeaders: []string{ModernProtocolVersion}, methodHeaders: []string{"tools/call"}, nameHeaders: []string{"list_threads"}, wantCode: sdkmcp.CodeHeaderMismatch},
		{name: "non-object client info", method: "tools/call", params: modernListThreadsParams(mergeModernMeta(modernOAuthMeta(), sdkmcp.MetaKeyClientInfo, "nerve-email-python/0.3.0")), protocolHeaders: []string{ModernProtocolVersion}, methodHeaders: []string{"tools/call"}, nameHeaders: []string{"list_threads"}, wantCode: sdkmcp.CodeHeaderMismatch},
		{name: "missing client name", method: "tools/call", params: modernListThreadsParams(mergeModernMeta(modernOAuthMeta(), sdkmcp.MetaKeyClientInfo, map[string]any{"version": "0.3.0"})), protocolHeaders: []string{ModernProtocolVersion}, methodHeaders: []string{"tools/call"}, nameHeaders: []string{"list_threads"}, wantCode: sdkmcp.CodeHeaderMismatch},
		{name: "missing client version", method: "tools/call", params: modernListThreadsParams(mergeModernMeta(modernOAuthMeta(), sdkmcp.MetaKeyClientInfo, map[string]any{"name": "nerve-email-python"})), protocolHeaders: []string{ModernProtocolVersion}, methodHeaders: []string{"tools/call"}, nameHeaders: []string{"list_threads"}, wantCode: sdkmcp.CodeHeaderMismatch},
		{name: "oversized client name", method: "tools/call", params: modernListThreadsParams(mergeModernMeta(modernOAuthMeta(), sdkmcp.MetaKeyClientInfo, map[string]any{"name": strings.Repeat("n", 129), "version": "0.3.0"})), protocolHeaders: []string{ModernProtocolVersion}, methodHeaders: []string{"tools/call"}, nameHeaders: []string{"list_threads"}, wantCode: sdkmcp.CodeHeaderMismatch},
		{name: "oversized client version", method: "tools/call", params: modernListThreadsParams(mergeModernMeta(modernOAuthMeta(), sdkmcp.MetaKeyClientInfo, map[string]any{"name": "nerve-email-python", "version": strings.Repeat("v", 129)})), protocolHeaders: []string{ModernProtocolVersion}, methodHeaders: []string{"tools/call"}, nameHeaders: []string{"list_threads"}, wantCode: sdkmcp.CodeHeaderMismatch},
		{name: "oversized client title", method: "tools/call", params: modernListThreadsParams(mergeModernMeta(modernOAuthMeta(), sdkmcp.MetaKeyClientInfo, map[string]any{"name": "nerve-email-python", "version": "0.3.0", "title": strings.Repeat("t", 257)})), protocolHeaders: []string{ModernProtocolVersion}, methodHeaders: []string{"tools/call"}, nameHeaders: []string{"list_threads"}, wantCode: sdkmcp.CodeHeaderMismatch},
		{name: "oversized client description", method: "tools/call", params: modernListThreadsParams(mergeModernMeta(modernOAuthMeta(), sdkmcp.MetaKeyClientInfo, map[string]any{"name": "nerve-email-python", "version": "0.3.0", "description": strings.Repeat("d", 1_025)})), protocolHeaders: []string{ModernProtocolVersion}, methodHeaders: []string{"tools/call"}, nameHeaders: []string{"list_threads"}, wantCode: sdkmcp.CodeHeaderMismatch},
		{name: "oversized client website", method: "tools/call", params: modernListThreadsParams(mergeModernMeta(modernOAuthMeta(), sdkmcp.MetaKeyClientInfo, map[string]any{"name": "nerve-email-python", "version": "0.3.0", "websiteUrl": strings.Repeat("w", 2_049)})), protocolHeaders: []string{ModernProtocolVersion}, methodHeaders: []string{"tools/call"}, nameHeaders: []string{"list_threads"}, wantCode: sdkmcp.CodeHeaderMismatch},
		{name: "missing method header", method: "tools/call", params: modernListThreadsParams(modernOAuthMeta()), protocolHeaders: []string{ModernProtocolVersion}, nameHeaders: []string{"list_threads"}, wantCode: sdkmcp.CodeHeaderMismatch},
		{name: "duplicate method header", method: "tools/call", params: modernListThreadsParams(modernOAuthMeta()), protocolHeaders: []string{ModernProtocolVersion}, methodHeaders: []string{"tools/call", "tools/call"}, nameHeaders: []string{"list_threads"}, wantCode: sdkmcp.CodeHeaderMismatch},
		{name: "method mismatch", method: "tools/call", params: modernListThreadsParams(modernOAuthMeta()), protocolHeaders: []string{ModernProtocolVersion}, methodHeaders: []string{"resources/list"}, nameHeaders: []string{"list_threads"}, wantCode: sdkmcp.CodeHeaderMismatch},
		{name: "missing name header", method: "tools/call", params: modernListThreadsParams(modernOAuthMeta()), protocolHeaders: []string{ModernProtocolVersion}, methodHeaders: []string{"tools/call"}, wantCode: sdkmcp.CodeHeaderMismatch},
		{name: "duplicate name header", method: "tools/call", params: modernListThreadsParams(modernOAuthMeta()), protocolHeaders: []string{ModernProtocolVersion}, methodHeaders: []string{"tools/call"}, nameHeaders: []string{"list_threads", "list_threads"}, wantCode: sdkmcp.CodeHeaderMismatch},
		{name: "name mismatch", method: "tools/call", params: modernListThreadsParams(modernOAuthMeta()), protocolHeaders: []string{ModernProtocolVersion}, methodHeaders: []string{"tools/call"}, nameHeaders: []string{"wrong-tool"}, wantCode: sdkmcp.CodeHeaderMismatch},
		{name: "missing body name", method: "tools/call", params: map[string]any{"_meta": modernOAuthMeta(), "arguments": map[string]any{}}, protocolHeaders: []string{ModernProtocolVersion}, methodHeaders: []string{"tools/call"}, nameHeaders: []string{"list_threads"}, wantCode: sdkmcp.CodeHeaderMismatch},
		{name: "malformed body name", method: "tools/call", params: map[string]any{"_meta": modernOAuthMeta(), "name": 42, "arguments": map[string]any{}}, protocolHeaders: []string{ModernProtocolVersion}, methodHeaders: []string{"tools/call"}, nameHeaders: []string{"list_threads"}, wantCode: sdkmcp.CodeHeaderMismatch},
		{name: "unexpected name header", method: "tools/list", params: map[string]any{"_meta": modernOAuthMeta()}, protocolHeaders: []string{ModernProtocolVersion}, methodHeaders: []string{"tools/list"}, nameHeaders: []string{"stale-tool"}, wantCode: sdkmcp.CodeHeaderMismatch},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := hostedRouterConfig()
			entitlementGate := &fakeEntitlementGate{preAuthErr: entitlements.ErrQuotaExceeded}
			runtime := NewServer(cfg, nil, auth.NewService(cfg, nil), entitlementGate)
			principal := auth.Principal{
				Kind: auth.PrincipalM2MOrg, OrgID: "org-1", ClientID: "client-1", Generation: 1,
				Scopes: []string{"nerve:email.read"}, AuthMethod: "m2m_bearer",
			}
			router := NewRouter(cfg, authenticatorFunc(func(*http.Request) (auth.Principal, error) {
				return principal, nil
			}), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("invalid modern request reached legacy adapter")
			}), NewSDKHandler(runtime, true))
			request := modernContractRequest(t, test.method, test.params)
			setHeaderValues(request.Header, "MCP-Protocol-Version", test.protocolHeaders)
			setHeaderValues(request.Header, "Mcp-Method", test.methodHeaders)
			setHeaderValues(request.Header, "Mcp-Name", test.nameHeaders)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
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
			assertModernContractErrorData(t, response.Error, test.wantRequested, test.wantSupported, test.wantOAuthRequired)
			if entitlementGate.preAuthCalls != 0 {
				t.Fatalf("contract failure reached entitlement gate %d times", entitlementGate.preAuthCalls)
			}
			if runtime.MemoryBudget.Used() != 0 {
				t.Fatalf("contract failure leaked %d bytes", runtime.MemoryBudget.Used())
			}
		})
	}
}

func modernListThreadsParams(meta map[string]any) map[string]any {
	return map[string]any{
		"_meta": meta,
		"name":  "list_threads",
		"arguments": map[string]any{
			"inbox_id": modernTestUUID,
			"limit":    1,
		},
	}
}

func setHeaderValues(header http.Header, name string, values []string) {
	header.Del(name)
	for _, value := range values {
		header.Add(name, value)
	}
}

func assertModernContractErrorData(t *testing.T, responseError *ResponseError, requested string, supported []string, oauthRequired bool) {
	t.Helper()
	if requested != "" {
		data, ok := responseError.Data.(map[string]any)
		if !ok || data["requested"] != requested {
			t.Fatalf("unsupported-version data=%#v", responseError.Data)
		}
		gotSupported, ok := data["supported"].([]any)
		if !ok || len(gotSupported) != len(supported) {
			t.Fatalf("unsupported-version supported=%#v", data["supported"])
		}
		for index, version := range supported {
			if gotSupported[index] != version {
				t.Fatalf("unsupported-version supported=%#v want=%#v", gotSupported, supported)
			}
		}
		return
	}
	if responseError.Code != sdkmcp.CodeMissingRequiredClientCapabilities {
		if responseError.Data != nil {
			t.Fatalf("unexpected error data=%#v", responseError.Data)
		}
		return
	}
	data, ok := responseError.Data.(map[string]any)
	if !ok {
		t.Fatalf("missing-capabilities data=%#v", responseError.Data)
	}
	required, ok := data["requiredCapabilities"].(map[string]any)
	if !ok {
		t.Fatalf("required capabilities=%#v", data["requiredCapabilities"])
	}
	roots, hasRoots := required["roots"].(map[string]any)
	if !hasRoots || len(roots) != 0 {
		t.Fatalf("required roots shape=%#v", required["roots"])
	}
	extensions, hasExtensions := required["extensions"].(map[string]any)
	if oauthRequired {
		if !hasExtensions || len(required) != 2 {
			t.Fatalf("OAuth extension requirement missing: %#v", required)
		}
		if _, ok := extensions[oauthClientCredentialsExtension].(map[string]any); !ok {
			t.Fatalf("OAuth extension settings missing: %#v", extensions)
		}
	} else if hasExtensions || len(required) != 1 {
		t.Fatalf("base capability error unexpectedly requires capabilities: %#v", required)
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

func TestModernContractMissingOAuthExtensionFailsBeforeToolDispatch(t *testing.T) {
	cfg := hostedRouterConfig()
	entitlementGate := &fakeEntitlementGate{preAuthErr: entitlements.ErrQuotaExceeded}
	runtime := NewServer(cfg, nil, auth.NewService(cfg, nil), entitlementGate)
	meta := map[string]any{
		sdkmcp.MetaKeyProtocolVersion:    ModernProtocolVersion,
		sdkmcp.MetaKeyClientCapabilities: map[string]any{},
	}
	request := modernContractRequest(t, "tools/call", map[string]any{
		"_meta": meta,
		"name":  "list_threads",
		"arguments": map[string]any{
			"inbox_id": modernTestUUID,
			"limit":    1,
		},
	})
	request.Header.Set("Mcp-Name", "list_threads")
	request = request.WithContext(auth.WithPrincipal(request.Context(), auth.Principal{
		Kind: auth.PrincipalM2MOrg, OrgID: "org-1", ClientID: "client-1", Generation: 1,
		Scopes: []string{"nerve:email.read"}, AuthMethod: "m2m_bearer",
	}))
	recorder := httptest.NewRecorder()
	NewSDKHandler(runtime, true).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("missing extension status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var denied Response
	if err := json.Unmarshal(recorder.Body.Bytes(), &denied); err != nil {
		t.Fatalf("decode missing extension response: %v body=%s", err, recorder.Body.String())
	}
	if denied.Error == nil || denied.Error.Code != sdkmcp.CodeMissingRequiredClientCapabilities || denied.Error.Message != "missing required client capabilities" {
		t.Fatalf("missing extension response=%#v", denied.Error)
	}
	if entitlementGate.preAuthCalls != 0 {
		t.Fatalf("missing extension reached entitlement gate %d times", entitlementGate.preAuthCalls)
	}
	if runtime.MemoryBudget.Used() != 0 {
		t.Fatalf("missing extension leaked %d bytes", runtime.MemoryBudget.Used())
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

func TestModernContractJSONResponseModesEmitOneFinalResponse(t *testing.T) {
	meta := map[string]any{
		sdkmcp.MetaKeyProtocolVersion:    ModernProtocolVersion,
		sdkmcp.MetaKeyClientCapabilities: map[string]any{},
	}
	modes := []struct {
		name         string
		jsonResponse bool
		contentType  string
	}{
		{name: "json", jsonResponse: true, contentType: "application/json"},
		{name: "sse", jsonResponse: false, contentType: "text/event-stream"},
	}
	fixtures := []struct {
		name       string
		method     string
		tool       string
		arguments  map[string]any
		wantMember string
		wantBody   string
	}{
		{name: "success", method: "tools/list", wantMember: `"result"`},
		{
			name: "failure", method: "tools/call", tool: "compose_email", wantMember: `"result"`, wantBody: `"isError":true`,
			arguments: map[string]any{"inbox_id": modernTestUUID, "to": "not-an-email", "subject": "subject", "body": "body"},
		},
	}
	for _, mode := range modes {
		for _, fixture := range fixtures {
			t.Run(mode.name+"/"+fixture.name, func(t *testing.T) {
				runtime := NewServer(config.Default(), nil, nil, nil)
				params := map[string]any{"_meta": meta}
				if fixture.tool != "" {
					params["name"] = fixture.tool
					params["arguments"] = fixture.arguments
				}
				request := modernContractRequest(t, fixture.method, params)
				if fixture.tool != "" {
					request.Header.Set("Mcp-Name", fixture.tool)
				}
				recorder := httptest.NewRecorder()
				NewSDKHandler(runtime, mode.jsonResponse).ServeHTTP(recorder, request)
				if recorder.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
				}
				if got := recorder.Header().Get("Content-Type"); len(got) < len(mode.contentType) || got[:len(mode.contentType)] != mode.contentType {
					t.Fatalf("content type=%q want base %q", got, mode.contentType)
				}
				if !bytes.Contains(recorder.Body.Bytes(), []byte(`"jsonrpc":"2.0"`)) ||
					!bytes.Contains(recorder.Body.Bytes(), []byte(fixture.wantMember)) {
					t.Fatalf("missing final JSON-RPC %s member: %s", fixture.wantMember, recorder.Body.String())
				}
				if fixture.wantBody != "" && !bytes.Contains(recorder.Body.Bytes(), []byte(fixture.wantBody)) {
					t.Fatalf("missing failure fixture %s: %s", fixture.wantBody, recorder.Body.String())
				}
				if err := validateModernResponseStream(request.Context(), recorder.Header().Get("Content-Type"), bytes.NewReader(recorder.Body.Bytes()), json.RawMessage(`1`)); err != nil {
					t.Fatalf("handler response violated contract: %v body=%s", err, recorder.Body.String())
				}
			})
		}
	}
}

func TestExplicitProtocolProfilesReturnFrozenLegacyAndModernWire(t *testing.T) {
	const frozenLegacyInitialize = `{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"resources":true,"tools":true},"protocolVersion":"2025-11-25","serverInfo":{"name":"nerve-runtime","version":"0.1.0"}}}` + "\n"

	for _, test := range []struct {
		name         string
		jsonResponse bool
		contentType  string
	}{
		{name: "modern JSON", jsonResponse: true, contentType: "application/json"},
		{name: "modern SSE", jsonResponse: false, contentType: "text/event-stream"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.Default()
			runtime := NewServer(cfg, nil, nil, nil)
			router := NewRouter(cfg, nil, NewLegacyHandler(runtime), NewSDKHandler(runtime, test.jsonResponse))

			legacyRequest := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(
				`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
			))
			legacyRequest.Header.Set("MCP-Protocol-Version", LegacyProtocolVersion)
			legacyRecorder := httptest.NewRecorder()
			router.ServeHTTP(legacyRecorder, legacyRequest)
			if legacyRecorder.Code != http.StatusOK || legacyRecorder.Body.String() != frozenLegacyInitialize {
				t.Fatalf("explicit legacy wire changed: status=%d body=%s", legacyRecorder.Code, legacyRecorder.Body.String())
			}
			if legacyRecorder.Header().Get("MCP-Session-Id") == "" {
				t.Fatal("explicit legacy profile did not establish its frozen session")
			}

			modernRequest := modernContractRequest(t, "tools/list", map[string]any{"_meta": map[string]any{
				sdkmcp.MetaKeyProtocolVersion:    ModernProtocolVersion,
				sdkmcp.MetaKeyClientCapabilities: map[string]any{},
			}})
			modernRecorder := httptest.NewRecorder()
			router.ServeHTTP(modernRecorder, modernRequest)
			if modernRecorder.Code != http.StatusOK {
				t.Fatalf("explicit modern status=%d body=%s", modernRecorder.Code, modernRecorder.Body.String())
			}
			mediaType, _, err := mime.ParseMediaType(modernRecorder.Header().Get("Content-Type"))
			if err != nil || mediaType != test.contentType {
				t.Fatalf("explicit modern content type=%q want=%q err=%v", mediaType, test.contentType, err)
			}
			if err := validateModernResponseStream(
				modernRequest.Context(), modernRecorder.Header().Get("Content-Type"),
				bytes.NewReader(modernRecorder.Body.Bytes()), json.RawMessage(`1`),
			); err != nil {
				t.Fatalf("explicit modern wire violated contract: %v body=%s", err, modernRecorder.Body.String())
			}
			if !bytes.Contains(modernRecorder.Body.Bytes(), []byte(`"cacheScope":"private"`)) ||
				bytes.Contains(modernRecorder.Body.Bytes(), []byte(`"protocolVersion":"2025-11-25"`)) {
				t.Fatalf("explicit modern response did not retain the modern private shape: %s", modernRecorder.Body.String())
			}
		})
	}
}

func TestModernJSONAndSSEContractFixtures(t *testing.T) {
	validResponse := `{"jsonrpc":"2.0","id":1,"result":{}}`
	validError := `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`
	tests := []struct {
		name        string
		contentType string
		body        string
		wantError   bool
	}{
		{name: "JSON result", contentType: "application/json; charset=utf-8", body: validResponse},
		{name: "JSON error", contentType: "application/json", body: validError},
		{name: "malformed JSON", contentType: "application/json", body: `{"jsonrpc":"2.0","id":1`, wantError: true},
		{name: "wrong JSON response ID", contentType: "application/json", body: `{"jsonrpc":"2.0","id":2,"result":{}}`, wantError: true},
		{name: "JSON response without result or error", contentType: "application/json", body: `{"jsonrpc":"2.0","id":1}`, wantError: true},
		{name: "duplicate JSON response", contentType: "application/json", body: validResponse + validResponse, wantError: true},
		{name: "multiline comments and related notification", contentType: "text/event-stream; charset=utf-8", body: ": keepalive\n\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\ndata: \"method\":\"notifications/progress\",\"params\":{}}\n\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\ndata: \"id\":1,\"result\":{}}\n\n"},
		{name: "SSE error", contentType: "text/event-stream", body: "data: " + validError + "\n\n"},
		{name: "malformed event line", contentType: "text/event-stream", body: "not-an-sse-field\n\n", wantError: true},
		{name: "truncated JSON", contentType: "text/event-stream", body: "data: {\"jsonrpc\":\"2.0\",\"id\":1", wantError: true},
		{name: "wrong response ID", contentType: "text/event-stream", body: "data: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{}}\n\n", wantError: true},
		{name: "duplicate final response", contentType: "text/event-stream", body: "data: " + validResponse + "\n\ndata: " + validResponse + "\n\n", wantError: true},
		{name: "EOF without final response", contentType: "text/event-stream", body: "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n", wantError: true},
		{name: "missing content type", body: validResponse, wantError: true},
		{name: "unsupported content type", contentType: "text/plain", body: validResponse, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateModernResponseStream(context.Background(), test.contentType, strings.NewReader(test.body), json.RawMessage(`1`))
			if (err != nil) != test.wantError {
				t.Fatalf("error=%v wantError=%v", err, test.wantError)
			}
		})
	}
}

func TestModernSSEContractHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := validateModernResponseStream(ctx, "text/event-stream", cancelReader{ctx: ctx}, json.RawMessage(`1`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error=%v", err)
	}
}

type cancelReader struct {
	ctx context.Context
}

func (reader cancelReader) Read([]byte) (int, error) {
	<-reader.ctx.Done()
	return 0, reader.ctx.Err()
}

type modernConformanceMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

func validateModernResponseStream(ctx context.Context, contentType string, reader io.Reader, expectedID json.RawMessage) error {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return fmt.Errorf("invalid content type: %w", err)
	}
	switch mediaType {
	case "application/json":
		decoder := json.NewDecoder(reader)
		var message modernConformanceMessage
		if err := decoder.Decode(&message); err != nil {
			return err
		}
		if err := requireConformanceJSONEOF(decoder); err != nil {
			return err
		}
		return validateModernFinalMessage(message, expectedID)
	case "text/event-stream":
		return validateModernSSE(ctx, reader, expectedID)
	default:
		return fmt.Errorf("unsupported content type %q", mediaType)
	}
}

func validateModernSSE(ctx context.Context, reader io.Reader, expectedID json.RawMessage) error {
	scanner := bufio.NewScanner(reader)
	var data []string
	finalResponses := 0
	flush := func() error {
		if len(data) == 0 {
			return nil
		}
		var message modernConformanceMessage
		if err := json.Unmarshal([]byte(strings.Join(data, "\n")), &message); err != nil {
			return fmt.Errorf("malformed SSE JSON: %w", err)
		}
		data = nil
		if len(message.ID) == 0 && message.Method != "" {
			return nil
		}
		if err := validateModernFinalMessage(message, expectedID); err != nil {
			return err
		}
		finalResponses++
		if finalResponses > 1 {
			return errors.New("duplicate final response")
		}
		return nil
	}
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := scanner.Text()
		switch {
		case line == "":
			if err := flush(); err != nil {
				return err
			}
		case strings.HasPrefix(line, ":"):
			continue
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case strings.HasPrefix(line, "event:") || strings.HasPrefix(line, "id:") || strings.HasPrefix(line, "retry:"):
			continue
		default:
			return fmt.Errorf("malformed SSE line %q", line)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	if finalResponses != 1 {
		return errors.New("EOF without exactly one final response")
	}
	return nil
}

func validateModernFinalMessage(message modernConformanceMessage, expectedID json.RawMessage) error {
	if message.JSONRPC != "2.0" || len(message.ID) == 0 || (!bytes.Equal(bytes.TrimSpace(message.ID), bytes.TrimSpace(expectedID))) {
		return errors.New("wrong JSON-RPC response ID or version")
	}
	if len(message.Result) == 0 && len(message.Error) == 0 {
		return errors.New("response has neither result nor error")
	}
	return nil
}

func requireConformanceJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("duplicate JSON response")
		}
		return err
	}
	return nil
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
		{"inbox_id": modernTestUUID, "to": "not-an-email", "subject": "subject", "body": "body"},
		{"inbox_id": strings.Repeat("i", 129), "to": "valid@example.com", "subject": "subject", "body": "body"},
		{"inbox_id": "not-a-uuid", "to": "valid@example.com", "subject": "subject", "body": "body"},
		{"inbox_id": modernTestUUID, "to": "valid@example.com", "from": "spoofed@example.com", "subject": "subject", "body": "body"},
		{"inbox_id": modernTestUUID, "to": "valid@example.com", "authentication_results": "dkim=pass", "subject": "subject", "body": "body"},
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

func TestModernEmptyOutboundFieldsFailSchemaBeforePolicy(t *testing.T) {
	cfg := hostedRouterConfig()
	runtime := NewServer(cfg, nil, auth.NewService(cfg, nil), nil)
	policyGate := &countingOutboundPolicyGate{}
	runtime.OutboundPolicy = policyGate
	tests := []struct {
		name      string
		tool      string
		scope     string
		arguments map[string]any
	}{
		{name: "empty reply", tool: "send_reply", scope: "nerve:email.reply", arguments: map[string]any{"thread_id": modernTestUUID, "body_or_draft_id": ""}},
		{name: "empty compose subject", tool: "compose_email", scope: "nerve:email.compose", arguments: map[string]any{"inbox_id": modernTestUUID, "to": "recipient@example.net", "subject": "", "body": "body"}},
		{name: "empty compose body", tool: "compose_email", scope: "nerve:email.compose", arguments: map[string]any{"inbox_id": modernTestUUID, "to": "recipient@example.net", "subject": "subject", "body": ""}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			principal := auth.Principal{
				Kind: auth.PrincipalM2MOrg, OrgID: "org-1", ClientID: "client-1", Generation: 1,
				Scopes: []string{test.scope}, AuthMethod: "m2m_bearer",
			}
			request := modernContractRequest(t, "tools/call", map[string]any{
				"_meta": modernOAuthMeta(), "name": test.tool, "arguments": test.arguments,
			})
			request.Header.Set("Mcp-Name", test.tool)
			request = request.WithContext(auth.WithPrincipal(request.Context(), principal))
			recorder := httptest.NewRecorder()
			NewSDKHandler(runtime, true).ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK ||
				!bytes.Contains(recorder.Body.Bytes(), []byte(`"isError":true`)) ||
				!bytes.Contains(recorder.Body.Bytes(), []byte(`validating`)) {
				t.Fatalf("empty field did not fail schema validation: status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
	if policyGate.dispatchCalls.Load() != 0 {
		t.Fatalf("empty reply reached outbound policy %d times", policyGate.dispatchCalls.Load())
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
			"inbox_id": modernTestUUID, "to": "recipient@example.net", "subject": "subject", "body": "body",
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

func TestModernToolsListChangesWithPrincipalAndPolicyStateAndStaysPrivate(t *testing.T) {
	cfg := hostedRouterConfig()
	runtime := NewServer(cfg, nil, auth.NewService(cfg, nil), nil)
	policy := &mutableOutboundPolicyGate{allowed: true}
	runtime.OutboundPolicy = policy
	handler := NewSDKHandler(runtime, true)

	type catalogCase struct {
		name      string
		principal auth.Principal
		allowed   bool
		wantTools []string
	}
	cases := []catalogCase{
		{
			name: "onboarding principal",
			principal: auth.Principal{
				Kind: auth.PrincipalM2MOnboarding, ClientID: "client-1", Generation: 1,
				Scopes: []string{"nerve:onboarding"}, AuthMethod: "m2m_bearer",
			},
			allowed:   true,
			wantTools: nil,
		},
		{
			name: "read-only org principal",
			principal: auth.Principal{
				Kind: auth.PrincipalM2MOrg, OrgID: "org-1", ClientID: "client-1", Generation: 1,
				Scopes: []string{"nerve:email.read"}, AuthMethod: "m2m_bearer",
			},
			allowed:   true,
			wantTools: []string{"get_thread", "list_threads"},
		},
		{
			name: "compose-enabled org state",
			principal: auth.Principal{
				Kind: auth.PrincipalM2MOrg, OrgID: "org-1", ClientID: "client-1", Generation: 1,
				Scopes: []string{"nerve:email.compose"}, AuthMethod: "m2m_bearer",
			},
			allowed:   true,
			wantTools: []string{"compose_email"},
		},
		{
			name: "compose-denied org state",
			principal: auth.Principal{
				Kind: auth.PrincipalM2MOrg, OrgID: "org-1", ClientID: "client-1", Generation: 1,
				Scopes: []string{"nerve:email.compose"}, AuthMethod: "m2m_bearer",
			},
			allowed:   false,
			wantTools: nil,
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			policy.allowed = test.allowed
			request := modernContractRequest(t, "tools/list", map[string]any{"_meta": modernOAuthMeta()})
			request = request.WithContext(auth.WithPrincipal(request.Context(), test.principal))
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("tools/list status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			var response struct {
				Result struct {
					Tools []struct {
						Name string `json:"name"`
					} `json:"tools"`
					CacheScope string `json:"cacheScope"`
					TTLMs      int64  `json:"ttlMs"`
				} `json:"result"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode tools/list response: %v: %s", err, recorder.Body.String())
			}
			if response.Result.CacheScope != "private" || response.Result.TTLMs != 5_000 {
				t.Fatalf("cache metadata=%q/%d want private/5000", response.Result.CacheScope, response.Result.TTLMs)
			}
			gotTools := make([]string, 0, len(response.Result.Tools))
			for _, tool := range response.Result.Tools {
				gotTools = append(gotTools, tool.Name)
			}
			if !slices.Equal(gotTools, test.wantTools) {
				t.Fatalf("tools=%v want=%v", gotTools, test.wantTools)
			}
		})
	}
}

func TestModernLockedOrgListsReadDraftAndReplyButNeverCompose(t *testing.T) {
	cfg := hostedRouterConfig()
	runtime := NewServer(cfg, nil, auth.NewService(cfg, nil), nil)
	runtime.OutboundPolicy = replyOnlyOutboundPolicyGate{}
	handler := NewSDKHandler(runtime, true)
	principal := auth.Principal{
		Kind: auth.PrincipalM2MOrg, OrgID: "org-1", ClientID: "client-1", Generation: 1,
		Scopes:     []string{"nerve:email.read", "nerve:email.search", "nerve:email.draft", "nerve:email.reply", "nerve:email.compose"},
		AuthMethod: "m2m_bearer",
	}

	request := modernContractRequest(t, "tools/list", map[string]any{"_meta": modernOAuthMeta()})
	request = request.WithContext(auth.WithPrincipal(request.Context(), principal))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("tools/list status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, tool := range response.Result.Tools {
		listed[tool.Name] = true
	}
	for _, required := range []string{"list_threads", "get_thread", "search_inbox", "triage_message", "extract_to_schema", "draft_reply_with_policy", "send_reply"} {
		if !listed[required] {
			t.Fatalf("locked org omitted allowed tool %q: %v", required, listed)
		}
	}
	if listed["compose_email"] {
		t.Fatalf("locked org listed compose: %v", listed)
	}

	call := modernContractRequest(t, "tools/call", map[string]any{
		"_meta": modernOAuthMeta(), "name": "compose_email",
		"arguments": map[string]any{"inbox_id": modernTestUUID, "to": "recipient@example.net", "subject": "subject", "body": "body"},
	})
	call.Header.Set("Mcp-Name", "compose_email")
	call = call.WithContext(auth.WithPrincipal(call.Context(), principal))
	callRecorder := httptest.NewRecorder()
	handler.ServeHTTP(callRecorder, call)
	if callRecorder.Code != http.StatusOK ||
		!bytes.Contains(callRecorder.Body.Bytes(), []byte(`"isError":true`)) ||
		!bytes.Contains(callRecorder.Body.Bytes(), []byte(`email_compose_org_enabled_denied`)) {
		t.Fatalf("locked compose call was not denied: status=%d body=%s", callRecorder.Code, callRecorder.Body.String())
	}
}

func TestModernCachedComposeAuthorityIsRevokedByLiveSuspensionState(t *testing.T) {
	cfg := hostedRouterConfig()
	runtime := NewServer(cfg, nil, auth.NewService(cfg, nil), nil)
	policy := &mutableOutboundPolicyGate{allowed: true}
	runtime.OutboundPolicy = policy
	handler := NewSDKHandler(runtime, true)
	principal := auth.Principal{
		Kind: auth.PrincipalM2MOrg, OrgID: "org-1", ClientID: "client-1", Generation: 1,
		Scopes: []string{"nerve:email.compose"}, AuthMethod: "m2m_bearer",
	}

	list := func() *httptest.ResponseRecorder {
		request := modernContractRequest(t, "tools/list", map[string]any{"_meta": modernOAuthMeta()})
		request = request.WithContext(auth.WithPrincipal(request.Context(), principal))
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("tools/list status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		return recorder
	}
	if recorder := list(); !bytes.Contains(recorder.Body.Bytes(), []byte(`"compose_email"`)) {
		t.Fatalf("compose missing before suspension: %s", recorder.Body.String())
	}

	// The principal models an already-issued generation-bound token. A live
	// complaint changes policy state, not token bytes, so both discovery and
	// direct invocation must consult the new state on the next request.
	policy.allowed = false
	if recorder := list(); bytes.Contains(recorder.Body.Bytes(), []byte(`"compose_email"`)) {
		t.Fatalf("cached compose remained visible after suspension: %s", recorder.Body.String())
	}

	call := modernContractRequest(t, "tools/call", map[string]any{
		"_meta": modernOAuthMeta(),
		"name":  "compose_email",
		"arguments": map[string]any{
			"inbox_id": modernTestUUID, "to": "recipient@example.net",
			"subject": "subject", "body": "body",
		},
	})
	call.Header.Set("Mcp-Name", "compose_email")
	call = call.WithContext(auth.WithPrincipal(call.Context(), principal))
	callRecorder := httptest.NewRecorder()
	handler.ServeHTTP(callRecorder, call)
	if callRecorder.Code != http.StatusOK ||
		!bytes.Contains(callRecorder.Body.Bytes(), []byte(`"isError":true`)) ||
		!bytes.Contains(callRecorder.Body.Bytes(), []byte(`test_policy_denied`)) {
		t.Fatalf("cached compose call was not denied: status=%d body=%s", callRecorder.Code, callRecorder.Body.String())
	}
}

func TestModernToolScopeFailureReturnsOAuth403BeforeInvoker(t *testing.T) {
	cfg := hostedRouterConfig()
	authService := auth.NewService(cfg, nil)
	entitlementGate := &fakeEntitlementGate{preAuthErr: entitlements.ErrQuotaExceeded}
	runtime := NewServer(cfg, nil, authService, entitlementGate)
	runtime.OutboundPolicy = allowOutboundPolicyGate{}
	principal := auth.Principal{
		Kind: auth.PrincipalM2MOrg, OrgID: "org-1", ClientID: "client-1", Generation: 1,
		Scopes: []string{"nerve:email.read"}, AuthMethod: "m2m_bearer",
	}
	meta := modernOAuthMeta()
	request := modernContractRequest(t, "tools/call", map[string]any{
		"_meta": meta,
		"name":  "compose_email",
		"arguments": map[string]any{
			"inbox_id": modernTestUUID, "to": "recipient@example.net", "subject": "subject", "body": "body",
		},
	})
	request.Header.Set("Mcp-Name", "compose_email")
	request = request.WithContext(auth.WithPrincipal(request.Context(), principal))
	recorder := httptest.NewRecorder()
	NewSDKHandler(runtime, true).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("scope failure status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("WWW-Authenticate"); got != `Bearer error="insufficient_scope", scope="nerve:email.compose"` {
		t.Fatalf("scope challenge=%q", got)
	}
	if entitlementGate.preAuthCalls != 0 {
		t.Fatalf("scope failure reached entitlement gate %d times", entitlementGate.preAuthCalls)
	}
}

func TestModernResourceIDsAreValidatedBeforeRead(t *testing.T) {
	cfg := hostedRouterConfig()
	runtime := NewServer(cfg, nil, auth.NewService(cfg, nil), nil)
	principal := auth.Principal{Kind: auth.PrincipalM2MOrg, OrgID: "org-1", Scopes: []string{"nerve:email.read"}}
	for _, uri := range []string{"email://threads/" + strings.Repeat("i", 129), "email://messages/not-a-uuid"} {
		request := modernContractRequest(t, "resources/read", map[string]any{"_meta": modernOAuthMeta(), "uri": uri})
		request.Header.Set("Mcp-Name", uri)
		request = request.WithContext(auth.WithPrincipal(request.Context(), principal))
		recorder := httptest.NewRecorder()
		NewSDKHandler(runtime, true).ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest || !bytes.Contains(recorder.Body.Bytes(), []byte(`"code":-32602`)) {
			t.Fatalf("invalid resource ID status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	}
}

type denyOutboundPolicyGate struct {
	code string
}

type replyOnlyOutboundPolicyGate struct{}

func (replyOnlyOutboundPolicyGate) Authorize(_ context.Context, _ auth.Principal, tool string, _ json.RawMessage) error {
	if tool == "compose_email" {
		return &outboundPolicyError{Code: "email_compose_org_enabled_denied"}
	}
	return nil
}

type countingOutboundPolicyGate struct {
	dispatchCalls atomic.Int32
}

type mutableOutboundPolicyGate struct {
	allowed bool
}

func (gate *countingOutboundPolicyGate) Authorize(_ context.Context, _ auth.Principal, _ string, arguments json.RawMessage) error {
	if arguments != nil {
		gate.dispatchCalls.Add(1)
	}
	return nil
}

func (gate *mutableOutboundPolicyGate) Authorize(context.Context, auth.Principal, string, json.RawMessage) error {
	if gate.allowed {
		return nil
	}
	return &outboundPolicyError{Code: "test_policy_denied"}
}

func modernOAuthMeta() map[string]any {
	return map[string]any{
		sdkmcp.MetaKeyProtocolVersion: ModernProtocolVersion,
		sdkmcp.MetaKeyClientCapabilities: map[string]any{
			"extensions": map[string]any{oauthClientCredentialsExtension: map[string]any{}},
		},
	}
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
