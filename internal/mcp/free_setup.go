package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"neuralmail/internal/auth"
	"neuralmail/internal/domains"
	"neuralmail/internal/emailaddr"
	"strings"
	"time"
	"unicode/utf8"
)

// Free setup is a distinct modern contract. Ordinary trial/managed start and
// the frozen onboarding response retain their meaning.
type FreeSetupInput struct {
	IdempotencyKey   string `json:"idempotency_key"`
	OrganizationName string `json:"organization_name"`
	ApexDomain       string `json:"apex_domain"`
	LocalPart        string `json:"local_part"`
}

type FreeSetupResult struct {
	ResultType           string                `json:"resultType"`
	SetupID              string                `json:"setup_id"`
	OnboardingID         string                `json:"onboarding_id"`
	Generation           int64                 `json:"generation"`
	ApexDomain           string                `json:"apex_domain"`
	SetupState           string                `json:"setup_state"`
	State                string                `json:"state"`
	SetupExpiresAt       time.Time             `json:"setup_expires_at"`
	OwnershipTXT         OnboardingDNSRecord   `json:"ownership_txt"`
	DNSRecords           []OnboardingDNSRecord `json:"dns_records,omitempty"`
	NextAction           string                `json:"next_action"`
	Reauthorize          bool                  `json:"reauthorize"`
	Address              string                `json:"address,omitempty"`
	ReturnReceiptID      string                `json:"return_receipt_id,omitempty"`
	ReturnIdempotencyKey string                `json:"return_idempotency_key,omitempty"`
}

func DecodeFreeSetupInput(operation string, generation int64, raw json.RawMessage) (json.RawMessage, error) {
	if !utf8.Valid(raw) {
		return nil, onboardingInvalidRequest()
	}
	doc, err := inboundObject(raw)
	if err != nil {
		return nil, onboardingInvalidRequest()
	}
	switch operation {
	case "setup":
		if len(doc) != 4 || !inboundKeys(doc, "idempotency_key organization_name apex_domain local_part") {
			return nil, onboardingInvalidRequest()
		}
		var input FreeSetupInput
		if json.Unmarshal(raw, &input) != nil {
			return nil, onboardingInvalidRequest()
		}
		ordinary := OnboardingStartInput{IdempotencyKey: input.IdempotencyKey, OrganizationName: input.OrganizationName, MailboxMode: OnboardingMailboxCustomDomain, CustomDomain: input.ApexDomain, LocalPart: input.LocalPart}
		if normalizeOnboardingStartInput(&ordinary) != nil {
			return nil, onboardingInvalidRequest()
		}
		apex, err := domains.CanonicalizeFreeApex(ordinary.CustomDomain)
		if err != nil {
			return nil, onboardingInvalidRequest()
		}
		input = FreeSetupInput{IdempotencyKey: ordinary.IdempotencyKey, OrganizationName: ordinary.OrganizationName, ApexDomain: apex, LocalPart: ordinary.LocalPart}
		return json.Marshal(input)
	case "status", "verify-domain":
		if len(doc) != 0 {
			return nil, onboardingInvalidRequest()
		}
		return json.RawMessage(`{}`), nil
	case "resume":
		if len(doc) != 1 || !inboundKeys(doc, "idempotency_key") || generation < 1 {
			return nil, onboardingInvalidRequest()
		}
		var input struct {
			IdempotencyKey string `json:"idempotency_key"`
		}
		if json.Unmarshal(raw, &input) != nil || validateOnboardingIdempotencyKey(input.IdempotencyKey) != nil {
			return nil, onboardingInvalidRequest()
		}
		return json.Marshal(input)
	case "close":
		if len(doc) != 2 || !inboundKeys(doc, "idempotency_key expected_generation") {
			return nil, onboardingInvalidRequest()
		}
		var input OnboardingCloseInput
		if json.Unmarshal(raw, &input) != nil || validateOnboardingIdempotencyKey(input.IdempotencyKey) != nil || generation < 1 || input.ExpectedGeneration != generation {
			return nil, onboardingInvalidRequest()
		}
		return json.Marshal(input)
	}
	return nil, onboardingInvalidRequest()
}

