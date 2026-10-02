package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"

	"github.com/google/uuid"
)

const billingPairingConfirmToolName = "nerve_billing_pairing_confirm"

type BillingPairingConfirmInput struct {
	PairingID            string `json:"pairing_id"`
	BrowserSessionSHA256 string `json:"browser_session_sha256"`
	Challenge            string `json:"challenge"`
}

type BillingPairingConfirmResult struct {
	ResultType string `json:"resultType"`
	State      string `json:"state"`
	OfferID    string `json:"offer_id"`
	UpgradeURL string `json:"upgrade_url"`
}

func ValidBillingPairingConfirmInput(input BillingPairingConfirmInput) bool {
	id, e := uuid.Parse(input.PairingID)
	session, e2 := hex.DecodeString(input.BrowserSessionSHA256)
	challenge, e3 := base64.RawURLEncoding.DecodeString(input.Challenge)
	return e == nil && id != uuid.Nil && id.String() == input.PairingID && e2 == nil && len(session) == 32 && hex.EncodeToString(session) == input.BrowserSessionSHA256 && e3 == nil && len(challenge) == 32 && base64.RawURLEncoding.EncodeToString(challenge) == input.Challenge
}

func ValidBillingPairingConfirmResult(result BillingPairingConfirmResult, dashboardBaseURL string) bool {
	if result.ResultType != "complete" || result.State != "completed" || result.OfferID != starterOfferID || len(result.UpgradeURL) > 2048 || !validBillingDashboardOrigin(dashboardBaseURL) {
		return false
	}
	base, _ := url.Parse(dashboardBaseURL)
	target, e := url.Parse(result.UpgradeURL)
	if e != nil || target.Scheme != base.Scheme || target.Host != base.Host || target.User != nil || target.Fragment != "" || target.Path != "/billing/pairing" || target.RawPath != "" || target.Opaque != "" {
		return false
	}
	query, e := url.ParseQuery(target.RawQuery)
	if e != nil || len(query) != 1 || len(query["pairing_id"]) != 1 {
		return false
	}
	id, e := uuid.Parse(query.Get("pairing_id"))
	return e == nil && id != uuid.Nil && id.String() == query.Get("pairing_id")
}

func billingPairingConfirmToolDescriptor() toolDescriptor {
	return toolDescriptor{Name: billingPairingConfirmToolName, Description: "Confirm the owner's browser pairing proof using the original authenticated agent; this does not activate a paid plan",
		InputSchema: inputObject(map[string]any{"pairing_id": boundedStringProperty(36, 36), "browser_session_sha256": boundedStringProperty(64, 64), "challenge": boundedStringProperty(43, 43)}, "pairing_id", "browser_session_sha256", "challenge"),
		OutputShape: outputObject(map[string]any{"resultType": map[string]any{"type": "string", "const": "complete"}, "state": map[string]any{"type": "string", "const": "completed"}, "offer_id": map[string]any{"type": "string", "const": starterOfferID}, "upgrade_url": map[string]any{"type": "string", "format": "uri", "maxLength": 2048}}, "resultType", "state", "offer_id", "upgrade_url"), ErrorCodes: billingBusinessErrorCodes()}
}

func decodeBillingPairingArguments(raw json.RawMessage) (out BillingPairingConfirmInput, err error) {
	if len(raw) < 2 || len(raw) > 1024 {
		return out, errors.New("invalid pairing input")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	token, e := d.Token()
	if e != nil || token != json.Delim('{') {
		return out, errors.New("invalid pairing input")
	}
	fields := map[string]bool{}
	for d.More() {
		token, e = d.Token()
		name, ok := token.(string)
		if e != nil || !ok || fields[name] || (name != "pairing_id" && name != "browser_session_sha256" && name != "challenge") {
			return out, errors.New("invalid pairing input")
		}
		fields[name] = true
		var v json.RawMessage
		if d.Decode(&v) != nil {
			return out, errors.New("invalid pairing input")
		}
	}
	if token, e = d.Token(); e != nil || token != json.Delim('}') || len(fields) != 3 || d.Decode(&struct{}{}) != io.EOF {
		return out, errors.New("invalid pairing input")
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&out) != nil || !ValidBillingPairingConfirmInput(out) {
		return out, errors.New("invalid pairing input")
	}
	return out, nil
}

func invokeBillingPairingConfirmTool(ctx context.Context, provisioner HostedBillingProvisioner, caller BillingCaller, raw json.RawMessage, base string) (BillingPairingConfirmResult, error) {
	empty := BillingPairingConfirmResult{}
	if provisioner == nil {
		return empty, billingTemporarilyUnavailable()
	}
	if !isActiveBillingPrincipalForTool(caller.Principal) {
		return empty, billingInvalidState()
	}
	input, e := decodeBillingPairingArguments(raw)
	if e != nil {
		return empty, billingInvalidRequest()
	}
	result, e := provisioner.ConfirmBillingPairing(ctx, caller, input)
	if e != nil {
		return empty, sanitizeBillingProvisionerError(e)
	}
	if !ValidBillingPairingConfirmResult(result, base) {
		return empty, billingTemporarilyUnavailable()
	}
	target, _ := url.Parse(result.UpgradeURL)
	if target.Query().Get("pairing_id") != input.PairingID {
		return empty, billingTemporarilyUnavailable()
	}
	return result, nil
}
