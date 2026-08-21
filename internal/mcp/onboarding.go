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

	"github.com/google/uuid"

	"neuralmail/internal/auth"
	"neuralmail/internal/domains"
	"neuralmail/internal/emailaddr"
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
	OnboardingErrorInProgress             = "onboarding_in_progress"
	OnboardingErrorOutcomeUnknown         = "onboarding_outcome_unknown"
)

var publicOnboardingBusinessErrorCodes = func() map[string]struct{} {
	codes := make(map[string]struct{})
	for _, code := range onboardingBusinessErrorCodes() {
		codes[code] = struct{}{}
	}
	return codes
}()

func onboardingBusinessErrorCodes() []string {
	return []string{
		OnboardingErrorInvalidRequest, OnboardingErrorIdempotencyConflict,
		OnboardingErrorGenerationConflict, OnboardingErrorMailboxModeNotAllowed,
		OnboardingErrorNotFound, OnboardingErrorInvalidState,
		OnboardingErrorDomainConflict, OnboardingErrorDomainNotReady,
		OnboardingErrorBillingRequired, OnboardingErrorPaymentActionRequired,
		OnboardingErrorRateLimited, OnboardingErrorTemporarilyUnavailable,
		OnboardingErrorInProgress, OnboardingErrorOutcomeUnknown,
	}
}

// IsPublicOnboardingBusinessErrorCode is the closed wire contract shared by
// the runtime adapter and the control-plane delegation boundary. Unknown
// upstream values are protocol failures and must never be reflected to agents.
func IsPublicOnboardingBusinessErrorCode(code string) bool {
	_, ok := publicOnboardingBusinessErrorCodes[code]
	return ok
}

func (err *OnboardingBusinessError) Error() string {
	if err == nil || !IsPublicOnboardingBusinessErrorCode(err.Code) {
		return "onboarding request failed"
	}
	return err.Code
}

var ErrOnboardingOutcomeUnknown = errors.New("onboarding outcome unknown; poll status for the same generation")

type onboardingAuthorizationContextKey struct{}

func withOnboardingAuthorization(ctx context.Context, authorization string) context.Context {
	return context.WithValue(ctx, onboardingAuthorizationContextKey{}, authorization)
}

func onboardingAuthorizationFromContext(ctx context.Context) string {
	authorization, _ := ctx.Value(onboardingAuthorizationContextKey{}).(string)
	return authorization
}

func onboardingToolDescriptors() []toolDescriptor {
	result := outputObject(map[string]any{
		"resultType":    map[string]any{"type": "string", "const": "complete"},
		"onboarding_id": uuidStringProperty(),
		"generation":    map[string]any{"type": "integer", "minimum": 1},
		"state": map[string]any{"type": "string", "enum": []string{
			"provisioning", "dns_pending", "active", "deprovisioning", "closed",
		}},
		"mode":    map[string]any{"type": "string", "enum": []string{string(OnboardingMailboxManaged), string(OnboardingMailboxCustomDomain)}},
		"address": boundedStringProperty(0, 320),
		"dns_records": map[string]any{"type": "array", "items": map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				"type": stringProperty(1), "name": stringProperty(1), "value": stringProperty(1),
				"priority": map[string]any{"type": "integer", "minimum": 0, "maximum": 65535},
			}, "required": []string{"type", "name", "value"},
		}},
		"dns_checks": map[string]any{"type": "array", "items": map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{"name": stringProperty(1), "status": stringProperty(1)},
			"required":   []string{"name", "status"},
		}},
		"next_action": stringProperty(0),
		"retry_at":    map[string]any{"type": "string", "format": "date-time"},
		"reauthorize": map[string]any{"type": "boolean"},
	}, "resultType", "onboarding_id", "generation", "state", "mode", "reauthorize")
	managedStart := inputObject(map[string]any{
		"idempotency_key":   boundedStringProperty(1, 128),
		"organization_name": boundedStringProperty(1, 160),
		"mailbox_mode":      map[string]any{"type": "string", "const": string(OnboardingMailboxManaged)},
	}, "idempotency_key", "organization_name", "mailbox_mode")
	customStart := inputObject(map[string]any{
		"idempotency_key":   boundedStringProperty(1, 128),
		"organization_name": boundedStringProperty(1, 160),
		"mailbox_mode":      map[string]any{"type": "string", "const": string(OnboardingMailboxCustomDomain)},
		"custom_domain":     map[string]any{"type": "string", "minLength": 1, "maxLength": 253, "pattern": `^[A-Za-z0-9.-]+$`},
		"local_part":        map[string]any{"type": "string", "minLength": 1, "maxLength": 64, "pattern": `^[A-Za-z0-9][A-Za-z0-9._+-]*$`},
	}, "idempotency_key", "organization_name", "mailbox_mode", "custom_domain", "local_part")
	return []toolDescriptor{
		{Name: "nerve_onboarding_start", Description: "Start or replay the caller's generation-bound onboarding", InputSchema: map[string]any{
			"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object", "oneOf": []any{managedStart, customStart},
		}, OutputShape: result, ErrorCodes: onboardingBusinessErrorCodes()},
		{Name: "nerve_onboarding_status", Description: "Read the caller's generation-bound onboarding status", InputSchema: inputObject(map[string]any{}), OutputShape: result, ErrorCodes: onboardingBusinessErrorCodes()},
		{Name: "nerve_onboarding_verify_domain", Description: "Poll authoritative domain and provider readiness", InputSchema: inputObject(map[string]any{}), OutputShape: result, ErrorCodes: onboardingBusinessErrorCodes()},
		{Name: "nerve_onboarding_close", Description: "Idempotently close the caller's generation", InputSchema: inputObject(map[string]any{
			"idempotency_key": boundedStringProperty(1, 128), "expected_generation": map[string]any{"type": "integer", "minimum": 1},
		}, "idempotency_key", "expected_generation"), OutputShape: result, ErrorCodes: onboardingBusinessErrorCodes()},
	}
}

