package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"neuralmail/internal/auth"
	"neuralmail/internal/store"
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
		if principal.Kind == auth.PrincipalM2MOrg && isOutboundTool(invocation.Name) {
			if invoker.server.OutboundPolicy == nil {
				return nil, &outboundPolicyError{Code: "outbound_policy_unavailable"}
			}
			if err := invoker.server.OutboundPolicy.Authorize(ctx, principal, invocation.Name, invocation.Arguments); err != nil {
				return nil, err
			}
		}
	}
	return invoker.invokeTool(ctx, ToolCallParams{
		Name: invocation.Name, Arguments: invocation.Arguments,
	})
}

func isOutboundTool(toolName string) bool {
	return toolName == "send_reply" || toolName == "compose_email"
}

type outboundPolicyError struct {
	Code string
}

func (err *outboundPolicyError) Error() string {
	return err.Code
}

type outboundPolicyStore interface {
	LookupFeatureFlagForOrg(context.Context, string, string) (store.FeatureFlagValues, error)
	GetInboxRecordByIDForOrg(context.Context, string, string) (store.InboxRecord, error)
	ListInboxRecordsByOrg(context.Context, string) ([]store.InboxRecord, error)
	GetOrgDomainByIDForOrg(context.Context, string, string) (store.OrgDomain, error)
	GetThread(context.Context, string) (store.Thread, []store.Message, error)
	GetMessage(context.Context, string) (store.Message, error)
}

type storeOutboundPolicyGate struct {
	store outboundPolicyStore
}

func (gate *storeOutboundPolicyGate) Authorize(ctx context.Context, principal auth.Principal, toolName string, arguments json.RawMessage) error {
	if principal.Kind != auth.PrincipalM2MOrg || !isOutboundTool(toolName) {
		return nil
	}
	if gate == nil || gate.store == nil || principal.OrgID == "" {
		return &outboundPolicyError{Code: "outbound_policy_unavailable"}
	}
	required := []struct {
		flag string
		want bool
	}{
		{flag: "autonomous_outbound_policy", want: true},
		{flag: "email_outbound_suspended", want: false},
	}
	for _, requirement := range required {
		values, err := gate.store.LookupFeatureFlagForOrg(ctx, principal.OrgID, requirement.flag)
		if err != nil {
			return &outboundPolicyError{Code: "outbound_policy_unavailable"}
		}
		if values.Org == nil || *values.Org != requirement.want {
			return &outboundPolicyError{Code: fmt.Sprintf("%s_denied", requirement.flag)}
		}
	}
	if toolName == "send_reply" && len(arguments) != 0 {
		allowed, err := gate.hasRealInboundReplyTarget(ctx, principal.OrgID, arguments)
		if err != nil {
			return &outboundPolicyError{Code: "outbound_policy_unavailable"}
		}
		if !allowed {
			return &outboundPolicyError{Code: "inbound_reply_policy_denied"}
		}
	}
	if toolName == "compose_email" {
		values, err := gate.store.LookupFeatureFlagForOrg(ctx, principal.OrgID, "email_compose_org_enabled")
		if err != nil || values.Org == nil {
			return &outboundPolicyError{Code: "outbound_policy_unavailable"}
		}
		if *values.Org {
			return nil
		}
		allowed, err := gate.hasReadyOwnedComposeInbox(ctx, principal.OrgID, arguments)
		if err != nil {
			return &outboundPolicyError{Code: "outbound_policy_unavailable"}
		}
		if !allowed {
			return &outboundPolicyError{Code: "email_compose_org_enabled_denied"}
		}
	}
	return nil
}

func (gate *storeOutboundPolicyGate) hasReadyOwnedComposeInbox(ctx context.Context, orgID string, arguments json.RawMessage) (bool, error) {
	var inboxes []store.InboxRecord
	if len(arguments) == 0 {
		var err error
		inboxes, err = gate.store.ListInboxRecordsByOrg(ctx, orgID)
		if err != nil {
			return false, err
		}
	} else {
		var input struct {
			InboxID string `json:"inbox_id"`
		}
		if err := json.Unmarshal(arguments, &input); err != nil || input.InboxID == "" {
			return false, err
		}
		inbox, err := gate.store.GetInboxRecordByIDForOrg(ctx, orgID, input.InboxID)
		if err != nil {
			return false, err
		}
		inboxes = []store.InboxRecord{inbox}
	}

	for _, inbox := range inboxes {
		if inbox.Status != "active" || !inbox.OrgDomainID.Valid {
			continue
		}
		domain, err := gate.store.GetOrgDomainByIDForOrg(ctx, orgID, inbox.OrgDomainID.String)
		if err != nil {
			continue
		}
		at := strings.LastIndexByte(inbox.Address, '@')
		if domain.Status == "active" && domain.MXVerified && domain.SPFVerified && domain.DKIMVerified &&
			domain.InboundEnabled && domain.ResendReceivingEnabled &&
			at >= 0 && strings.EqualFold(inbox.Address[at+1:], domain.Domain) {
			return true, nil
		}
	}
	return false, nil
}

func (gate *storeOutboundPolicyGate) hasRealInboundReplyTarget(ctx context.Context, orgID string, arguments json.RawMessage) (bool, error) {
	var input struct {
		ThreadID string `json:"thread_id"`
	}
	if err := json.Unmarshal(arguments, &input); err != nil {
		return false, err
	}
	if input.ThreadID == "" {
		return false, nil
	}
	thread, messages, err := gate.store.GetThread(ctx, input.ThreadID)
	if err != nil {
		return false, err
	}
	if len(messages) == 0 {
		return false, nil
	}
	inbox, err := gate.store.GetInboxRecordByIDForOrg(ctx, orgID, thread.InboxID)
	if err != nil {
		return false, err
	}
	latest := messages[len(messages)-1]
	if inbox.Status != "active" || latest.Direction != "inbound" || latest.InboxID != thread.InboxID {
		return false, nil
	}
	message, err := gate.store.GetMessage(ctx, latest.ID)
	if err != nil {
		return false, err
	}
	return message.ThreadID == thread.ID && message.InboxID == thread.InboxID &&
		message.Direction == "inbound" && message.ReceivedEmailID != "" &&
		strings.TrimSpace(message.From.Email) != "", nil
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
