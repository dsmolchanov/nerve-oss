package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"neuralmail/internal/auth"
	"neuralmail/internal/config"
	"neuralmail/internal/entitlements"
	"neuralmail/internal/llm"
	"neuralmail/internal/tools"
)

var unavailableCalls = []ToolInvocation{
	{Name: "triage_message", Arguments: json.RawMessage(`{"message_id":"11111111-1111-4111-8111-111111111111"}`)},
	{Name: "extract_to_schema", Arguments: json.RawMessage(`{"message_id":"11111111-1111-4111-8111-111111111111","schema_id":"invoice"}`)},
	{Name: "draft_reply_with_policy", Arguments: json.RawMessage(`{"thread_id":"11111111-1111-4111-8111-111111111111","goal":"reply"}`)},
}

func TestUnavailableAIPrecedesUsageButFollowsScopes(t *testing.T) {
	cfg := config.Default()
	cfg.Cloud.Mode = true
	gate := &fakeEntitlementGate{preAuthErr: entitlements.ErrQuotaExceeded}
	srv := NewServer(cfg, &tools.Service{LLM: llm.NewNoop()}, auth.NewService(cfg, nil), gate)
	for _, call := range unavailableCalls {
		principal := auth.Principal{Kind: auth.PrincipalM2MOrg, OrgID: "org-1", Scopes: []string{"nerve:email.draft"}}
		result, err := srv.Invoker.Invoke(auth.WithPrincipal(context.Background(), principal), call)
		if !errors.Is(err, llm.ErrUnavailable) || result != nil || gate.preAuthCalls != 0 {
			t.Fatalf("%s: %v %v calls=%d", call.Name, result, err, gate.preAuthCalls)
		}
		principal.Scopes = nil
		_, err = srv.Invoker.Invoke(auth.WithPrincipal(context.Background(), principal), call)
		if err == nil || errors.Is(err, llm.ErrUnavailable) {
			t.Fatalf("scope check bypassed: %v", err)
		}
	}
	// FTS does not require an LLM: it must reach the existing entitlement gate.
	principal := auth.Principal{Kind: auth.PrincipalM2MOrg, OrgID: "org-1", Scopes: []string{"nerve:email.search"}}
	_, err := srv.Invoker.Invoke(auth.WithPrincipal(context.Background(), principal), ToolInvocation{Name: "search_inbox", Arguments: json.RawMessage(`{}`)})
	if !errors.Is(err, entitlements.ErrQuotaExceeded) || gate.preAuthCalls != 1 {
		t.Fatalf("search incorrectly gated: %v", err)
	}
}

func assertUnavailableWire(t *testing.T, data []byte) {
	t.Helper()
	for _, want := range []string{`"code":"ai_unavailable"`, `"retryable":false`, `"remediation":"docs/SELF_HOSTING.md#ai-configuration"`} {
		if !bytes.Contains(data, []byte(want)) {
			t.Fatalf("missing %s: %s", want, data)
		}
	}
}

func TestUnavailableAILegacyHTTP(t *testing.T) {
	cfg := config.Default()
	srv := NewServer(cfg, &tools.Service{LLM: llm.NewNoop()}, nil, nil)
	srv.sessions["test-session"] = time.Now().Add(time.Hour)
	router := NewRouter(cfg, nil, NewLegacyHandler(srv), nil)
	for _, call := range unavailableCalls {
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": call.Name, "arguments": call.Arguments}})
		req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(payload))
		req.Header.Set("MCP-Protocol-Version", LegacyProtocolVersion)
		req.Header.Set("MCP-Session-Id", "test-session")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		assertUnavailableWire(t, rec.Body.Bytes())
	}
}

func TestUnavailableAIModernHTTP(t *testing.T) {
	for _, jsonResponse := range []bool{true, false} {
		cfg := config.Default()
		srv := NewServer(cfg, &tools.Service{LLM: llm.NewNoop()}, nil, nil)
		hosted := httptest.NewServer(NewRouter(cfg, nil, NewLegacyHandler(srv), NewSDKHandler(srv, jsonResponse)))
		client := newModernSDKTestClient()
		session, err := client.Connect(context.Background(), &sdkmcp.StreamableClientTransport{Endpoint: hosted.URL, DisableStandaloneSSE: true}, nil)
		if err != nil {
			hosted.Close()
			t.Fatal(err)
		}
		for _, call := range unavailableCalls {
			result, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: call.Name, Arguments: call.Arguments})
			if err != nil {
				t.Fatal(err)
			}
			if !result.IsError {
				t.Fatalf("false success: %#v", result)
			}
			raw, _ := json.Marshal(result.StructuredContent)
			assertUnavailableWire(t, raw)
		}
		session.Close()
		hosted.Close()
	}
}

func TestUnavailableAIStdio(t *testing.T) {
	// Run the actual stdio adapter; no parallel tests may swap these process streams.
	input, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	for _, call := range unavailableCalls {
		if err := json.NewEncoder(input).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": call.Name, "arguments": call.Arguments}}); err != nil {
			t.Fatal(err)
		}
	}
	input.Seek(0, io.SeekStart)
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = input, output
	defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()
	err = RunStdio(context.Background(), NewServer(config.Default(), &tools.Service{LLM: llm.NewNoop()}, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	output.Seek(0, io.SeekStart)
	raw, _ := io.ReadAll(output)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 {
		t.Fatalf("responses: %s", raw)
	}
	for _, line := range lines {
		assertUnavailableWire(t, []byte(line))
	}
}
