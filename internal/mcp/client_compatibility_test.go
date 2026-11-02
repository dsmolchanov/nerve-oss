package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/santhosh-tekuri/jsonschema/v5"

	"neuralmail/internal/config"
	"neuralmail/internal/entitlements"
)

func TestStrictLegacySuccessfulToolResultKeepsOriginalPayload(t *testing.T) {
	response := callToolWithEntitlementError(t, &entitlements.IdempotencyReplayError{Response: map[string]any{"threads": []any{map[string]any{"id": "thread-1", "subject": "fixture"}}}})
	if response.Error != nil {
		t.Fatalf("authorized replay refused: %#v", response.Error)
	}
	raw, err := json.Marshal(response.Result)
	if err != nil {
		t.Fatal(err)
	}
	var strict sdkmcp.CallToolResult
	if err = json.Unmarshal(raw, &strict); err != nil {
		t.Fatalf("strict tool result decode: %v", err)
	}
	if len(strict.Content) != 1 {
		t.Fatalf("strict tool result lacks content: %s", raw)
	}
	text, ok := strict.Content[0].(*sdkmcp.TextContent)
	if !ok {
		t.Fatalf("tool content is not text: %#v", strict.Content)
	}
	var displayed map[string]any
	if err = json.Unmarshal([]byte(text.Text), &displayed); err != nil {
		t.Fatal(err)
	}
	legacy := response.Result.(map[string]any)
	if len(legacy["threads"].([]any)) != 1 || len(displayed["threads"].([]any)) != 1 {
		t.Fatal("legacy or strict client lost the thread payload")
	}
}

func TestStrictClientDiscoveryShapes(t *testing.T) {
	runtime := NewServer(config.Default(), nil, nil, nil)
	router := NewRouter(runtime.Config, nil, NewLegacyHandler(runtime), nil)
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"strict-client","version":"1"}}}`))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, request)
	if rec.Code != http.StatusOK {
		t.Fatalf("headerless initialize: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Result sdkmcp.InitializeResult `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("strict SDK initialize decode: %v", err)
	}
	if response.Result.Capabilities == nil || response.Result.Capabilities.Tools == nil || response.Result.Capabilities.Resources == nil {
		t.Fatal("tools/resources capabilities must be objects")
	}
	tools := ListTools()["tools"].([]map[string]any)
	validArguments := map[string]any{
		"list_threads":            map[string]any{"inbox_id": "inbox-1"},
		"get_thread":              map[string]any{"thread_id": "thread-1"},
		"search_inbox":            map[string]any{"inbox_id": "inbox-1", "query": "hello"},
		"triage_message":          map[string]any{"message_id": "message-1"},
		"extract_to_schema":       map[string]any{"message_id": "message-1", "schema_id": "invoice"},
		"draft_reply_with_policy": map[string]any{"thread_id": "thread-1"},
		"send_reply":              map[string]any{"thread_id": "thread-1", "body_or_draft_id": "reply"},
		"compose_email":           map[string]any{"inbox_id": "inbox-1", "to": "recipient@example.com", "subject": "test", "body": "test"},
	}
	if len(tools) != 8 {
		t.Fatalf("unexpected tool inventory: %d", len(tools))
	}
	for _, tool := range tools {
		schema, ok := tool["inputSchema"].(map[string]any)
		if !ok || schema["type"] != "object" {
			t.Errorf("tool %s lacks required object inputSchema", tool["name"])
			continue
		}
		name := tool["name"].(string)
		raw, err := json.Marshal(schema)
		if err != nil {
			t.Fatal(err)
		}
		compiled, err := jsonschema.CompileString("https://example.invalid/input.json", string(raw))
		if err != nil {
			t.Fatalf("%s inputSchema is invalid: %v", name, err)
		}
		args, present := validArguments[name]
		if !present {
			t.Fatalf("unexpected discovery tool %s", name)
		}
		if err := compiled.Validate(args); err != nil {
			t.Errorf("%s rejects its existing arguments: %v", name, err)
		}
		if err := compiled.Validate(map[string]any{}); err == nil {
			t.Errorf("%s does not describe its required arguments", name)
		}
	}
	for _, resource := range ListResources()["resources"].([]map[string]any) {
		name, ok := resource["name"].(string)
		if !ok || name == "" {
			t.Error("resource lacks required name")
		}
	}
}

