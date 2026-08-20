package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"neuralmail/internal/auth"
	"neuralmail/internal/config"
	"neuralmail/internal/entitlements"
)

func TestSDKServerIsStatelessAndListsDeterministicEmailTools(t *testing.T) {
	cfg := config.Default()
	runtime := NewServer(cfg, nil, nil, nil)
	hosted := httptest.NewServer(NewRouter(cfg, nil,
		http.HandlerFunc(runtime.HandleRoutedHTTP), NewSDKHandler(runtime, true)))
	defer hosted.Close()

	client := newModernSDKTestClient()
	session, err := client.Connect(context.Background(), &sdkmcp.StreamableClientTransport{
		Endpoint: hosted.URL, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connect modern SDK client: %v", err)
	}
	defer session.Close()

	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list modern tools: %v", err)
	}
	want := []string{"compose_email", "draft_reply_with_policy", "extract_to_schema", "get_thread", "list_threads", "search_inbox", "send_reply", "triage_message"}
	if len(listed.Tools) != len(want) {
		t.Fatalf("tool count=%d want=%d: %#v", len(listed.Tools), len(want), listed.Tools)
	}
	for index, tool := range listed.Tools {
		if tool.Name != want[index] {
			t.Fatalf("tool[%d]=%q want=%q", index, tool.Name, want[index])
		}
	}
	if listed.CacheScope != "private" || listed.TTLMs != 5_000 {
		t.Fatalf("tools/list cache metadata=%q/%d want private/5000", listed.CacheScope, listed.TTLMs)
	}
	for _, tool := range listed.Tools {
		input, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshal %s input schema: %v", tool.Name, err)
		}
		output, err := json.Marshal(tool.OutputSchema)
		if err != nil {
			t.Fatalf("marshal %s output schema: %v", tool.Name, err)
		}
		const draft = `"$schema":"https://json-schema.org/draft/2020-12/schema"`
		if !strings.Contains(string(input), draft) || !strings.Contains(string(output), draft) {
			t.Fatalf("%s does not advertise complete 2020-12 schemas: input=%s output=%s", tool.Name, input, output)
		}
		if tool.Name == "compose_email" && !strings.Contains(string(output), `"thread_id"`) {
			t.Fatalf("compose output schema omits returned thread_id: %s", output)
		}
	}
}