func DecodeFreeSetupResult(raw []byte, generation int64) (FreeSetupResult, error) {
	if !utf8.Valid(raw) {
		return FreeSetupResult{}, onboardingTemporarilyUnavailable()
	}
	doc, err := inboundObject(raw)
	if err != nil || !inboundKeys(doc, "resultType setup_id onboarding_id generation apex_domain setup_state state setup_expires_at ownership_txt dns_records next_action reauthorize address return_receipt_id return_idempotency_key") {
		return FreeSetupResult{}, onboardingTemporarilyUnavailable()
	}
	for _, key := range []string{"resultType", "setup_id", "onboarding_id", "generation", "apex_domain", "setup_state", "state", "setup_expires_at", "ownership_txt", "next_action", "reauthorize"} {
		if _, ok := doc[key]; !ok {
			return FreeSetupResult{}, onboardingTemporarilyUnavailable()
		}
	}
	recordOK := func(v any) bool {
		d, ok := v.(map[string]any)
		if !ok || !inboundKeys(d, "type name value priority") {
			return false
		}
		for _, k := range []string{"type", "name", "value"} {
			if _, ok := d[k].(string); !ok {
				return false
			}
		}
		return true
	}
	if !recordOK(doc["ownership_txt"]) {
		return FreeSetupResult{}, onboardingTemporarilyUnavailable()
	}
	if v, ok := doc["dns_records"]; ok {
		records, ok := v.([]any)
		if !ok || len(records) > 32 {
			return FreeSetupResult{}, onboardingTemporarilyUnavailable()
		}
		for _, record := range records {
			if !recordOK(record) {
				return FreeSetupResult{}, onboardingTemporarilyUnavailable()
			}
		}
	}
	for _, key := range []string{"return_receipt_id", "return_idempotency_key"} {
		if value, present := doc[key]; present {
			text, ok := value.(string)
			if !ok || text == "" {
				return FreeSetupResult{}, onboardingTemporarilyUnavailable()
			}
		}
	}
	var result FreeSetupResult
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&result) != nil || ValidateFreeSetupResult(result, generation) != nil {
		return FreeSetupResult{}, onboardingTemporarilyUnavailable()
	}
	return result, nil
}

func ValidateFreeSetupResult(result FreeSetupResult, generation int64) error {
	if result.ResultType != "complete" || result.Generation != generation || generation <= 0 {
		return errors.New("invalid Free setup result authority")
	}
	for _, id := range []string{result.SetupID, result.OnboardingID} {
		parsed, err := uuid.Parse(id)
		if err != nil || parsed == uuid.Nil || parsed.String() != id {
			return errors.New("invalid Free setup result identity")
		}
	}
	if result.ReturnReceiptID != "" || result.ReturnIdempotencyKey != "" {
		receipt, err := uuid.Parse(result.ReturnReceiptID)
		if err != nil || receipt == uuid.Nil || receipt.String() != result.ReturnReceiptID || validateOnboardingIdempotencyKey(result.ReturnIdempotencyKey) != nil || result.State != "active" || result.SetupState != "proof_verified" {
			return errors.New("invalid Free return provenance")
		}
	}
	apex, err := domains.CanonicalizeFreeApex(result.ApexDomain)
	if err != nil || apex != result.ApexDomain || (result.SetupExpiresAt.IsZero() || result.SetupExpiresAt.Year() < 1 || result.SetupExpiresAt.Year() > 9999) {
		return errors.New("invalid Free setup apex/deadline")
	}
	record := result.OwnershipTXT
	if record.Type != "TXT" || record.Name != domains.OwnershipTXTLabel+"."+apex || record.Priority != nil || !validFreeOwnershipChallenge(record.Value) {
		return errors.New("invalid Free setup ownership instructions")
	}
	if len(result.DNSRecords) > 32 {
		return errors.New("oversized Free DNS instructions")
	}
	for _, record := range result.DNSRecords {
		if (record.Type != "TXT" && record.Type != "MX" && record.Type != "CNAME") || record.Name == "" || len(record.Name) > 253 || record.Value == "" || len(record.Value) > 1024 || !utf8.ValidString(record.Name) || !utf8.ValidString(record.Value) || strings.ContainsAny(record.Name, "\r\n\x00") || strings.ContainsAny(record.Value, "\r\n\x00") || (record.Priority != nil && (record.Type != "MX" || *record.Priority < 0 || *record.Priority > 65535)) {
			return errors.New("invalid Free mail DNS instruction")
		}
	}
	switch result.SetupState {
	case "pending", "proof_verified", "terminal":
	default:
		return errors.New("invalid Free setup state")
	}
	switch result.State {
	case "provisioning", "dns_pending":
		if result.Reauthorize || result.Address != "" || result.SetupState == "terminal" || (result.SetupState == "pending" && len(result.DNSRecords) != 0) {
			return errors.New("Free setup published mail before activation")
		}
		if result.SetupState == "pending" && result.NextAction != "configure_ownership_dns_then_verify" {
			return errors.New("invalid ownership next action")
		}
		if result.SetupState == "proof_verified" && result.NextAction != "wait_for_domain_setup" && (result.NextAction != "configure_mail_dns_then_verify" || len(result.DNSRecords) == 0) {
			return errors.New("invalid readiness next action")
		}
	case "active":
		canonical, _, domain, addressErr := emailaddr.Canonicalize(result.Address)
		if result.SetupState != "proof_verified" || !result.Reauthorize || result.NextAction != "reauthorize_org" || addressErr != nil || canonical != result.Address || domain != apex || len(result.DNSRecords) != 0 {
			return errors.New("invalid active Free setup result")
		}
	case "deprovisioning":
		if result.Reauthorize || result.Address != "" || result.NextAction != "poll_close" || len(result.DNSRecords) != 0 {
			return errors.New("invalid Free cleanup result")
		}
	case "closed":
		if result.Reauthorize || result.Address != "" || result.NextAction != "closed" || len(result.DNSRecords) != 0 {
			return errors.New("invalid closed Free setup result")
		}
	default:
		return errors.New("invalid Free lifecycle result")
	}
	return nil
}

