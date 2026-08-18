package mcp

import (
	"testing"

	"neuralmail/internal/tools"
)

func TestTranslateModernBusinessErrorMapsEnqueuePolicyDenial(t *testing.T) {
	// The enqueue-time recheck raises the tools-layer type; a caller must see
	// the actual denial rather than a generic tool_failed.
	translated := translateModernBusinessError(&tools.OutboundPolicyError{
		Code: "email_outbound_suspended_denied",
	})

	if translated.Code != "email_outbound_suspended_denied" {
		t.Fatalf("expected the denial code, got %q", translated.Code)
	}
}

func TestTranslateModernBusinessErrorMapsBoundaryPolicyDenial(t *testing.T) {
	translated := translateModernBusinessError(&outboundPolicyError{
		Code: "inbound_reply_policy_denied",
	})

	if translated.Code != "inbound_reply_policy_denied" {
		t.Fatalf("expected the denial code, got %q", translated.Code)
	}
}
