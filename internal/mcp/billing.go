package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"neuralmail/internal/auth"
)

const billingSubscribeToolName = "nerve_billing_subscribe"

// BillingCaller contains authority derived exclusively from the authenticated
// MCP request. Billing tool arguments never select a client, organization,
// onboarding generation, customer, payment method, or mandate.
type BillingCaller struct {
	Principal     auth.Principal
	Authorization string
}

type BillingSubscribeInput struct {
	PlanCode       string `json:"plan_code"`
	IdempotencyKey string `json:"idempotency_key"`
}

type BillingSubscribeState string

const (
	BillingSubscribeProcessing      BillingSubscribeState = "processing"
	BillingSubscribeProviderUnknown BillingSubscribeState = "provider_unknown"
	BillingSubscribeRequiresAction  BillingSubscribeState = "requires_action"
	BillingSubscribeActive          BillingSubscribeState = "active"
)

// BillingSubscribeResult intentionally excludes provider, customer, payment,
// mandate, client, organization, and generation identifiers. The control plane
// retains those references as durable evidence and resolves them from Caller.
type BillingSubscribeResult struct {
	ResultType     string                `json:"resultType"`
	State          BillingSubscribeState `json:"state"`
	PlanCode       string                `json:"plan_code"`
	ComposeEnabled bool                  `json:"compose_enabled"`
	RetryAt        *time.Time            `json:"retry_at,omitempty"`
}

// BillingProvisioner is the only runtime dependency required by the modern
// billing tool. Implementations must resolve all authority and payment data
// server-side from BillingCaller.
type BillingProvisioner interface {
	Subscribe(context.Context, BillingCaller, BillingSubscribeInput) (BillingSubscribeResult, error)
}

type BillingBusinessError struct {
	Code      string     `json:"code"`
	Retryable bool       `json:"retryable"`
	RetryAt   *time.Time `json:"retry_at,omitempty"`
}

const (
	BillingErrorInvalidRequest         = "billing_invalid_request"
	BillingErrorIdempotencyConflict    = "billing_idempotency_conflict"
	BillingErrorMandateUnavailable     = "billing_mandate_unavailable"
	BillingErrorPlanNotAllowed         = "billing_plan_not_allowed"
	BillingErrorInvalidState           = "billing_invalid_state"
	BillingErrorRateLimited            = "billing_rate_limited"
	BillingErrorTemporarilyUnavailable = "billing_temporarily_unavailable"
)

var publicBillingBusinessErrorCodes = func() map[string]struct{} {
	codes := make(map[string]struct{})
	for _, code := range billingBusinessErrorCodes() {
		codes[code] = struct{}{}
	}
	return codes
}()

func billingBusinessErrorCodes() []string {
	return []string{
		BillingErrorInvalidRequest,
		BillingErrorIdempotencyConflict,
		BillingErrorMandateUnavailable,
		BillingErrorPlanNotAllowed,
		BillingErrorInvalidState,
		BillingErrorRateLimited,
		BillingErrorTemporarilyUnavailable,
	}
}

func IsPublicBillingBusinessErrorCode(code string) bool {
	_, ok := publicBillingBusinessErrorCodes[code]
	return ok
}

func (err *BillingBusinessError) Error() string {
	if err == nil || !IsPublicBillingBusinessErrorCode(err.Code) {
		return "billing request failed"
	}
	return err.Code
}

