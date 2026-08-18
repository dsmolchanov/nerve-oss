package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"neuralmail/internal/config"
	"neuralmail/internal/entitlements"
	"neuralmail/internal/policy"
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

func TestSendReplyDerivesApprovalFromServerPolicyWhenCallerSaysFalse(t *testing.T) {
	cfg := config.Default()
	cfg.Cloud.Mode = true
	svc := &Service{Config: cfg, Policy: policy.Policy{
		ForbiddenPhrases: []string{"wire the deposit"},
	}}
	ctx := entitlements.WithReservation(context.Background(), entitlements.Reservation{
		Features: json.RawMessage(`{"email_autopilot_send_override": true}`),
	})

	// The caller claims no approval is needed; server policy disagrees, and the
	// caller's claim must not decide it.
	_, err := svc.SendReply(ctx, "thread-1", "please wire the deposit today", "", false, "idemp-policy-1")

	if err == nil || !strings.Contains(err.Error(), "send blocked by policy") {
		t.Fatalf("expected the server policy to block the send, got %v", err)
	}
}

func TestComposeEmailEvaluatesServerPolicy(t *testing.T) {
	cfg := config.Default()
	cfg.Cloud.Mode = true
	svc := &Service{Config: cfg, Policy: policy.Policy{
		ForbiddenPhrases: []string{"wire the deposit"},
	}}
	ctx := entitlements.WithReservation(context.Background(), entitlements.Reservation{
		Features: json.RawMessage(`{"email_autopilot_send_override": true}`),
	})

	// compose has no needs_human_approval field, so policy is the only check.
	_, err := svc.ComposeEmailWithOptions(ctx, "inbox-1", "someone@example.test",
		"Subject", "please wire the deposit today", "", "idemp-policy-2", ComposeEmailOptions{})

	if err == nil || !strings.Contains(err.Error(), "send blocked by policy") {
		t.Fatalf("expected the server policy to block the compose, got %v", err)
	}
}

func TestSendReplyStillAllowsCleanContent(t *testing.T) {
	cfg := config.Default()
	cfg.Cloud.Mode = false
	svc := &Service{Config: cfg, Policy: policy.Policy{ForbiddenPhrases: []string{"forbidden"}}}

	// Clean content must pass the gate; it fails later for want of a store,
	// which is exactly how far this unit can reach.
	err := svc.approvalGate(context.Background(), "thanks, see you tomorrow", "", false)

	if err != nil {
		t.Fatalf("clean content must pass the approval gate, got %v", err)
	}
}

func TestApprovalGateEvaluatesTheHTMLRepresentation(t *testing.T) {
	cfg := config.Default()
	svc := &Service{Config: cfg, Policy: policy.Policy{ForbiddenPhrases: []string{"wire the deposit"}}}

	// An HTML-only message used to be judged on an empty plain-text body.
	err := svc.approvalGate(context.Background(), "", "<p>please wire the deposit</p>", false)

	if err == nil || !strings.Contains(err.Error(), "send blocked by policy") {
		t.Fatalf("expected the HTML body to be evaluated, got %v", err)
	}
}

func TestApprovalGateRefusesContentPolicyWouldRewrite(t *testing.T) {
	cfg := config.Default()
	redacting := policy.Policy{}
	redacting.Redactions.Patterns = []string{`\d{16}`}
	redacting.Redactions.Replacement = "[redacted]"
	svc := &Service{Config: cfg, Policy: redacting}

	// A redaction means the evaluated text is not the text this path enqueues,
	// so the send is refused rather than delivered unredacted.
	err := svc.approvalGate(context.Background(), "card 4111111111111111 attached", "", false)

	if err == nil || !strings.Contains(err.Error(), "requires redaction") {
		t.Fatalf("expected a redaction refusal, got %v", err)
	}
}

func TestApprovalGateSeesTextHiddenByEntitiesAndTags(t *testing.T) {
	cfg := config.Default()
	svc := &Service{Config: cfg, Policy: policy.Policy{ForbiddenPhrases: []string{"guarantee"}}}

	for name, markup := range map[string]string{
		"entity encoded": "<p>we guar&#97;ntee it</p>",
		"split by a tag": "<p>we guar<b>a</b>ntee it</p>",
		"plain markup":   "<p>we guarantee it</p>",
	} {
		t.Run(name, func(t *testing.T) {
			err := svc.approvalGate(context.Background(), "", markup, false)
			if err == nil || !strings.Contains(err.Error(), "send blocked by policy") {
				t.Fatalf("expected the rendered text to be evaluated, got %v", err)
			}
		})
	}
}

func TestApprovalGateKeepsWordBoundariesAcrossBlocks(t *testing.T) {
	cfg := config.Default()
	svc := &Service{Config: cfg, Policy: policy.Policy{ForbiddenPhrases: []string{"nowarranty"}}}

	// Two separate words must not fuse into a forbidden phrase, which is why
	// the spaced rendering exists alongside the joined one.
	if err := svc.approvalGate(context.Background(), "", "<p>no</p><p>warranty</p>", false); err != nil {
		t.Fatalf("adjacent blocks must not fabricate a violation, got %v", err)
	}
}

func TestApprovalGateIgnoresScriptAndStyleContent(t *testing.T) {
	cfg := config.Default()
	svc := &Service{Config: cfg, Policy: policy.Policy{ForbiddenPhrases: []string{"guarantee"}}}

	if err := svc.approvalGate(context.Background(), "",
		"<style>.guarantee{color:red}</style><p>hello</p>", false); err != nil {
		t.Fatalf("invisible content must not trip policy, got %v", err)
	}
}

func TestApprovalGateStillEvaluatesTheHTMLBodyWhenPlainIsEmpty(t *testing.T) {
	cfg := config.Default()
	svc := &Service{Config: cfg, Policy: policy.Policy{ForbiddenPhrases: []string{"wire the deposit"}}}

	err := svc.approvalGate(context.Background(), "", "<div><p>please wire the deposit</p></div>", false)

	if err == nil || !strings.Contains(err.Error(), "send blocked by policy") {
		t.Fatalf("an HTML-only send must still be evaluated, got %v", err)
	}
}
