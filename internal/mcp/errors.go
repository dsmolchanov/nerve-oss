package mcp

import (
	"errors"
	"time"

	"neuralmail/internal/entitlements"
	"neuralmail/internal/tools"
)

type modernBusinessError struct {
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
	RetryAt   string `json:"retry_at,omitempty"`
}

func translateModernBusinessError(err error) modernBusinessError {
	translated := modernBusinessError{Code: "tool_failed"}
	var attachmentErr *tools.AttachmentInputError
	var policyErr *outboundPolicyError
	// The enqueue-time recheck raises the tools-layer type; without this the
	// caller would see a generic tool_failed instead of the actual denial.
	var enqueuePolicyErr *tools.OutboundPolicyError
	var rateErr *entitlements.RateLimitError
	var inProgressErr *entitlements.IdempotencyInProgressError
	var onboardingErr *OnboardingBusinessError
	switch {
	case errors.As(err, &onboardingErr):
		if !IsPublicOnboardingBusinessErrorCode(onboardingErr.Code) {
			return translated
		}
		translated.Code = onboardingErr.Code
		translated.Retryable = onboardingErr.Retryable
		if onboardingErr.RetryAt != nil {
			translated.RetryAt = onboardingErr.RetryAt.UTC().Format(time.RFC3339)
		}
	case errors.Is(err, ErrOnboardingOutcomeUnknown):
		translated.Code = OnboardingErrorOutcomeUnknown
		translated.Retryable = true
	case errors.As(err, &policyErr):
		translated.Code = policyErr.Code
	case errors.As(err, &enqueuePolicyErr):
		translated.Code = enqueuePolicyErr.Code
	case errors.As(err, &attachmentErr):
		translated.Code = attachmentErr.Code
	case errors.Is(err, entitlements.ErrQuotaExceeded):
		translated.Code = "quota_exceeded"
	case errors.Is(err, entitlements.ErrSubscriptionInactive):
		translated.Code = "subscription_inactive"
	case errors.As(err, &rateErr):
		translated.Code = "rate_limited"
		translated.Retryable = true
		translated.RetryAt = time.Now().UTC().Add(time.Duration(rateErr.RetryAfterSeconds) * time.Second).Format(time.RFC3339)
	case errors.As(err, &inProgressErr):
		translated.Code = "idempotency_in_progress"
		translated.Retryable = true
		translated.RetryAt = time.Now().UTC().Add(time.Duration(inProgressErr.RetryAfterSeconds) * time.Second).Format(time.RFC3339)
	}
	return translated
}
