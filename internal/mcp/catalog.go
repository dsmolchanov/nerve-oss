package mcp

import (
	"context"
	"encoding/json"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"neuralmail/internal/auth"
)

type toolDescriptor struct {
	Name        string
	Description string
	InputSchema any
	OutputShape map[string]any
}

func modernToolCatalog(ctx context.Context, server *Server, principal auth.Principal) []toolDescriptor {
	if principal.Kind == auth.PrincipalM2MOnboarding {
		// Lifecycle tools are registered only when Phase 3 supplies the delegated
		// onboarding implementation. An empty list is a fail-closed profile.
		return nil
	}
	tools := modernToolDescriptors(ctx, server, principal)
	if !server.Config.Cloud.Mode || server.Auth == nil {
		return tools
	}
	visible := tools[:0]
	for _, tool := range tools {
		if server.Auth.ValidateScopes(principal, requiredToolScope(principal, tool.Name)) != nil {
			continue
		}
		if principal.Kind == auth.PrincipalM2MOrg && isOutboundTool(tool.Name) {
			if server.OutboundPolicy == nil || server.OutboundPolicy.Authorize(ctx, principal, tool.Name, nil) != nil {
				continue
			}
		}
		visible = append(visible, tool)
	}
	return visible
}

func modernToolDescriptors(ctx context.Context, server *Server, principal auth.Principal) []toolDescriptor {
	attachmentsEnabled := server.attachmentsEnabled(auth.WithPrincipal(ctx, principal))
	tools := []toolDescriptor{
		{Name: "list_threads", Description: "List threads in an inbox", InputSchema: inputObject(
			map[string]any{"inbox_id": uuidStringProperty(), "status": boundedStringProperty(0, 32), "limit": map[string]any{"type": "integer", "minimum": 0, "maximum": 100}},
			"inbox_id"), OutputShape: outputObject(map[string]any{"threads": arrayProperty()}, "threads")},
		{Name: "get_thread", Description: "Fetch a thread with messages", InputSchema: inputObject(
			map[string]any{"thread_id": uuidStringProperty()}, "thread_id"), OutputShape: outputObject(
			map[string]any{"thread": objectProperty(), "messages": arrayProperty()}, "thread", "messages")},
		{Name: "search_inbox", Description: "Semantic search over an inbox", InputSchema: inputObject(
			map[string]any{"inbox_id": uuidStringProperty(), "query": boundedStringProperty(1, 4_096), "top_k": map[string]any{"type": "integer", "minimum": 0, "maximum": 100}},
			"inbox_id", "query"), OutputShape: outputObject(map[string]any{"results": arrayProperty()}, "results")},
		{Name: "triage_message", Description: "Classify intent, urgency, sentiment", InputSchema: inputObject(
			map[string]any{"message_id": uuidStringProperty()}, "message_id"), OutputShape: outputObject(map[string]any{
			"intent": stringProperty(0), "urgency": stringProperty(0), "sentiment": stringProperty(0),
			"confidence": map[string]any{"type": "number"}, "suggested_route": stringProperty(0),
		}, "intent", "urgency", "sentiment", "confidence", "suggested_route")},
		{Name: "extract_to_schema", Description: "Extract structured data", InputSchema: inputObject(
			map[string]any{"message_id": uuidStringProperty(), "schema_id": schemaIDProperty()}, "message_id", "schema_id"),
			OutputShape: outputObject(map[string]any{
				"data": map[string]any{}, "confidence": map[string]any{"type": "number"},
				"missing_fields": arrayProperty(), "validation_errors": arrayProperty(),
			}, "data", "confidence", "missing_fields", "validation_errors")},
		{Name: "draft_reply_with_policy", Description: "Draft a reply constrained by policy", InputSchema: inputObject(
			map[string]any{"thread_id": uuidStringProperty(), "goal": boundedStringProperty(0, 4_096)}, "thread_id"), OutputShape: outputObject(map[string]any{
			"draft": stringProperty(0), "risk_flags": arrayProperty(), "cited_message_ids": map[string]any{"type": []string{"array", "null"}},
			"needs_human_approval": map[string]any{"type": "boolean"}, "policy_blocked": map[string]any{"type": "boolean"}, "reason": stringProperty(0),
		}, "draft", "risk_flags", "cited_message_ids", "needs_human_approval")},
		{Name: "send_reply", Description: "Send a reply", InputSchema: modernSendReplyInputSchema(attachmentsEnabled), OutputShape: queuedMessageOutput()},
		{Name: "compose_email", Description: "Compose and send a new email (not a reply)", InputSchema: modernComposeEmailInputSchema(attachmentsEnabled), OutputShape: composeQueuedMessageOutput()},
	}
	return tools
}