func TestSDKServerTranslatesBusinessFailureAsCallToolResult(t *testing.T) {
	modes := []struct {
		name         string
		jsonResponse bool
		contentType  string
	}{
		{name: "JSON", jsonResponse: true, contentType: "application/json"},
		{name: "SSE", jsonResponse: false, contentType: "text/event-stream"},
	}
	tests := []struct {
		name          string
		err           error
		wantCode      string
		wantRetryable bool
	}{
		{name: "quota", err: entitlements.ErrQuotaExceeded, wantCode: "quota_exceeded"},
		{name: "subscription", err: entitlements.ErrSubscriptionInactive, wantCode: "subscription_inactive"},
		{name: "rate", err: &entitlements.RateLimitError{RetryAfterSeconds: 12}, wantCode: "rate_limited", wantRetryable: true},
		{name: "idempotency", err: &entitlements.IdempotencyInProgressError{RetryAfterSeconds: 3}, wantCode: "idempotency_in_progress", wantRetryable: true},
	}
	for _, mode := range modes {
		for _, test := range tests {
			t.Run(mode.name+"/"+test.name, func(t *testing.T) {
				cfg := hostedRouterConfig()
				authService := auth.NewService(cfg, nil)
				runtime := NewServer(cfg, nil, authService, &fakeEntitlementGate{preAuthErr: test.err})
				principal := auth.Principal{
					Kind: auth.PrincipalM2MOrg, OrgID: "org-1", ClientID: "client-1", Generation: 1,
					Scopes: []string{"nerve:email.read"}, AuthMethod: "m2m_bearer",
				}
				hosted := httptest.NewServer(NewRouter(cfg, authenticatorFunc(func(*http.Request) (auth.Principal, error) {
					return principal, nil
				}), NewLegacyHandler(runtime), NewSDKHandler(runtime, mode.jsonResponse)))
				defer hosted.Close()

				recorder := &rawResponseRecordingTransport{base: http.DefaultTransport, origin: "https://agent.example"}
				client := newModernSDKTestClient()
				session, err := client.Connect(context.Background(), &sdkmcp.StreamableClientTransport{
					Endpoint: hosted.URL, HTTPClient: &http.Client{Transport: recorder}, DisableStandaloneSSE: true,
				}, nil)
				if err != nil {
					t.Fatalf("connect modern client: %v", err)
				}
				defer session.Close()
				result, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{
					Name: "list_threads", Arguments: json.RawMessage(`{"inbox_id":"11111111-1111-4111-8111-111111111111"}`),
				})
				if err != nil {
					t.Fatalf("business failure escaped as protocol error: %v", err)
				}
				if !result.IsError {
					t.Fatalf("business failure missing isError: %#v", result)
				}
				contentType, raw, requestID := recorder.snapshot()
				if !strings.HasPrefix(contentType, mode.contentType) {
					t.Fatalf("content type=%q want base=%q", contentType, mode.contentType)
				}
				if err := validateModernResponseStream(context.Background(), contentType, bytes.NewReader(raw), requestID); err != nil {
					t.Fatalf("raw modern response violated final-response contract: %v body=%s", err, raw)
				}
				wire := string(raw)
				if !strings.Contains(wire, `"code":"`+test.wantCode+`"`) ||
					!strings.Contains(wire, fmt.Sprintf(`"retryable":%t`, test.wantRetryable)) ||
					strings.Contains(wire, "-32040") || strings.Contains(wire, "-32041") ||
					strings.Contains(wire, "-32042") || strings.Contains(wire, "-32043") {
					t.Fatalf("modern error partition violated: %s", raw)
				}
				if test.wantRetryable != strings.Contains(wire, `"retry_at":`) {
					t.Fatalf("modern retry metadata=%s want retry_at present=%v", raw, test.wantRetryable)
				}
			})
		}
	}
}

type rawResponseRecordingTransport struct {
	base   http.RoundTripper
	origin string

	mu          sync.Mutex
	contentType string
	body        []byte
	requestID   json.RawMessage
}

func (transport *rawResponseRecordingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header.Set("Origin", transport.origin)
	var requestID json.RawMessage
	if clone.Body != nil {
		body, err := io.ReadAll(clone.Body)
		if err != nil {
			return nil, err
		}
		clone.Body = io.NopCloser(bytes.NewReader(body))
		var message struct {
			ID json.RawMessage `json:"id"`
		}
		if json.Unmarshal(body, &message) == nil {
			requestID = message.ID
		}
	}
	response, err := transport.base.RoundTrip(clone)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		_ = response.Body.Close()
		return nil, err
	}
	_ = response.Body.Close()
	response.Body = io.NopCloser(bytes.NewReader(body))
	transport.mu.Lock()
	transport.contentType = response.Header.Get("Content-Type")
	transport.body = append(transport.body[:0], body...)
	transport.requestID = append(transport.requestID[:0], requestID...)
	transport.mu.Unlock()
	return response, nil
}

func (transport *rawResponseRecordingTransport) snapshot() (string, []byte, json.RawMessage) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return transport.contentType, append([]byte(nil), transport.body...), append(json.RawMessage(nil), transport.requestID...)
}

