package mcp

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"neuralmail/internal/store"
	"neuralmail/internal/tools"
)

func TestTranslateModernBusinessErrorSeparatesRecipientQuotaAndPeriodClosure(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code string
	}{
		{"recipient allowance", store.ErrRecipientAllowanceExhausted, "recipient_quota_exceeded"},
		{"recipient period", store.ErrRecipientPeriodUnavailable, "recipient_period_unavailable"},
		{"generic recipient denial", store.ErrRecipientLimit, "recipient_admission_unavailable"},
		{"storage", store.ErrAttachmentQuotaExceeded, "storage_quota_exceeded"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			translated := translateModernBusinessError(fmt.Errorf("enqueue: %w", test.err))
			if translated.Code != test.code || translated.Retryable || translated.Remediation != "" {
				t.Fatalf("translated error=%+v want code=%q", translated, test.code)
			}
		})
	}
}

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

func TestTranslateModernBusinessErrorRedactsCloudOutboundConfiguration(t *testing.T) {
	for _, code := range []string{"outbound_disabled", "recipient_domain_not_allowed", "outbound_transport_unconfigured"} {
		t.Run(code, func(t *testing.T) {
			translated := translateModernBusinessError(&tools.CloudOutboundConfigurationError{Code: code})
			if translated.Code != code || translated.Remediation != "" || translated.Retryable {
				t.Fatalf("unsafe or missing cloud configuration result: %+v", translated)
			}
		})
	}
	unsafe := translateModernBusinessError(&tools.CloudOutboundConfigurationError{Code: "SYNTHETIC_PROVIDER_SECRET"})
	if unsafe.Code != "tool_failed" || strings.Contains(unsafe.Code, "SYNTHETIC_PROVIDER_SECRET") {
		t.Fatalf("unrecognized cloud configuration code escaped: %+v", unsafe)
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
