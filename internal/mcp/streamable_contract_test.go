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
		{name: "unexpected name", meta: validMeta, headerName: "tools/list", headerTool: "stale-tool", wantCode: sdkmcp.CodeHeaderMismatch},
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
			if err := validateModernResponseStream(request.Context(), recorder.Header().Get("Content-Type"), bytes.NewReader(recorder.Body.Bytes()), json.RawMessage(`1`)); err != nil {
				t.Fatalf("handler response violated contract: %v body=%s", err, recorder.Body.String())
			}
		})
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

func TestModernSSEContractFixtures(t *testing.T) {
	validResponse := `{"jsonrpc":"2.0","id":1,"result":{}}`
	tests := []struct {
		name        string
		contentType string
		body        string
		wantError   bool
	}{
		{name: "multiline comments and related notification", contentType: "text/event-stream; charset=utf-8", body: ": keepalive\n\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\ndata: \"method\":\"notifications/progress\",\"params\":{}}\n\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\ndata: \"id\":1,\"result\":{}}\n\n"},
		{name: "malformed event line", contentType: "text/event-stream", body: "not-an-sse-field\n\n", wantError: true},
		{name: "truncated JSON", contentType: "text/event-stream", body: "data: {\"jsonrpc\":\"2.0\",\"id\":1", wantError: true},
		{name: "wrong response ID", contentType: "text/event-stream", body: "data: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{}}\n\n", wantError: true},
		{name: "duplicate final response", contentType: "text/event-stream", body: "data: " + validResponse + "\n\ndata: " + validResponse + "\n\n", wantError: true},
		{name: "EOF without final response", contentType: "text/event-stream", body: "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n", wantError: true},
		{name: "missing content type", body: validResponse, wantError: true},
		{name: "unsupported content type", contentType: "text/plain", body: validResponse, wantError: true},
		{name: "duplicate JSON response", contentType: "application/json", body: validResponse + validResponse, wantError: true},
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