func TestSDKServerListsConformantPrivateResources(t *testing.T) {
	cfg := config.Default()
	runtime := NewServer(cfg, nil, nil, nil)
	hosted := httptest.NewServer(NewRouter(cfg, nil, NewLegacyHandler(runtime), NewSDKHandler(runtime, true)))
	defer hosted.Close()

	client := newModernSDKTestClient()
	session, err := client.Connect(context.Background(), &sdkmcp.StreamableClientTransport{
		Endpoint: hosted.URL, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connect modern client: %v", err)
	}
	defer session.Close()
	resources, err := session.ListResources(context.Background(), nil)
	if err != nil {
		t.Fatalf("list resources: %v", err)
	}
	if len(resources.Resources) != 1 || resources.Resources[0].URI != "email://inboxes" {
		t.Fatalf("unexpected resources: %#v", resources.Resources)
	}
	if resources.CacheScope != "private" || resources.TTLMs != 5_000 {
		t.Fatalf("resource cache metadata=%q/%d want private/5000", resources.CacheScope, resources.TTLMs)
	}
	templates, err := session.ListResourceTemplates(context.Background(), nil)
	if err != nil {
		t.Fatalf("list resource templates: %v", err)
	}
	if len(templates.ResourceTemplates) != 2 ||
		templates.ResourceTemplates[0].URITemplate != "email://messages/{message_id}" ||
		templates.ResourceTemplates[1].URITemplate != "email://threads/{thread_id}" {
		t.Fatalf("unexpected resource templates: %#v", templates.ResourceTemplates)
	}
	if templates.CacheScope != "private" || templates.TTLMs != 5_000 {
		t.Fatalf("template cache metadata=%q/%d want private/5000", templates.CacheScope, templates.TTLMs)
	}
}

func TestSDKHandlerEnforcesAndReleasesSharedMemoryBudget(t *testing.T) {
	cfg := config.Default()
	cfg.Memory.BudgetBytes = maxModernRequestMemoryBytes - 1
	exhausted := NewServer(cfg, nil, nil, nil)
	request := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{}`))
	recorder := httptest.NewRecorder()
	NewSDKHandler(exhausted, true).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "1" {
		t.Fatalf("memory exhaustion status=%d retry=%q body=%s", recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String())
	}
	if exhausted.MemoryBudget.Used() != 0 {
		t.Fatalf("failed reservation leaked memory: %d", exhausted.MemoryBudget.Used())
	}

	cfg.Memory.BudgetBytes = 64 << 20
	runtime := NewServer(cfg, nil, nil, nil)
	tooLarge := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(nil))
	tooLarge.ContentLength = maxMCPBodyBytes + 1
	recorder = httptest.NewRecorder()
	NewSDKHandler(runtime, true).ServeHTTP(recorder, tooLarge)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized modern request status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if runtime.MemoryBudget.Used() != 0 {
		t.Fatalf("oversized request changed memory accounting: %d", runtime.MemoryBudget.Used())
	}

	invalid := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{}`))
	recorder = httptest.NewRecorder()
	NewSDKHandler(runtime, true).ServeHTTP(recorder, invalid)
	if runtime.MemoryBudget.Used() != 0 {
		t.Fatalf("completed modern request leaked memory: %d", runtime.MemoryBudget.Used())
	}
}

func TestModernOutputSchemasAcceptEmptySliceEncoding(t *testing.T) {
	descriptors := modernToolCatalog(context.Background(), NewServer(config.Default(), nil, nil, nil), auth.Principal{})
	for _, descriptor := range descriptors {
		encoded, err := json.Marshal(descriptor.OutputShape)
		if err != nil {
			t.Fatalf("marshal %s output shape: %v", descriptor.Name, err)
		}
		if strings.Contains(string(encoded), `"type":"array"`) {
			t.Fatalf("%s output rejects nil slices encoded as null: %s", descriptor.Name, encoded)
		}
	}
}

func TestSDKServerOnboardingProfileHasNoLifecycleToolsBeforePhase3(t *testing.T) {
	cfg := hostedRouterConfig()
	runtime := NewServer(cfg, nil, nil, nil)
	authenticator := authenticatorFunc(func(*http.Request) (auth.Principal, error) {
		return auth.Principal{
			Kind: auth.PrincipalM2MOnboarding, ClientID: "client-1", Generation: 1,
			Scopes: []string{"nerve:onboarding"}, AuthMethod: "m2m_bearer",
		}, nil
	})
	hosted := httptest.NewServer(NewRouter(cfg, authenticator,
		http.HandlerFunc(runtime.HandleRoutedHTTP), NewSDKHandler(runtime, true)))
	defer hosted.Close()

	client := newModernSDKTestClient()
	session, err := client.Connect(context.Background(), &sdkmcp.StreamableClientTransport{
		Endpoint: hosted.URL, HTTPClient: &http.Client{Transport: originRoundTripper{
			base: http.DefaultTransport, origin: "https://agent.example",
		}}, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connect onboarding SDK client: %v", err)
	}
	defer session.Close()
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list onboarding tools: %v", err)
	}
	if len(listed.Tools) != 0 {
		t.Fatalf("lifecycle tools registered before Phase 3: %#v", listed.Tools)
	}
}

