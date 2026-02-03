package mcp

import "encoding/json"

type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type Response struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id"`
	Result  any            `json:"result,omitempty"`
	Error   *ResponseError `json:"error,omitempty"`
}

type ResponseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type ToolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type ResourceReadParams struct {
	URI string `json:"uri"`
}

func ListTools() map[string]any {
	return map[string]any{
		"tools": []map[string]any{
			{"name": "list_threads", "description": "List threads in an inbox"},
			{"name": "get_thread", "description": "Fetch a thread with messages"},
			{"name": "search_inbox", "description": "Semantic search over an inbox"},
			{"name": "triage_message", "description": "Classify intent, urgency, sentiment"},
			{"name": "extract_to_schema", "description": "Extract structured data"},
			{"name": "draft_reply_with_policy", "description": "Draft a reply constrained by policy"},
			{"name": "send_reply", "description": "Send a reply"},
		},
	}
}

func ListResources() map[string]any {
	return map[string]any{
		"resources": []map[string]any{
			{"uri": "email://inboxes", "description": "List inbox IDs"},
		},
	}
}
