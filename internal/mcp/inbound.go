package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"neuralmail/internal/auth"
)

const InboundDelegationPath = "/internal/v1/agent-inbound/"
const InboundUsageTool = "nerve_inbound_usage"
const InboundReceiptsTool = "nerve_inbound_receipts"
const InboundRecoverTool = "nerve_inbound_recover"

// InboundCaller is authority captured from the authenticated modern request.
// None of its fields can be selected by tool arguments.
type InboundCaller struct {
	Principal     auth.Principal
	Authorization string
}
type InboundInput struct {
	Limit     int    `json:"limit,omitempty"`
	Cursor    string `json:"cursor,omitempty"`
	ReceiptID string `json:"receipt_id,omitempty"`
}
type InboundProvisioner interface {
	Inbound(context.Context, InboundCaller, string, InboundInput) (json.RawMessage, error)
}
type InboundBusinessError struct {
	Code      string
	Retryable bool
}

func (e *InboundBusinessError) Error() string { return e.Code }
func ValidInboundError(e *InboundBusinessError) bool {
	if e == nil {
		return false
	}
	switch e.Code {
	case "inbound_invalid_request", "inbound_unavailable", "recovery_response_exceeds_limit":
		return !e.Retryable
	case "inbound_retry_later", "inbound_outcome_unknown", "recovery_in_progress_or_changed", "recovery_provider_or_save_unavailable":
		return e.Retryable
	}
	return false
}
func inboundUnavailable() error {
	return &InboundBusinessError{Code: "inbound_retry_later", Retryable: true}
}
func ValidInboundCaller(c InboundCaller) bool {
	p := c.Principal
	if p.Kind != auth.PrincipalM2MOrg || p.AuthMethod != "m2m_bearer" || p.ClientID == "" || p.OrgID == "" || p.Generation < 1 || p.TokenID == "" {
		return false
	}
	scope := false
	for _, s := range p.Scopes {
		if s == "nerve:email.read" {
			scope = true
		}
	}
	parts := strings.Fields(c.Authorization)
	return scope && len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") && parts[1] != "" && strings.TrimSpace(c.Authorization) == c.Authorization && !strings.ContainsAny(c.Authorization, "\r\n")
}
func inboundToolOperation(name string) string {
	switch name {
	case InboundUsageTool:
		return "usage"
	case InboundReceiptsTool:
		return "receipts"
	case InboundRecoverTool:
		return "recover"
	}
	return ""
}
func inboundToolsAvailable(s *Server, p auth.Principal) bool {
	return s != nil && s.Config.Cloud.Mode && s.Inbound != nil && s.Auth != nil && ValidInboundCaller(InboundCaller{Principal: p, Authorization: "Bearer visibility"})
}
func inboundToolDescriptors() []toolDescriptor {
	return []toolDescriptor{
		{Name: InboundUsageTool, Description: "Read admitted inbound usage, pending slots and receipt completeness", InputSchema: inputObject(map[string]any{}), OutputShape: inboundOutputSchema("usage"), ErrorCodes: inboundErrorCodes()},
		{Name: InboundReceiptsTool, Description: "List bounded content-free inbound receipts; opaque IDs never grant access", InputSchema: inputObject(map[string]any{"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}, "cursor": map[string]any{"type": "string", "maxLength": 66}}), OutputShape: inboundOutputSchema("receipts"), ErrorCodes: inboundErrorCodes()},
		{Name: InboundRecoverTool, Description: "Attempt recovery of one receipt and paused attachments; the provider deadline is an attempt window, not a guarantee", InputSchema: inputObject(map[string]any{"receipt_id": map[string]any{"type": "string", "minLength": 36, "maxLength": 66}}, "receipt_id"), OutputShape: inboundOutputSchema("recover"), ErrorCodes: inboundErrorCodes()},
	}
}
func inboundOutputSchema(op string) map[string]any {
	// Validation below is the authoritative closed, content-free wire contract.
	props := map[string]any{}
	switch op {
	case "usage":
		for _, k := range []string{"enrolled", "receipt_overflow", "usage_complete"} {
			props[k] = map[string]any{"type": "boolean"}
		}
		for _, k := range []string{"soft_limit", "hard_limit", "reserved", "materialized"} {
			props[k] = map[string]any{"type": "integer", "minimum": 0}
		}
		for _, k := range []string{"period_id", "starts_at", "ends_at"} {
			props[k] = boundedStringProperty(0, 64)
		}
		return outputObject(props, "enrolled", "receipt_overflow", "usage_complete", "soft_limit", "hard_limit", "reserved", "materialized")
	case "receipts":
		return outputObject(map[string]any{"items": map[string]any{"type": "array", "maxItems": 100, "items": inboundReceiptSchema()}, "next_cursor": boundedStringProperty(0, 66)}, "items")
	default:
		for _, k := range []string{"receipt_id", "state", "reason", "message_id", "provider_created_at", "recovery_attempt_deadline"} {
			props[k] = boundedStringProperty(0, 66)
		}
		props["attachment_retry_state"] = map[string]any{"type": "string", "enum": []string{"evaluated", "retry_later"}}
		props["attachments_reopened"] = map[string]any{"type": "integer", "minimum": 0, "maximum": 100}
		return outputObject(props, "receipt_id", "state")
	}
}
func inboundReceiptSchema() map[string]any {
	props := map[string]any{}
	for _, k := range []string{"id", "inbox_id", "period_id", "state", "reason", "created_at", "expires_at", "message_id", "provider_created_at", "recovery_attempt_deadline"} {
		props[k] = boundedStringProperty(0, 66)
	}
	return outputObject(props, "id", "inbox_id", "period_id", "state", "created_at", "expires_at")
}
func ValidInboundInput(op string, i InboundInput) bool {
	switch op {
	case "usage":
		return i == (InboundInput{})
	case "receipts":
		return i.ReceiptID == "" && i.Limit >= 1 && i.Limit <= 100 && (i.Cursor == "" || OpaqueInboundID(i.Cursor))
	case "recover":
		return i.Limit == 0 && i.Cursor == "" && OpaqueInboundID(i.ReceiptID)
	}
	return false
}
func OpaqueInboundID(id string) bool {
	if len(id) == 66 && strings.HasPrefix(id, "q_") {
		for _, c := range id[2:] {
			if !strings.ContainsRune("0123456789abcdef", c) {
				return false
			}
		}
		return true
	}
	u, e := uuid.Parse(id)
	return e == nil && u != uuid.Nil && u.String() == id
}
func invokeInboundTool(ctx context.Context, p InboundProvisioner, c InboundCaller, name string, raw json.RawMessage) (any, error) {
	if p == nil || !ValidInboundCaller(c) {
		return nil, &InboundBusinessError{Code: "inbound_unavailable"}
	}
	op := inboundToolOperation(name)
	i, e := DecodeInboundInput(op, raw)
	if e != nil {
		return nil, e
	}
	result, e := p.Inbound(ctx, c, op, i)
	if e != nil {
		var public *InboundBusinessError
		if errors.As(e, &public) && ValidInboundError(public) {
			return nil, public
		}
		return nil, inboundUnavailable()
	}
	return ValidateInboundResult(op, i, result)
}

// An invalid success is not proof that a requested recovery did not commit.
// Reads can be repeated; a recovery must first be reconciled from its receipt.
func inboundInvalidResult(operation string) error {
	if operation == "recover" {
		return &InboundBusinessError{Code: "inbound_outcome_unknown", Retryable: true}
	}
	return inboundUnavailable()
}

// ValidateInboundResult prevents provider IDs, bodies, sender/recipient metadata,
// impossible counters and wrong receipt identities crossing the MCP boundary.
func ValidateInboundResult(op string, i InboundInput, raw []byte) (map[string]any, error) {
	d, e := inboundObject(raw)
	if e != nil || !ValidInboundInput(op, i) {
		return nil, inboundInvalidResult(op)
	}
	bad := false
	switch op {
	case "usage":
		bad = !inboundKeys(d, "enrolled receipt_overflow usage_complete soft_limit hard_limit reserved materialized period_id starts_at ends_at")
		for _, k := range []string{"enrolled", "receipt_overflow", "usage_complete"} {
			if _, ok := d[k].(bool); !ok {
				bad = true
			}
		}
		ns := map[string]int64{}
		for _, k := range []string{"soft_limit", "hard_limit", "reserved", "materialized"} {
			n, ok := inboundInteger(d[k])
			if !ok {
				bad = true
			}
			ns[k] = n
		}
		bad = bad || ns["soft_limit"] > ns["hard_limit"] || ns["reserved"] > ns["hard_limit"] || ns["materialized"] > ns["hard_limit"]-ns["reserved"]
		if enrolled, _ := d["enrolled"].(bool); enrolled {
			a, ok := inboundTime(d["starts_at"])
			b, ok2 := inboundTime(d["ends_at"])
			id, _ := d["period_id"].(string)
			bad = bad || !ok || !ok2 || !b.After(a) || !inboundUUID(id)
		} else {
			bad = bad || ns["soft_limit"] != 0 || ns["hard_limit"] != 0 || ns["reserved"] != 0 || ns["materialized"] != 0
			for _, k := range []string{"period_id", "starts_at", "ends_at"} {
				if _, ok := d[k]; ok {
					bad = true
				}
			}
		}
	case "receipts":
		bad = !inboundKeys(d, "items next_cursor")
		items, ok := d["items"].([]any)
		bad = bad || !ok || len(items) > i.Limit
		seen := map[string]bool{}
		for _, v := range items {
			item, ok := v.(map[string]any)
			if !ok || !validInboundReceipt(item) {
				bad = true
				continue
			}
			id := item["id"].(string)
			if seen[id] {
				bad = true
			}
			seen[id] = true
		}
		if c, ok := d["next_cursor"]; ok {
			s, ok := c.(string)
			bad = bad || !ok || !OpaqueInboundID(s) || len(items) == 0 || s == i.Cursor
		}
	case "recover":
		bad = !inboundKeys(d, "receipt_id state reason message_id attachments_reopened attachment_retry_state provider_created_at recovery_attempt_deadline") || d["receipt_id"] != i.ReceiptID || !validInboundState(d, false) || !validInboundDeadline(d)
		if v, ok := d["attachment_retry_state"]; ok {
			bad = bad || d["state"] != "materialized" || (v != "evaluated" && v != "retry_later")
		}
		if v, ok := d["attachments_reopened"]; ok {
			n, valid := inboundInteger(v)
			bad = bad || !valid || n > 100 || (d["state"] != "materialized" && n != 0)
		}
	default:
		bad = true
	}
	if bad {
		return nil, inboundInvalidResult(op)
	}
	return d, nil
}
func validInboundReceipt(d map[string]any) bool {
	if !inboundKeys(d, "id inbox_id period_id state reason created_at expires_at message_id provider_created_at recovery_attempt_deadline") || !validInboundState(d, true) || !validInboundDeadline(d) {
		return false
	}
	id, _ := d["id"].(string)
	if !OpaqueInboundID(id) {
		return false
	}
	for _, k := range []string{"inbox_id", "period_id"} {
		s, ok := d[k].(string)
		if !ok || (s != "" && !inboundUUID(s)) {
			return false
		}
	}
	a, ok := inboundTime(d["created_at"])
	b, ok2 := inboundTime(d["expires_at"])
	return ok && ok2 && b.After(a)
}
func validInboundState(d map[string]any, list bool) bool {
	switch d["state"] {
	case "pending", "materialized", "content_paused", "expired_unrecoverable":
	default:
		return false
	}
	if reason, ok := d["reason"]; ok {
		switch reason {
		case "inbound_hard_cap", "storage_exhausted", "period_closed", "provider_unavailable", "provider_retention_elapsed":
		case "inbox_daily_cap":
			if !list {
				return false
			}
		default:
			return false
		}
	}
	if d["state"] == "materialized" {
		s, _ := d["message_id"].(string)
		_, reason := d["reason"]
		return inboundUUID(s) && !reason
	}
	if _, ok := d["message_id"]; ok {
		return false
	}
	if !list && d["state"] == "expired_unrecoverable" {
		return d["reason"] == "provider_unavailable" || d["reason"] == "provider_retention_elapsed"
	}
	return true
}
func validInboundDeadline(d map[string]any) bool {
	a, aok := d["provider_created_at"]
	b, bok := d["recovery_attempt_deadline"]
	if !aok && !bok {
		return true
	}
	ta, ok := inboundTime(a)
	tb, ok2 := inboundTime(b)
	return aok && bok && ok && ok2 && tb.Sub(ta) == 30*24*time.Hour
}
func inboundTime(v any) (time.Time, bool) {
	s, ok := v.(string)
	if !ok || len(s) > 64 {
		return time.Time{}, false
	}
	t, e := time.Parse(time.RFC3339Nano, s)
	return t, e == nil
}
func inboundUUID(s string) bool {
	u, e := uuid.Parse(s)
	return e == nil && u != uuid.Nil && u.String() == s
}
func inboundInteger(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, e := strconv.ParseInt(string(n), 10, 64)
	return i, e == nil && i >= 0
}
func inboundKeys(d map[string]any, allowed string) bool {
	set := map[string]bool{}
	for _, k := range strings.Fields(allowed) {
		set[k] = true
	}
	for k, v := range d {
		if !set[k] || v == nil {
			return false
		}
	}
	return true
}
func inboundObject(raw []byte) (map[string]any, error) {
	if len(raw) > 65536 {
		return nil, errors.New("inbound response exceeds bound")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var walk func(int) (any, error)
	walk = func(depth int) (any, error) {
		if depth > 16 {
			return nil, errors.New("inbound nesting exceeds bound")
		}
		t, e := dec.Token()
		if e != nil {
			return nil, e
		}
		switch t {
		case json.Delim('{'):
			d := map[string]any{}
			seen := map[string]bool{}
			for dec.More() {
				k, e := dec.Token()
				if e != nil {
					return nil, e
				}
				s, ok := k.(string)
				if !ok || seen[strings.ToLower(s)] {
					return nil, errors.New("duplicate inbound field")
				}
				seen[strings.ToLower(s)] = true
				v, e := walk(depth + 1)
				if e != nil {
					return nil, e
				}
				d[s] = v
			}
			_, e = dec.Token()
			return d, e
		case json.Delim('['):
			a := []any{}
			for dec.More() {
				v, e := walk(depth + 1)
				if e != nil {
					return nil, e
				}
				a = append(a, v)
			}
			_, e = dec.Token()
			return a, e
		default:
			return t, nil
		}
	}
	v, e := walk(0)
	if e != nil {
		return nil, e
	}
	if _, e = dec.Token(); !errors.Is(e, io.EOF) {
		return nil, errors.New("trailing inbound JSON")
	}
	d, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("inbound object required")
	}
	return d, nil
}

// DecodeInboundInput pins the native caller input before signed delegation.
func DecodeInboundInput(op string, raw json.RawMessage) (InboundInput, error) {
	doc, e := inboundObject(raw)
	if e != nil {
		return InboundInput{}, &InboundBusinessError{Code: "inbound_invalid_request"}
	}
	i := InboundInput{}
	if op == "receipts" {
		i.Limit = 50
	}
	for k, v := range doc {
		switch {
		case op == "receipts" && k == "limit":
			n, ok := inboundInteger(v)
			if !ok || n > 100 {
				return InboundInput{}, &InboundBusinessError{Code: "inbound_invalid_request"}
			}
			i.Limit = int(n)
		case op == "receipts" && k == "cursor":
			i.Cursor, _ = v.(string)
			if i.Cursor == "" {
				return InboundInput{}, &InboundBusinessError{Code: "inbound_invalid_request"}
			}
		case op == "recover" && k == "receipt_id":
			i.ReceiptID, _ = v.(string)
		default:
			return InboundInput{}, &InboundBusinessError{Code: "inbound_invalid_request"}
		}
	}
	if !ValidInboundInput(op, i) {
		return InboundInput{}, &InboundBusinessError{Code: "inbound_invalid_request"}
	}
	return i, nil
}

func inboundErrorCodes() []string {
	return []string{"inbound_invalid_request", "inbound_unavailable", "inbound_retry_later", "inbound_outcome_unknown", "recovery_in_progress_or_changed", "recovery_provider_or_save_unavailable", "recovery_response_exceeds_limit"}
}