func TestStrictClientInitializedAndPing(t *testing.T) {
	runtime := NewServer(config.Default(), nil, nil, nil)
	router := NewRouter(runtime.Config, nil, NewLegacyHandler(runtime), nil)
	init := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, init)
	session := rec.Header().Get("MCP-Session-Id")
	if session == "" {
		t.Fatal("initialize did not establish session")
	}
	for _, item := range []struct {
		body string
		code int
	}{
		{`{"jsonrpc":"2.0","method":"notifications/initialized"}`, http.StatusAccepted},
		{`{"jsonrpc":"2.0","id":2,"method":"ping"}`, http.StatusOK},
	} {
		r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(item.body))
		r.Header.Set("MCP-Protocol-Version", LegacyProtocolVersion)
		r.Header.Set("MCP-Session-Id", session)
		result := httptest.NewRecorder()
		router.ServeHTTP(result, r)
		if result.Code != item.code || (item.code == http.StatusAccepted && result.Body.Len() != 0) || strings.Contains(result.Body.String(), `"error"`) {
			t.Errorf("request=%s status=%d body=%s", item.body, result.Code, result.Body.String())
		}
	}
	badSession := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	badSession.Header.Set("MCP-Protocol-Version", LegacyProtocolVersion)
	badSession.Header.Set("MCP-Session-Id", "foreign-session")
	denied := httptest.NewRecorder()
	router.ServeHTTP(denied, badSession)
	if denied.Code == http.StatusAccepted || !strings.Contains(denied.Body.String(), `"error"`) {
		t.Fatal("initialized notification bypassed its session fence")
	}
}