func TestSDKServerM2MOrgUsesSplitReplyAndComposeScopes(t *testing.T) {
	cfg := hostedRouterConfig()
	authService := auth.NewService(cfg, nil)
	runtime := NewServer(cfg, nil, authService, nil)
	runtime.OutboundPolicy = allowOutboundPolicyGate{}
	principal := auth.Principal{
		Kind: auth.PrincipalM2MOrg, OrgID: "org-1", ClientID: "client-1", Generation: 1,
		Scopes: []string{"nerve:email.reply", "nerve:email.compose"}, AuthMethod: "m2m_bearer",
	}
	hosted := httptest.NewServer(NewRouter(cfg, authenticatorFunc(func(*http.Request) (auth.Principal, error) {
		return principal, nil
	}), http.HandlerFunc(runtime.HandleRoutedHTTP), NewSDKHandler(runtime, true)))
	defer hosted.Close()

	client := newModernSDKTestClient()
	session, err := client.Connect(context.Background(), &sdkmcp.StreamableClientTransport{
		Endpoint: hosted.URL, HTTPClient: &http.Client{Transport: originRoundTripper{
			base: http.DefaultTransport, origin: "https://agent.example",
		}}, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connect M2M org client: %v", err)
	}
	defer session.Close()
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list M2M org tools: %v", err)
	}
	if len(listed.Tools) != 2 || listed.Tools[0].Name != "compose_email" || listed.Tools[1].Name != "send_reply" {
		t.Fatalf("split scopes exposed wrong tools: %#v", listed.Tools)
	}
	resources, err := session.ListResources(context.Background(), nil)
	if err != nil {
		t.Fatalf("list resources without read scope: %v", err)
	}
	if len(resources.Resources) != 0 {
		t.Fatalf("read resources exposed without nerve:email.read: %#v", resources.Resources)
	}
	templates, err := session.ListResourceTemplates(context.Background(), nil)
	if err != nil {
		t.Fatalf("list resource templates without read scope: %v", err)
	}
	if len(templates.ResourceTemplates) != 0 {
		t.Fatalf("read templates exposed without nerve:email.read: %#v", templates.ResourceTemplates)
	}
}

func TestSDKServerPhase2DoesNotRegisterFutureBillingTool(t *testing.T) {
	cfg := hostedRouterConfig()
	runtime := NewServer(cfg, nil, auth.NewService(cfg, nil), nil)
	principal := auth.Principal{
		Kind: auth.PrincipalM2MOrg, OrgID: "org-1", ClientID: "client-1", Generation: 1,
		Scopes: []string{"nerve:billing.subscribe"}, AuthMethod: "m2m_bearer",
	}
	for _, descriptor := range modernToolDescriptors(context.Background(), runtime, principal) {
		if descriptor.Name == "nerve_billing_subscribe" {
			t.Fatal("Phase 7 billing tool registered during the Phase 2 adapter rollout")
		}
	}
	// The Phase 2 success criterion intentionally keeps future lifecycle and
	// billing dispatch absent. Phase 7 adds BillingProvisioner and registers
	// nerve_billing_subscribe atomically with its live mandate enforcement.
}

type originRoundTripper struct {
	base   http.RoundTripper
	origin string
}

func newModernSDKTestClient() *sdkmcp.Client {
	capabilities := &sdkmcp.ClientCapabilities{}
	capabilities.AddExtension(oauthClientCredentialsExtension, map[string]any{})
	return sdkmcp.NewClient(&sdkmcp.Implementation{Name: "nerve-test", Version: "0.3.0"}, &sdkmcp.ClientOptions{
		Capabilities: capabilities,
	})
}

type allowOutboundPolicyGate struct{}

func (allowOutboundPolicyGate) Authorize(context.Context, auth.Principal, string, json.RawMessage) error {
	return nil
}

func (transport originRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header.Set("Origin", transport.origin)
	return transport.base.RoundTrip(clone)
}