func inputObject(properties map[string]any, required ...string) map[string]any {
	return map[string]any{
		"$schema":              "https://json-schema.org/draft/2020-12/schema",
		"type":                 "object",
		"additionalProperties": false,
		"properties":           properties,
		"required":             required,
	}
}

func modernInputSchema(schema map[string]any) map[string]any {
	schema["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	return schema
}

func modernSendReplyInputSchema(attachmentsEnabled bool) map[string]any {
	schema := sendReplyInputSchema(attachmentsEnabled)
	properties := schema["properties"].(map[string]any)
	properties["thread_id"] = uuidStringProperty()
	properties["body_or_draft_id"] = boundedStringProperty(1, 10<<20)
	properties["idempotency_key"] = boundedStringProperty(0, 128)
	properties["html"] = boundedStringProperty(0, 10<<20)
	return modernInputSchema(schema)
}

func modernComposeEmailInputSchema(attachmentsEnabled bool) map[string]any {
	schema := composeEmailInputSchema(attachmentsEnabled)
	properties := schema["properties"].(map[string]any)
	properties["inbox_id"] = uuidStringProperty()
	properties["to"] = map[string]any{
		"type": "string", "format": "email", "pattern": `^[^\s@]+@[^\s@]+\.[^\s@]+$`, "maxLength": 320,
	}
	properties["subject"] = boundedStringProperty(1, 998)
	properties["from_name"] = boundedStringProperty(0, 256)
	properties["idempotency_key"] = boundedStringProperty(0, 128)
	properties["body"] = boundedStringProperty(1, 10<<20)
	properties["html"] = boundedStringProperty(1, 10<<20)
	return modernInputSchema(schema)
}

func stringProperty(minLength int) map[string]any {
	property := map[string]any{"type": "string"}
	if minLength > 0 {
		property["minLength"] = minLength
	}
	return property
}

func boundedStringProperty(minLength, maxLength int) map[string]any {
	property := stringProperty(minLength)
	property["maxLength"] = maxLength
	return property
}

// schemaIDProperty mirrors tools.LoadSchema's accepted identifier so a
// traversal attempt fails at the MCP boundary rather than at the file read.
func schemaIDProperty() map[string]any {
	return map[string]any{
		"type": "string", "minLength": 1, "maxLength": 64,
		"pattern": `^[a-z0-9][a-z0-9_-]{0,63}$`,
	}
}

func uuidStringProperty() map[string]any {
	return map[string]any{
		"type": "string", "format": "uuid", "minLength": 36, "maxLength": 36,
		"pattern": `^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`,
	}
}

func objectProperty() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": true}
}

func arrayProperty() map[string]any {
	return map[string]any{"type": []string{"array", "null"}, "items": map[string]any{}}
}

func outputObject(properties map[string]any, required ...string) map[string]any {
	properties["replay_id"] = stringProperty(0)
	properties["audit_id"] = stringProperty(0)
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           properties,
		"required":             required,
	}
}

func queuedMessageOutput() map[string]any {
	return outputObject(map[string]any{
		"message_id": stringProperty(1),
		"status":     map[string]any{"type": "string", "enum": []string{"queued"}},
	}, "message_id", "status")
}

func composeQueuedMessageOutput() map[string]any {
	return outputObject(map[string]any{
		"thread_id":  stringProperty(1),
		"message_id": stringProperty(1),
		"status":     map[string]any{"type": "string", "enum": []string{"queued"}},
	}, "thread_id", "message_id", "status")
}

func modernErrorOutput() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"error": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"code":      stringProperty(1),
					"retryable": map[string]any{"type": "boolean"},
					"retry_at":  map[string]any{"type": "string", "format": "date-time"},
				},
				"required": []string{"code", "retryable"},
			},
		},
		"required": []string{"error"},
	}
}

func sdkTool(descriptor toolDescriptor) *sdkmcp.Tool {
	return &sdkmcp.Tool{
		Name: descriptor.Name, Description: descriptor.Description,
		InputSchema: descriptor.InputSchema,
		OutputSchema: map[string]any{
			"$schema": "https://json-schema.org/draft/2020-12/schema",
			"oneOf":   []any{descriptor.OutputShape, modernErrorOutput()},
		},
	}
}

func rawJSON(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return `{"error":"result_encoding_failed"}`
	}
	return string(raw)
}