func billingToolDescriptor() toolDescriptor {
	return toolDescriptor{
		Name:        billingSubscribeToolName,
		Description: "Create or replay the caller's generation-bound paid subscription",
		InputSchema: inputObject(map[string]any{
			"plan_code": map[string]any{
				"type": "string", "minLength": 1, "maxLength": 64,
				"pattern": `^[a-z0-9][a-z0-9._-]{0,63}$`,
			},
			"idempotency_key": boundedStringProperty(1, 128),
		}, "plan_code", "idempotency_key"),
		OutputShape: outputObject(map[string]any{
			"resultType": map[string]any{"type": "string", "const": "complete"},
			"state": map[string]any{"type": "string", "enum": []string{
				string(BillingSubscribeProcessing), string(BillingSubscribeProviderUnknown),
				string(BillingSubscribeRequiresAction), string(BillingSubscribeActive),
			}},
			"plan_code":       boundedStringProperty(1, 64),
			"compose_enabled": map[string]any{"type": "boolean"},
			"retry_at":        map[string]any{"type": "string", "format": "date-time"},
		}, "resultType", "state", "plan_code", "compose_enabled"),
		ErrorCodes: billingBusinessErrorCodes(),
	}
}

func billingToolAvailable(server *Server, principal auth.Principal) bool {
	if server == nil || server.Billing == nil || !server.Config.Cloud.Mode || server.Auth == nil {
		return false
	}
	if !isActiveBillingPrincipalForTool(principal) {
		return false
	}
	return server.Auth.ValidateScopes(principal, "nerve:billing.subscribe") == nil
}

func isActiveBillingPrincipalForTool(principal auth.Principal) bool {
	if principal.Kind != auth.PrincipalM2MOrg || principal.AuthMethod != "m2m_bearer" ||
		principal.ClientID == "" || principal.OrgID == "" || principal.Generation < 1 {
		return false
	}
	for _, scope := range principal.Scopes {
		if scope == "nerve:billing.subscribe" {
			return true
		}
	}
	return false
}

func invokeBillingTool(
	ctx context.Context,
	provisioner BillingProvisioner,
	caller BillingCaller,
	name string,
	arguments json.RawMessage,
) (BillingSubscribeResult, error) {
	var result BillingSubscribeResult
	if name != billingSubscribeToolName {
		return result, billingInvalidRequest()
	}
	if provisioner == nil {
		return result, billingTemporarilyUnavailable()
	}
	if !isActiveBillingPrincipalForTool(caller.Principal) {
		return result, billingInvalidState()
	}
	var input BillingSubscribeInput
	if err := decodeBillingArguments(arguments, &input); err != nil {
		return result, billingInvalidRequest()
	}
	if err := validateBillingSubscribeInput(input); err != nil {
		return result, billingInvalidRequest()
	}
	result, err := provisioner.Subscribe(ctx, caller, input)
	if err != nil {
		return BillingSubscribeResult{}, sanitizeBillingProvisionerError(err)
	}
	if err := validateBillingSubscribeResult(result, input.PlanCode); err != nil {
		return BillingSubscribeResult{}, billingTemporarilyUnavailable()
	}
	return result, nil
}

func billingInvalidRequest() error {
	return &BillingBusinessError{Code: BillingErrorInvalidRequest, Retryable: false}
}

func billingInvalidState() error {
	return &BillingBusinessError{Code: BillingErrorInvalidState, Retryable: false}
}

func billingTemporarilyUnavailable() error {
	return &BillingBusinessError{Code: BillingErrorTemporarilyUnavailable, Retryable: true}
}

func sanitizeBillingProvisionerError(err error) error {
	var businessError *BillingBusinessError
	if errors.As(err, &businessError) && validBillingBusinessError(businessError) {
		return businessError
	}
	return billingTemporarilyUnavailable()
}

func validBillingBusinessError(businessError *BillingBusinessError) bool {
	if businessError == nil || !IsPublicBillingBusinessErrorCode(businessError.Code) {
		return false
	}
	if !validBillingRetryAt(businessError.RetryAt) {
		return false
	}
	switch businessError.Code {
	case BillingErrorInvalidRequest, BillingErrorIdempotencyConflict, BillingErrorMandateUnavailable,
		BillingErrorPlanNotAllowed, BillingErrorInvalidState:
		return !businessError.Retryable && businessError.RetryAt == nil
	case BillingErrorRateLimited:
		return businessError.Retryable && businessError.RetryAt != nil
	case BillingErrorTemporarilyUnavailable:
		return businessError.Retryable
	default:
		return false
	}
}

