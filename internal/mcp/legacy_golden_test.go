package mcp

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"neuralmail/internal/config"
	"neuralmail/internal/entitlements"
)

func TestLegacyInitializeWireGolden(t *testing.T) {
	cfg := config.Default()
	runtime := NewServer(cfg, nil, nil, nil)
	request := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
	))
	request.Header.Set("MCP-Protocol-Version", LegacyProtocolVersion)
	recorder := httptest.NewRecorder()
	NewRouter(cfg, nil, NewLegacyHandler(runtime), nil).ServeHTTP(recorder, request)

	want := `{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"resources":true,"tools":true},"protocolVersion":"2025-11-25","serverInfo":{"name":"nerve-runtime","version":"0.1.0"}}}` + "\n"
	if got := recorder.Body.String(); got != want {
		t.Fatalf("legacy initialize wire changed\n got: %s want: %s", got, want)
	}
	if recorder.Header().Get("MCP-Session-Id") == "" {
		t.Fatal("legacy initialize no longer establishes a session")
	}
}

func TestLegacyResourcesListWireGolden(t *testing.T) {
	cfg := config.Default()
	runtime := NewServer(cfg, nil, nil, nil)
	runtime.sessions["frozen-session"] = time.Now().Add(time.Hour)
	request := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(
		`{"jsonrpc":"2.0","id":3,"method":"resources/list","params":{}}`,
	))
	request.Header.Set("MCP-Protocol-Version", LegacyProtocolVersion)
	request.Header.Set("MCP-Session-Id", "frozen-session")
	recorder := httptest.NewRecorder()
	NewRouter(cfg, nil, NewLegacyHandler(runtime), nil).ServeHTTP(recorder, request)

	want := `{"jsonrpc":"2.0","id":3,"result":{"resources":[{"description":"List inbox IDs","uri":"email://inboxes"}]}}` + "\n"
	if got := recorder.Body.String(); got != want {
		t.Fatalf("legacy resources/list wire changed\n got: %s want: %s", got, want)
	}
}

func TestLegacyToolsListWireGolden(t *testing.T) {
	cfg := config.Default()
	runtime := NewServer(cfg, nil, nil, nil)
	runtime.sessions["frozen-session"] = time.Now().Add(time.Hour)
	request := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
	))
	request.Header.Set("MCP-Protocol-Version", LegacyProtocolVersion)
	request.Header.Set("MCP-Session-Id", "frozen-session")
	recorder := httptest.NewRecorder()
	NewRouter(cfg, nil, NewLegacyHandler(runtime), nil).ServeHTTP(recorder, request)

	want := `{"jsonrpc":"2.0","id":2,"result":{"tools":[{"description":"List threads in an inbox","name":"list_threads"},{"description":"Fetch a thread with messages","name":"get_thread"},{"description":"Semantic search over an inbox","name":"search_inbox"},{"description":"Classify intent, urgency, sentiment","name":"triage_message"},{"description":"Extract structured data","name":"extract_to_schema"},{"description":"Draft a reply constrained by policy","name":"draft_reply_with_policy"},{"description":"Send a reply","inputSchema":{"additionalProperties":false,"properties":{"body_or_draft_id":{"type":"string"},"html":{"type":"string"},"idempotency_key":{"type":"string"},"needs_human_approval":{"default":false,"type":"boolean"},"thread_id":{"minLength":1,"type":"string"}},"required":["thread_id","body_or_draft_id"],"type":"object"},"name":"send_reply"},{"description":"Compose and send a new email (not a reply)","inputSchema":{"additionalProperties":false,"anyOf":[{"required":["body"]},{"required":["html"]}],"properties":{"body":{"type":"string"},"from_name":{"type":"string"},"html":{"type":"string"},"idempotency_key":{"type":"string"},"inbox_id":{"minLength":1,"type":"string"},"subject":{"type":"string"},"to":{"format":"email","type":"string"}},"required":["inbox_id","to","subject"],"type":"object"},"name":"compose_email"}]}}` + "\n"
	if got := recorder.Body.String(); got != want {
		t.Fatalf("legacy tools/list wire changed\n got: %s want: %s", got, want)
	}
}

func TestLegacyBusinessErrorWireGolden(t *testing.T) {
	server := &Server{}
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"quota", entitlements.ErrQuotaExceeded, `{"jsonrpc":"2.0","id":2,"error":{"code":-32040,"message":"quota_exceeded","data":{"retryable":false}}}` + "\n"},
		{"subscription", entitlements.ErrSubscriptionInactive, `{"jsonrpc":"2.0","id":2,"error":{"code":-32041,"message":"subscription_inactive","data":{"retryable":false}}}` + "\n"},
		{"rate", &entitlements.RateLimitError{RetryAfterSeconds: 12}, `{"jsonrpc":"2.0","id":2,"error":{"code":-32042,"message":"rate_limited","data":{"retry_after_seconds":12,"retryable":true}}}` + "\n"},
		{"idempotency", &entitlements.IdempotencyInProgressError{RetryAfterSeconds: 3}, `{"jsonrpc":"2.0","id":2,"error":{"code":-32043,"message":"idempotency_in_progress","data":{"retry_after_seconds":3,"retryable":true}}}` + "\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			server.writeDispatchError(recorder, 2, test.err)
			if got := recorder.Body.String(); got != test.want {
				t.Fatalf("legacy wire changed\n got: %s want: %s", got, test.want)
			}
		})
	}
}

func TestModernTranslationTableNeverUsesLegacyCodes(t *testing.T) {
	errorsToTranslate := []error{
		entitlements.ErrQuotaExceeded,
		entitlements.ErrSubscriptionInactive,
		&entitlements.RateLimitError{RetryAfterSeconds: 12},
		&entitlements.IdempotencyInProgressError{RetryAfterSeconds: 3},
		errors.New("unexpected"),
	}
	for _, err := range errorsToTranslate {
		translated := translateModernBusinessError(err)
		if translated.Code == "-32040" || translated.Code == "-32041" || translated.Code == "-32042" || translated.Code == "-32043" {
			t.Fatalf("modern translation reused legacy code for %v: %#v", err, translated)
		}
	}
}
