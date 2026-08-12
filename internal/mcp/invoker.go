package mcp

import (
	"context"
	"encoding/json"
	"errors"

	"neuralmail/internal/auth"
)

// ToolInvocation is the protocol-neutral input consumed by both MCP adapters.
// Header and envelope metadata are adapter concerns and cannot select a tool.
type ToolInvocation struct {
	Name      string
	Arguments json.RawMessage
}

type Invoker struct {
	server *Server
}

func ToolInvocationFromRequest(request Request) (ToolInvocation, error) {
	var params ToolCallParams
	if err := decodeParams(request.Params, &params); err != nil {
		return ToolInvocation{}, err
	}
	return ToolInvocation{Name: params.Name, Arguments: params.Arguments}, nil
}

func (invoker *Invoker) Invoke(ctx context.Context, invocation ToolInvocation) (any, error) {
	if invoker == nil || invoker.server == nil {
		return nil, errors.New("MCP invoker is unavailable")
	}
	if invocation.Name == "" {
		return nil, errors.New("tool name is required")
	}
	if invoker.server.Config.Cloud.Mode {
		principal, ok := auth.PrincipalFromContext(ctx)
		if !ok || invoker.server.Auth == nil {
			return nil, errors.New("missing cloud principal")
		}
		if err := invoker.server.Auth.ValidateScopes(principal, requiredToolScope(principal, invocation.Name)); err != nil {
			return nil, err
		}
	}
	return invoker.invokeTool(ctx, ToolCallParams{
		Name: invocation.Name, Arguments: invocation.Arguments,
	})
}

func requiredToolScope(principal auth.Principal, toolName string) string {
	switch toolName {
	case "list_threads", "get_thread":
		return "nerve:email.read"
	case "search_inbox":
		return "nerve:email.search"
	case "triage_message", "extract_to_schema", "draft_reply_with_policy":
		return "nerve:email.draft"
	case "send_reply":
		if principal.Kind == auth.PrincipalM2MOrg {
			return "nerve:email.reply"
		}
		return "nerve:email.send"
	case "compose_email":
		if principal.Kind == auth.PrincipalM2MOrg {
			return "nerve:email.compose"
		}
		return "nerve:email.send"
	default:
		return "nerve:email.read"
	}
}