func validFreeOwnershipChallenge(value string) bool {
	const prefix = "nerve-free-verification="
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+64 {
		return false
	}
	for _, ch := range value[len(prefix):] {
		if !(ch >= '0' && ch <= '9') && !(ch >= 'a' && ch <= 'f') {
			return false
		}
	}
	return true
}

const FreeSetupDelegationPath = "/internal/v1/agent-free/"

type FreeSetupProvisioner interface {
	FreeSetup(context.Context, OnboardingCaller, string, json.RawMessage) (FreeSetupResult, error)
}

func ValidFreeSetupCaller(c OnboardingCaller) bool {
	p := c.Principal
	parts := strings.Fields(c.Authorization)
	return p.Kind == auth.PrincipalM2MOnboarding && p.AuthMethod == "m2m_bearer" && p.ClientID != "" && p.OrgID == "" && p.Generation > 0 && p.TokenID != "" && len(p.Scopes) == 1 && p.Scopes[0] == "nerve:onboarding" && len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") && parts[1] != "" && c.Authorization == strings.TrimSpace(c.Authorization) && !strings.ContainsAny(c.Authorization, "\r\n")
}
func freeSetupToolOperation(name string) string {
	switch name {
	case "nerve_free_setup":
		return "setup"
	case "nerve_free_status":
		return "status"
	case "nerve_free_verify_domain":
		return "verify-domain"
	case "nerve_free_close":
		return "close"
	case "nerve_free_resume":
		return "resume"
	}
	return ""
}
func freeSetupToolsAvailable(s *Server, p auth.Principal) bool {
	return s != nil && s.Config.Cloud.Mode && s.Auth != nil && s.FreeSetup != nil && ValidFreeSetupCaller(OnboardingCaller{Principal: p, Authorization: "Bearer visibility"})
}
func freeSetupToolDescriptors() []toolDescriptor {
	dns := outputObject(map[string]any{"type": map[string]any{"type": "string", "enum": []string{"TXT", "MX", "CNAME"}}, "name": boundedStringProperty(1, 253), "value": boundedStringProperty(1, 1024), "priority": map[string]any{"type": "integer", "minimum": 0, "maximum": 65535}}, "type", "name", "value")
	props := map[string]any{"resultType": map[string]any{"type": "string", "const": "complete"}, "setup_id": uuidStringProperty(), "onboarding_id": uuidStringProperty(), "generation": map[string]any{"type": "integer", "minimum": 1}, "apex_domain": boundedStringProperty(1, 253), "setup_state": map[string]any{"type": "string", "enum": []string{"pending", "proof_verified", "terminal"}}, "state": map[string]any{"type": "string", "enum": []string{"provisioning", "dns_pending", "active", "deprovisioning", "closed"}}, "setup_expires_at": map[string]any{"type": "string", "format": "date-time"}, "ownership_txt": dns, "dns_records": map[string]any{"type": "array", "maxItems": 32, "items": dns}, "next_action": map[string]any{"type": "string", "enum": []string{"configure_ownership_dns_then_verify", "wait_for_domain_setup", "configure_mail_dns_then_verify", "reauthorize_org", "poll_close", "closed"}}, "reauthorize": map[string]any{"type": "boolean"}, "address": boundedStringProperty(1, 320), "return_receipt_id": uuidStringProperty(), "return_idempotency_key": boundedStringProperty(1, 128)}
	result := outputObject(props, "resultType", "setup_id", "onboarding_id", "generation", "apex_domain", "setup_state", "state", "setup_expires_at", "ownership_txt", "next_action", "reauthorize")
	return []toolDescriptor{
		{Name: "nerve_free_setup", Description: "Start or replay a gated setup-only Free generation for one owned registrable apex; no mail before fresh TXT and complete domain readiness", InputSchema: inputObject(map[string]any{"idempotency_key": boundedStringProperty(1, 128), "organization_name": boundedStringProperty(1, 160), "apex_domain": boundedStringProperty(1, 253), "local_part": boundedStringProperty(1, 64)}, "idempotency_key", "organization_name", "apex_domain", "local_part"), OutputShape: result, ErrorCodes: onboardingBusinessErrorCodes()},
		{Name: "nerve_free_resume", Description: "Explicitly request Free after terminal paid cleanup on the same retained apex; fresh readiness and compatible resources required; replay the exact saved decision key", InputSchema: inputObject(map[string]any{"idempotency_key": boundedStringProperty(1, 128)}, "idempotency_key"), OutputShape: result, ErrorCodes: onboardingBusinessErrorCodes()},
		{Name: "nerve_free_status", Description: "Read retained Free setup history for the caller's exact generation", InputSchema: inputObject(map[string]any{}), OutputShape: result, ErrorCodes: onboardingBusinessErrorCodes()},
		{Name: "nerve_free_verify_domain", Description: "Verify exact apex ownership before provider provisioning, then poll mail DNS readiness", InputSchema: inputObject(map[string]any{}), OutputShape: result, ErrorCodes: onboardingBusinessErrorCodes()},
		{Name: "nerve_free_close", Description: "Close the exact Free generation while retaining its history and uncertain provider outcomes", InputSchema: inputObject(map[string]any{"idempotency_key": boundedStringProperty(1, 128), "expected_generation": map[string]any{"type": "integer", "minimum": 1}}, "idempotency_key", "expected_generation"), OutputShape: result, ErrorCodes: onboardingBusinessErrorCodes()},
	}
}
func invokeFreeSetupTool(ctx context.Context, p FreeSetupProvisioner, c OnboardingCaller, name string, input json.RawMessage) (FreeSetupResult, error) {
	if p == nil || !ValidFreeSetupCaller(c) {
		return FreeSetupResult{}, onboardingTemporarilyUnavailable()
	}
	op := freeSetupToolOperation(name)
	normalized, err := DecodeFreeSetupInput(op, c.Principal.Generation, input)
	if err != nil {
		return FreeSetupResult{}, err
	}
	result, err := p.FreeSetup(ctx, c, op, normalized)
	if err != nil {
		return FreeSetupResult{}, sanitizeOnboardingProvisionerError(err)
	}
	if ValidateFreeSetupOperationResult(op, normalized, result, c.Principal.Generation) != nil {
		if op == "resume" {
			return FreeSetupResult{}, ErrOnboardingOutcomeUnknown
		}
		return FreeSetupResult{}, onboardingTemporarilyUnavailable()
	}
	return result, nil
}

// A retained active resource label is not proof of this mutation. Resume must
// return the exact saved decision key and a durable canonical receipt.
func ValidateFreeSetupOperationResult(operation string, input json.RawMessage, result FreeSetupResult, generation int64) error {
	if err := ValidateFreeSetupResult(result, generation); err != nil {
		return err
	}
	if operation != "resume" {
		return nil
	}
	var request struct {
		IdempotencyKey string `json:"idempotency_key"`
	}
	if json.Unmarshal(input, &request) != nil || validateOnboardingIdempotencyKey(request.IdempotencyKey) != nil || result.State != "active" || result.ReturnReceiptID == "" || result.ReturnIdempotencyKey != request.IdempotencyKey {
		return errors.New("Free resume lacks exact durable decision")
	}
	return nil
}
