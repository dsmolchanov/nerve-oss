package tools

import (
	"context"
	"encoding/json"
	"testing"

	"neuralmail/internal/config"
	"neuralmail/internal/entitlements"
)

func TestSendReplyBlocksNeedsApprovalInCloudWithoutOverrideFeature(t *testing.T) {
	cfg := config.Default()
	cfg.Cloud.Mode = true

	svc := &Service{Config: cfg}
	ctx := entitlements.WithReservation(context.Background(), entitlements.Reservation{
		Features: json.RawMessage(`{}`),
	})

	_, err := svc.SendReply(ctx, "", "", "", true, "idemp-1")
	if err == nil || err.Error() != "send blocked: needs human approval" {
		t.Fatalf("expected approval block, got %v", err)
	}
}

func TestSendReplyAllowsNeedsApprovalInCloudWithOverrideFeature(t *testing.T) {
	cfg := config.Default()
	cfg.Cloud.Mode = true

	svc := &Service{Config: cfg}
	ctx := entitlements.WithReservation(context.Background(), entitlements.Reservation{
		Features: json.RawMessage(`{"email_autopilot_send_override": true}`),
	})

	_, err := svc.SendReply(ctx, "", "", "", true, "idemp-2")
	if err == nil || err.Error() != "missing cloud principal" {
		t.Fatalf("expected to pass approval gate and then fail for missing principal, got %v", err)
	}
}
