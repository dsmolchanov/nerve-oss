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
	Data    any    `json:"data,omitempty"`
}

type ToolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type ResourceReadParams struct {
	URI string `json:"uri"`
}

type InitializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
}

func ListTools(attachmentsEnabled ...bool) map[string]any {
	enabled := len(attachmentsEnabled) > 0 && attachmentsEnabled[0]
	return map[string]any{
		"tools": []map[string]any{
			{"name": "list_threads", "description": "List threads in an inbox"},
			{"name": "get_thread", "description": "Fetch a thread with messages"},
			{"name": "search_inbox", "description": "Semantic search over an inbox"},
			{"name": "triage_message", "description": "Classify intent, urgency, sentiment"},
			{"name": "extract_to_schema", "description": "Extract structured data"},
			{"name": "draft_reply_with_policy", "description": "Draft a reply constrained by policy"},
			{"name": "send_reply", "description": "Send a reply", "inputSchema": sendReplyInputSchema(enabled)},
			{"name": "compose_email", "description": "Compose and send a new email (not a reply)", "inputSchema": composeEmailInputSchema(enabled)},
		},
	}
}

func attachmentInputSchema() map[string]any {
	return map[string]any{
		"type":     "array",
		"maxItems": 10,
		"items": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"filename": map[string]any{"type": "string", "minLength": 1, "maxLength": 255},
				"content_type": map[string]any{
					"type": "string",
					"enum": []string{
						"image/png",
						"image/jpeg",
						"image/webp",
						"application/pdf",
						"text/plain",
						"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
						"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
					},
				},
				"content_base64": map[string]any{"type": "string", "contentEncoding": "base64"},
			},
			"required": []string{"filename", "content_type", "content_base64"},
		},
	}
}

func sendReplyInputSchema(attachmentsEnabled bool) map[string]any {
	properties := map[string]any{
		"thread_id":            map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
		"body_or_draft_id":     map[string]any{"type": "string", "maxLength": 10 << 20},
		"idempotency_key":      map[string]any{"type": "string", "maxLength": 128},
		"html":                 map[string]any{"type": "string", "maxLength": 10 << 20},
		"needs_human_approval": map[string]any{"type": "boolean", "default": false},
	}
	if attachmentsEnabled {
		properties["attachments"] = attachmentInputSchema()
	}
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           properties,
		"required":             []string{"thread_id", "body_or_draft_id"},
	}
}

func composeEmailInputSchema(attachmentsEnabled bool) map[string]any {
	properties := map[string]any{
		"inbox_id":        map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
		"to":              map[string]any{"type": "string", "format": "email", "pattern": `^[^\s@]+@[^\s@]+\.[^\s@]+$`, "maxLength": 320},
		"subject":         map[string]any{"type": "string", "maxLength": 998},
		"from_name":       map[string]any{"type": "string", "maxLength": 256},
		"idempotency_key": map[string]any{"type": "string", "maxLength": 128},
		"body":            map[string]any{"type": "string", "maxLength": 10 << 20},
		"html":            map[string]any{"type": "string", "maxLength": 10 << 20},
	}
	if attachmentsEnabled {
		properties["attachments"] = attachmentInputSchema()
	}
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           properties,
		"required":             []string{"inbox_id", "to", "subject"},
		"anyOf":                []map[string]any{{"required": []string{"body"}}, {"required": []string{"html"}}},
	}
}

func ListResources() map[string]any {
	return map[string]any{
		"resources": []map[string]any{
			{"uri": "email://inboxes", "description": "List inbox IDs"},
		},
	}
}
