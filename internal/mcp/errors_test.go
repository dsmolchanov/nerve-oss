package mcp

import (
	"strings"
	"testing"
	"time"

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

func TestTranslateModernBusinessErrorMapsOnboardingFailures(t *testing.T) {
	retryAt := time.Unix(1_723_000_100, 0).UTC()
	translated := translateModernBusinessError(&OnboardingBusinessError{
		Code: OnboardingErrorInProgress, Retryable: true, RetryAt: &retryAt,
	})
	if translated.Code != OnboardingErrorInProgress || !translated.Retryable || translated.RetryAt != retryAt.Format(time.RFC3339) {
		t.Fatalf("translated business error=%+v", translated)
	}

	translated = translateModernBusinessError(ErrOnboardingOutcomeUnknown)
	if translated.Code != OnboardingErrorOutcomeUnknown || !translated.Retryable || translated.RetryAt != "" {
		t.Fatalf("translated outcome-unknown error=%+v", translated)
	}

	translated = translateModernBusinessError(&OnboardingBusinessError{Code: "sk_live_SYNTHETIC_PROVIDER_SECRET"})
	if translated.Code != "tool_failed" || strings.Contains(translated.Code, "SYNTHETIC_PROVIDER_SECRET") {
		t.Fatalf("unknown onboarding error translation=%+v", translated)
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