type heldDiscoveryResponse struct {
	*httptest.ResponseRecorder
	ready   chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (w *heldDiscoveryResponse) hold() { w.once.Do(func() { close(w.ready); <-w.release }) }
func (w *heldDiscoveryResponse) WriteHeader(status int) {
	w.hold()
	w.ResponseRecorder.WriteHeader(status)
}
func (w *heldDiscoveryResponse) Write(p []byte) (int, error) {
	w.hold()
	return w.ResponseRecorder.Write(p)
}

func TestModernDiscoveryOverlapsWithinDefaultMemoryBudget(t *testing.T) {
	for _, mode := range []bool{true, false} {
		t.Run(map[bool]string{true: "json", false: "sse"}[mode], func(t *testing.T) {
			runtime := NewServer(config.Default(), nil, nil, nil)
			handler := NewSDKHandler(runtime, mode)
			makeRequest := func() *http.Request {
				return modernContractRequest(t, "tools/list", map[string]any{"_meta": map[string]any{sdkmcp.MetaKeyProtocolVersion: ModernProtocolVersion, sdkmcp.MetaKeyClientCapabilities: map[string]any{}}})
			}
			release, ready, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			first := &heldDiscoveryResponse{ResponseRecorder: httptest.NewRecorder(), ready: ready, release: release}
			firstRequest := makeRequest()
			go func() { defer close(done); handler.ServeHTTP(first, firstRequest) }()
			defer func() {
				close(release)
				<-done
				if used := runtime.MemoryBudget.Used(); used != 0 {
					t.Errorf("concurrent discovery leaked %d bytes", used)
				}
			}()
			select {
			case <-ready:
			case <-time.After(5 * time.Second):
				t.Fatal("first discovery did not reach response")
			}
			second := httptest.NewRecorder()
			handler.ServeHTTP(second, makeRequest())
			if second.Code != http.StatusOK {
				t.Fatalf("overlapping small discovery exhausted default memory: status=%d body=%s used=%d", second.Code, second.Body.String(), runtime.MemoryBudget.Used())
			}
		})
	}
}

func TestModernWireBudgetChargesBothCopiesForEveryLengthMode(t *testing.T) {
	for _, lengthMode := range []string{"known", "chunked", "zero", "understated"} {
		for _, enough := range []bool{false, true} {
			t.Run(lengthMode+map[bool]string{false: "/exhausted", true: "/exact"}[enough], func(t *testing.T) {
				req := modernContractRequest(t, "tools/list", map[string]any{"_meta": map[string]any{sdkmcp.MetaKeyProtocolVersion: ModernProtocolVersion, sdkmcp.MetaKeyClientCapabilities: map[string]any{}}})
				wireLength := req.ContentLength
				switch lengthMode {
				case "chunked":
					req.ContentLength = -1
				case "zero":
					req.ContentLength = 0
				case "understated":
					req.ContentLength = 1
				}
				cfg := config.Default()
				cfg.Memory.BudgetBytes = 2 * wireLength
				if !enough {
					cfg.Memory.BudgetBytes--
				}
				runtime := NewServer(cfg, nil, nil, nil)
				rec := httptest.NewRecorder()
				NewSDKHandler(runtime, true).ServeHTTP(rec, req)
				want := http.StatusServiceUnavailable
				if enough {
					want = http.StatusOK
				}
				if rec.Code != want {
					t.Fatalf("status=%d want=%d body=%s", rec.Code, want, rec.Body.String())
				}
				if runtime.MemoryBudget.Used() != 0 {
					t.Fatal("wire reservation leaked")
				}
			})
		}
	}
}

func TestModernToolCallsReserveDecodedAllowanceBeforeDispatch(t *testing.T) {
	for _, enough := range []bool{false, true} {
		t.Run(map[bool]string{false: "exhausted", true: "exact"}[enough], func(t *testing.T) {
			req := modernContractRequest(t, "tools/call", map[string]any{"name": "unknown-tool", "arguments": map[string]any{}, "_meta": modernOAuthMeta()})
			req.Header.Set("Mcp-Name", "unknown-tool")
			cfg := config.Default()
			cfg.Memory.BudgetBytes = 2*req.ContentLength + maxModernDecodedAttachmentBytes
			if !enough {
				cfg.Memory.BudgetBytes--
			}
			runtime := NewServer(cfg, nil, nil, nil)
			rec := httptest.NewRecorder()
			NewSDKHandler(runtime, true).ServeHTTP(rec, req)
			want := http.StatusServiceUnavailable
			if enough {
				want = http.StatusBadRequest // The funded request reaches SDK unknown-tool validation.
			}
			if rec.Code != want {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, want, rec.Body.String())
			}
			if enough && !strings.Contains(rec.Body.String(), `"code":-32602`) {
				t.Fatalf("funded call did not reach SDK tool validation: %s", rec.Body.String())
			}
			if runtime.MemoryBudget.Used() != 0 {
				t.Fatal("decoded allowance leaked")
			}
		})
	}
}

func TestModernBatchCannotBypassPerCallBudgetOrHeaders(t *testing.T) {
	for _, version := range []string{LegacyProtocolVersion, ModernProtocolVersion, "2025-03-26"} {
		t.Run(version, func(t *testing.T) {
			runtime := NewServer(config.Default(), nil, nil, nil)
			r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"compose_email","arguments":{}}}]`))
			r.Header.Set("MCP-Protocol-Version", version)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Accept", "application/json, text/event-stream")
			rec := httptest.NewRecorder()
			NewSDKHandler(runtime, true).ServeHTTP(rec, r)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "batches are not supported") {
				t.Fatalf("batch reached SDK: status=%d body=%s", rec.Code, rec.Body.String())
			}
			if runtime.MemoryBudget.Used() != 0 {
				t.Fatal("batch rejection leaked wire reservation")
			}
		})
	}
}

func TestStdioInitializedDoesNotEmitAResponse(t *testing.T) {
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
	_, err = input.WriteString("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-11-25\"}}\n{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"ping\"}\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = input.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = input, output
	defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()
	if err = RunStdio(context.Background(), NewServer(config.Default(), nil, nil, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err = output.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(output)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("initialized must not emit a response: %s", raw)
	}
	for _, line := range lines {
		if strings.Contains(line, `"error"`) || strings.Contains(line, `"id":null`) {
			t.Fatalf("invalid stdio response: %s", line)
		}
	}
}
