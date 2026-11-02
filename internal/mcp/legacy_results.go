package mcp

import (
	"encoding/json"
	"errors"
)

// legacyResultForClient adds the protocol envelope without changing the
// business object held by audit/idempotency storage. Original fields remain
// available to the published 0.2 SDK; strict clients read the standard content.
func legacyResultForClient(req Request, result any) (any, error) {
	fields, ok := result.(map[string]any)
	if !ok {
		return nil, errors.New("MCP result must be an object")
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil, errors.New("cannot encode MCP result")
	}
	out := make(map[string]any, len(fields)+1)
	for key, value := range fields {
		out[key] = value
	}
	switch req.Method {
	case "tools/call":
		if _, exists := fields["content"]; exists {
			return nil, errors.New("business result uses reserved MCP content field")
		}
		out["content"] = []map[string]any{{"type": "text", "text": string(raw)}}
	case "resources/read":
		var params ResourceReadParams
		if err := decodeParams(req.Params, &params); err != nil {
			return nil, err
		}
		if _, exists := fields["contents"]; exists {
			return nil, errors.New("business result uses reserved MCP contents field")
		}
		out["contents"] = []map[string]any{{"uri": params.URI, "mimeType": "application/json", "text": string(raw)}}
	default:
		return nil, errors.New("unsupported MCP result adapter")
	}
	return out, nil
}