func invokeOnboardingTool(ctx context.Context, provisioner OnboardingProvisioner, caller OnboardingCaller, name string, arguments json.RawMessage) (OnboardingResult, error) {
	var result OnboardingResult
	if provisioner == nil {
		return result, errors.New("onboarding provisioner is unavailable")
	}
	var err error
	switch name {
	case "nerve_onboarding_start":
		var input OnboardingStartInput
		if err := decodeOnboardingArguments(arguments, &input); err != nil {
			return result, err
		}
		if err := normalizeOnboardingStartInput(&input); err != nil {
			return result, err
		}
		result, err = provisioner.Start(ctx, caller, input)
	case "nerve_onboarding_status":
		if err := decodeOnboardingArguments(arguments, &struct{}{}); err != nil {
			return result, err
		}
		result, err = provisioner.Status(ctx, caller)
	case "nerve_onboarding_verify_domain":
		if err := decodeOnboardingArguments(arguments, &struct{}{}); err != nil {
			return result, err
		}
		result, err = provisioner.VerifyDomain(ctx, caller)
	case "nerve_onboarding_close":
		var input OnboardingCloseInput
		if err := decodeOnboardingArguments(arguments, &input); err != nil {
			return result, err
		}
		if err := validateOnboardingIdempotencyKey(input.IdempotencyKey); err != nil {
			return result, err
		}
		if input.ExpectedGeneration != caller.Principal.Generation {
			return result, errors.New("expected_generation must match the authenticated generation")
		}
		result, err = provisioner.Close(ctx, caller, input)
	default:
		return result, fmt.Errorf("unknown onboarding tool %q", name)
	}
	if err != nil {
		return result, err
	}
	if err := validateOnboardingToolResult(result, caller.Principal.Generation); err != nil {
		return OnboardingResult{}, err
	}
	return result, nil
}

func validateOnboardingToolResult(result OnboardingResult, expectedGeneration int64) error {
	if result.ResultType != "complete" {
		return errors.New("onboarding provisioner returned a non-complete result")
	}
	onboardingID, err := uuid.Parse(result.OnboardingID)
	if err != nil || onboardingID == uuid.Nil || onboardingID.String() != result.OnboardingID {
		return errors.New("onboarding provisioner returned an invalid onboarding ID")
	}
	if result.Generation != expectedGeneration {
		return errors.New("onboarding provisioner returned the wrong generation")
	}
	switch result.State {
	case "provisioning", "dns_pending", "active", "deprovisioning", "closed":
	default:
		return errors.New("onboarding provisioner returned an invalid state")
	}
	if result.Mode != OnboardingMailboxManaged && result.Mode != OnboardingMailboxCustomDomain {
		return errors.New("onboarding provisioner returned an invalid mailbox mode")
	}
	if len(result.Address) > 320 {
		return errors.New("onboarding provisioner returned an oversized address")
	}
	for _, record := range result.DNSRecords {
		if record.Type == "" || record.Name == "" || record.Value == "" ||
			(record.Priority != nil && (*record.Priority < 0 || *record.Priority > 65535)) {
			return errors.New("onboarding provisioner returned an invalid DNS record")
		}
	}
	for _, check := range result.DNSChecks {
		if check.Name == "" || check.Status == "" {
			return errors.New("onboarding provisioner returned an invalid DNS check")
		}
	}
	return nil
}

