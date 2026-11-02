package mcp

import (
	"encoding/json"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestLegacyResourceContentBindsURIAndPreservesStoredObject(t *testing.T) {
	payload := map[string]any{"inbox_ids": []string{"inbox-1"}}
	before, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	result, err := legacyResultForClient(Request{Method: "resources/read", Params: json.RawMessage(`{"uri":"email://inboxes"}`)}, payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var strict sdkmcp.ReadResourceResult
	if err = json.Unmarshal(raw, &strict); err != nil {
		t.Fatal(err)
	}
	if len(strict.Contents) != 1 || strict.Contents[0].URI != "email://inboxes" || strict.Contents[0].MIMEType != "application/json" || strict.Contents[0].Text != string(before) {
		t.Fatalf("strict resource lost URI/payload binding: %s", raw)
	}
	after, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("resource adapter mutated the stored object")
	}
	if _, exists := payload["contents"]; exists {
		t.Fatal("protocol fields leaked into business storage")
	}
	if got := result.(map[string]any)["inbox_ids"].([]string); len(got) != 1 || got[0] != "inbox-1" {
		t.Fatal("legacy SDK field was lost")
	}
}

func TestLegacyResultAdapterRefusesAmbiguousProtocolFields(t *testing.T) {
	for _, item := range []struct {
		name    string
		req     Request
		payload any
	}{
		{"tool collision", Request{Method: "tools/call"}, map[string]any{"content": "legacy"}},
		{"resource collision", Request{Method: "resources/read", Params: json.RawMessage(`{"uri":"email://inboxes"}`)}, map[string]any{"contents": "legacy"}},
		{"nonobject", Request{Method: "tools/call"}, "text"},
		{"unencodable", Request{Method: "tools/call"}, map[string]any{"bad": func() {}}},
		{"invalid resource params", Request{Method: "resources/read", Params: json.RawMessage(`false`)}, map[string]any{"inbox_ids": []string{}}},
		{"unsupported method", Request{Method: "initialize"}, map[string]any{}},
	} {
		t.Run(item.name, func(t *testing.T) {
			if result, err := legacyResultForClient(item.req, item.payload); err == nil || result != nil {
				t.Fatalf("ambiguous result admitted: %#v %v", result, err)
			}
		})
	}
}