func validateBillingSubscribeInput(input BillingSubscribeInput) error {
	if !validBillingPlanCode(input.PlanCode) {
		return errors.New("plan_code is invalid")
	}
	return validateBillingIdempotencyKey(input.IdempotencyKey)
}

func validBillingPlanCode(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for index, character := range []byte(value) {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') {
			continue
		}
		if index > 0 && (character == '.' || character == '_' || character == '-') {
			continue
		}
		return false
	}
	return true
}

func validateBillingIdempotencyKey(value string) error {
	if value == "" || !utf8.ValidString(value) || value != strings.TrimSpace(value) || len(value) > 128 {
		return errors.New("idempotency_key is invalid")
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return errors.New("idempotency_key is invalid")
		}
	}
	return nil
}

func validateBillingSubscribeResult(result BillingSubscribeResult, expectedPlanCode string) error {
	if result.ResultType != "complete" || result.PlanCode != expectedPlanCode || !validBillingPlanCode(result.PlanCode) {
		return errors.New("billing provisioner returned an invalid result")
	}
	switch result.State {
	case BillingSubscribeActive:
		if !result.ComposeEnabled || result.RetryAt != nil {
			return errors.New("active billing result is inconsistent")
		}
	case BillingSubscribeProcessing, BillingSubscribeProviderUnknown:
		if result.ComposeEnabled {
			return errors.New("pending billing result enabled compose")
		}
	case BillingSubscribeRequiresAction:
		if result.ComposeEnabled || result.RetryAt != nil {
			return errors.New("requires_action billing result is inconsistent")
		}
	default:
		return errors.New("billing provisioner returned an invalid state")
	}
	if !validBillingRetryAt(result.RetryAt) {
		return errors.New("billing provisioner returned an invalid retry time")
	}
	return nil
}

func validBillingRetryAt(retryAt *time.Time) bool {
	if retryAt == nil {
		return true
	}
	if retryAt.IsZero() {
		return false
	}
	// time.Time.MarshalJSON is the same boundary used by the SDK response. It
	// rejects years and zone offsets that cannot be represented as RFC 3339,
	// preventing a provisioner value from escaping the advertised closed shape
	// as a generic JSON encoding failure.
	_, err := retryAt.MarshalJSON()
	return err == nil
}

func decodeBillingArguments(arguments json.RawMessage, target *BillingSubscribeInput) error {
	if len(bytes.TrimSpace(arguments)) == 0 {
		return errors.New("billing arguments are required")
	}
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return errors.New("billing arguments must be one JSON object")
	}
	seen := make(map[string]struct{}, 2)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("invalid billing arguments: %w", err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return errors.New("billing argument field is invalid")
		}
		if _, duplicate := seen[key]; duplicate {
			return errors.New("duplicate billing argument field")
		}
		seen[key] = struct{}{}
		switch key {
		case "plan_code":
			if err := decoder.Decode(&target.PlanCode); err != nil {
				return errors.New("plan_code must be a string")
			}
		case "idempotency_key":
			if err := decoder.Decode(&target.IdempotencyKey); err != nil {
				return errors.New("idempotency_key must be a string")
			}
		default:
			// This includes every authority- or payment-bearing field. Do not
			// decode its value or reflect the rejected name to the caller.
			return errors.New("unknown billing argument field")
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return errors.New("unterminated billing arguments")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("billing arguments contain multiple JSON values")
	}
	return nil
}

type billingAuthorizationContextKey struct{}

func withBillingAuthorization(ctx context.Context, authorization string) context.Context {
	return context.WithValue(ctx, billingAuthorizationContextKey{}, authorization)
}

func billingAuthorizationFromContext(ctx context.Context) string {
	authorization, _ := ctx.Value(billingAuthorizationContextKey{}).(string)
	return authorization
}