func decodeOnboardingArguments(arguments json.RawMessage, target any) error {
	if len(bytes.TrimSpace(arguments)) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	if err := rejectInvalidOnboardingArgumentFields(arguments, onboardingArgumentFields(target)); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid onboarding arguments: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("invalid onboarding arguments: multiple JSON values")
	}
	return nil
}

func onboardingArgumentFields(target any) map[string]struct{} {
	fields := make(map[string]struct{})
	switch target.(type) {
	case *OnboardingStartInput:
		for _, field := range []string{"idempotency_key", "organization_name", "mailbox_mode", "custom_domain", "local_part"} {
			fields[field] = struct{}{}
		}
	case *OnboardingCloseInput:
		fields["idempotency_key"], fields["expected_generation"] = struct{}{}, struct{}{}
	}
	return fields
}

func rejectInvalidOnboardingArgumentFields(arguments json.RawMessage, allowed map[string]struct{}) error {
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return errors.New("invalid onboarding arguments: expected one JSON object")
	}
	seen := make(map[string]struct{})
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("invalid onboarding arguments: %w", err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return errors.New("invalid onboarding arguments: object field is not a string")
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("invalid onboarding arguments: duplicate field %q", key)
		}
		if _, ok := allowed[key]; !ok {
			return errors.New("invalid onboarding arguments: unknown field")
		}
		seen[key] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return fmt.Errorf("invalid onboarding arguments: %w", err)
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return errors.New("invalid onboarding arguments: unterminated JSON object")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("invalid onboarding arguments: multiple JSON values")
	}
	return nil
}

func normalizeOnboardingStartInput(input *OnboardingStartInput) error {
	if input == nil {
		return errors.New("onboarding start input is required")
	}
	if err := validateOnboardingIdempotencyKey(input.IdempotencyKey); err != nil {
		return err
	}
	if !utf8.ValidString(input.OrganizationName) {
		return errors.New("organization_name must be valid UTF-8")
	}
	input.OrganizationName = strings.Join(strings.Fields(input.OrganizationName), " ")
	if input.OrganizationName == "" || utf8.RuneCountInString(input.OrganizationName) > 160 || len(input.OrganizationName) > 640 {
		return errors.New("organization_name must contain 1-160 Unicode scalars and at most 640 UTF-8 bytes")
	}
	switch input.MailboxMode {
	case OnboardingMailboxManaged:
		if input.CustomDomain != "" || input.LocalPart != "" {
			return errors.New("managed_mailbox does not accept custom_domain or local_part")
		}
	case OnboardingMailboxCustomDomain:
		canonicalDomain, err := domains.CanonicalizeDomain(input.CustomDomain)
		if err != nil || len(canonicalDomain) > 253 {
			return errors.New("custom_domain is invalid")
		}
		_, canonicalLocalPart, _, err := emailaddr.Canonicalize(input.LocalPart + "@example.com")
		if err != nil || len(canonicalLocalPart) > 64 {
			return errors.New("local_part is invalid")
		}
		input.CustomDomain, input.LocalPart = canonicalDomain, canonicalLocalPart
	default:
		return errors.New("mailbox_mode must be managed_mailbox or custom_domain")
	}
	return nil
}

func validateOnboardingIdempotencyKey(value string) error {
	if value == "" || !utf8.ValidString(value) || value != strings.TrimSpace(value) || len(value) > 128 {
		return errors.New("idempotency_key must contain 1-128 valid UTF-8 bytes without surrounding whitespace or controls")
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return errors.New("idempotency_key must contain 1-128 valid UTF-8 bytes without surrounding whitespace or controls")
		}
	}
	return nil
}
