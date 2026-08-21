package mcp

import (
	"context"
	"errors"
	"time"

	"neuralmail/internal/auth"
)

type OnboardingMailboxMode string

const (
	OnboardingMailboxManaged      OnboardingMailboxMode = "managed_mailbox"
	OnboardingMailboxCustomDomain OnboardingMailboxMode = "custom_domain"
)

// OnboardingCaller is derived exclusively from the authenticated MCP request.
// None of these fields are accepted from lifecycle tool arguments.
type OnboardingCaller struct {
	Principal     auth.Principal
	Authorization string
}

type OnboardingStartInput struct {
	IdempotencyKey   string                `json:"idempotency_key"`
	OrganizationName string                `json:"organization_name"`
	MailboxMode      OnboardingMailboxMode `json:"mailbox_mode"`
	CustomDomain     string                `json:"custom_domain,omitempty"`
	LocalPart        string                `json:"local_part,omitempty"`
}

type OnboardingCloseInput struct {
	IdempotencyKey     string `json:"idempotency_key"`
	ExpectedGeneration int64  `json:"expected_generation"`
}

type OnboardingDNSRecord struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	Value    string `json:"value"`
	Priority *int   `json:"priority,omitempty"`
}

type OnboardingDNSCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

type OnboardingResult struct {
	ResultType   string                `json:"resultType"`
	OnboardingID string                `json:"onboarding_id"`
	Generation   int64                 `json:"generation"`
	State        string                `json:"state"`
	Mode         OnboardingMailboxMode `json:"mode"`
	Address      string                `json:"address,omitempty"`
	DNSRecords   []OnboardingDNSRecord `json:"dns_records,omitempty"`
	DNSChecks    []OnboardingDNSCheck  `json:"dns_checks,omitempty"`
	NextAction   string                `json:"next_action,omitempty"`
	RetryAt      *time.Time            `json:"retry_at,omitempty"`
	Reauthorize  bool                  `json:"reauthorize"`
}

type OnboardingProvisioner interface {
	Start(context.Context, OnboardingCaller, OnboardingStartInput) (OnboardingResult, error)
	Status(context.Context, OnboardingCaller) (OnboardingResult, error)
	VerifyDomain(context.Context, OnboardingCaller) (OnboardingResult, error)
	Close(context.Context, OnboardingCaller, OnboardingCloseInput) (OnboardingResult, error)
}

type OnboardingBusinessError struct {
	Code      string     `json:"code"`
	Retryable bool       `json:"retryable"`
	RetryAt   *time.Time `json:"retry_at,omitempty"`
}

const (
	OnboardingErrorInvalidRequest         = "onboarding_invalid_request"
	OnboardingErrorIdempotencyConflict    = "onboarding_idempotency_conflict"
	OnboardingErrorGenerationConflict     = "onboarding_generation_conflict"
	OnboardingErrorMailboxModeNotAllowed  = "onboarding_mailbox_mode_not_allowed"
	OnboardingErrorNotFound               = "onboarding_not_found"
	OnboardingErrorInvalidState           = "onboarding_invalid_state"
	OnboardingErrorDomainConflict         = "onboarding_domain_conflict"
	OnboardingErrorDomainNotReady         = "onboarding_domain_not_ready"
	OnboardingErrorBillingRequired        = "onboarding_billing_required"
	OnboardingErrorPaymentActionRequired  = "onboarding_payment_action_required"
	OnboardingErrorRateLimited            = "onboarding_rate_limited"
	OnboardingErrorTemporarilyUnavailable = "onboarding_temporarily_unavailable"
)

var publicOnboardingBusinessErrorCodes = map[string]struct{}{
	OnboardingErrorInvalidRequest: {}, OnboardingErrorIdempotencyConflict: {},
	OnboardingErrorGenerationConflict: {}, OnboardingErrorMailboxModeNotAllowed: {},
	OnboardingErrorNotFound: {}, OnboardingErrorInvalidState: {},
	OnboardingErrorDomainConflict: {}, OnboardingErrorDomainNotReady: {},
	OnboardingErrorBillingRequired: {}, OnboardingErrorPaymentActionRequired: {},
	OnboardingErrorRateLimited: {}, OnboardingErrorTemporarilyUnavailable: {},
}

// IsPublicOnboardingBusinessErrorCode is the closed wire contract shared by
// the runtime adapter and the control-plane delegation boundary. Unknown
// upstream values are protocol failures and must never be reflected to agents.
func IsPublicOnboardingBusinessErrorCode(code string) bool {
	_, ok := publicOnboardingBusinessErrorCodes[code]
	return ok
}

func (err *OnboardingBusinessError) Error() string {
	if err == nil || err.Code == "" {
		return "onboarding request failed"
	}
	return err.Code
}

var ErrOnboardingOutcomeUnknown = errors.New("onboarding outcome unknown; poll status for the same generation")
