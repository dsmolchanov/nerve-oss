package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"neuralmail/internal/auth"
)

const billingUpgradeToolName = "nerve_billing_upgrade"
const starterOfferID = "starter_2026_09_v2"

// HostedBillingProvisioner delegates a human-confirmed purchase invitation to
// the control plane. It never accepts payment, owner, tenant, or Price fields
// from the tool caller.
type HostedBillingProvisioner interface {
	Upgrade(context.Context, BillingCaller, BillingUpgradeInput) (BillingUpgradeResult, error)
}

type BillingUpgradeInput struct {
	IdempotencyKey string `json:"idempotency_key"`
}

type BillingUpgradeResult struct {
	ResultType string `json:"resultType"`
	State      string `json:"state"`
	OfferID    string `json:"offer_id"`
	UpgradeURL string `json:"upgrade_url"`
}

func billingUpgradeToolDescriptor() toolDescriptor {
	return toolDescriptor{
		Name:        billingUpgradeToolName,
		Description: "Request an owner-confirmed Starter upgrade link for the authenticated organization",
		InputSchema: inputObject(map[string]any{
			"idempotency_key": boundedStringProperty(1, 128),
		}, "idempotency_key"),
		OutputShape: outputObject(map[string]any{
			"resultType":  map[string]any{"type": "string", "const": "complete"},
			"state":       map[string]any{"type": "string", "const": "awaiting_owner"},
			"offer_id":    map[string]any{"type": "string", "const": starterOfferID},
			"upgrade_url": map[string]any{"type": "string", "format": "uri", "maxLength": 2048},
		}, "resultType", "state", "offer_id", "upgrade_url"),
		ErrorCodes: billingBusinessErrorCodes(),
	}
}

func billingUpgradeToolAvailable(server *Server, principal auth.Principal) bool {
	return server != nil && server.HostedBilling != nil && server.Config.Cloud.Mode &&
		server.Auth != nil && isActiveBillingPrincipalForTool(principal) &&
		validBillingDashboardOrigin(server.Config.Cloud.DashboardBaseURL) &&
		server.Auth.ValidateScopes(principal, "nerve:billing.subscribe") == nil
}

func invokeBillingUpgradeTool(ctx context.Context, provisioner HostedBillingProvisioner,
	caller BillingCaller, arguments json.RawMessage, dashboardBaseURL string) (BillingUpgradeResult, error) {
	if provisioner == nil {
		return BillingUpgradeResult{}, billingTemporarilyUnavailable()
	}
	if !isActiveBillingPrincipalForTool(caller.Principal) {
		return BillingUpgradeResult{}, billingInvalidState()
	}
	var input BillingUpgradeInput
	if err := decodeBillingUpgradeArguments(arguments, &input); err != nil ||
		validateBillingIdempotencyKey(input.IdempotencyKey) != nil {
		return BillingUpgradeResult{}, billingInvalidRequest()
	}
	result, err := provisioner.Upgrade(ctx, caller, input)
	if err != nil {
		return BillingUpgradeResult{}, sanitizeBillingProvisionerError(err)
	}
	if err := validateBillingUpgradeResult(result, dashboardBaseURL); err != nil {
		return BillingUpgradeResult{}, billingTemporarilyUnavailable()
	}
	return result, nil
}

func decodeBillingUpgradeArguments(arguments json.RawMessage, target *BillingUpgradeInput) error {
	if len(arguments) == 0 || len(arguments) > 1024 {
		return errors.New("invalid billing upgrade argument size")
	}
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return errors.New("billing upgrade arguments must be one object")
	}
	seen := false
	for decoder.More() {
		field, err := decoder.Token()
		if err != nil || field != "idempotency_key" || seen {
			return errors.New("invalid billing upgrade field")
		}
		seen = true
		if err := decoder.Decode(&target.IdempotencyKey); err != nil {
			return errors.New("invalid idempotency key")
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || !seen {
		return errors.New("invalid billing upgrade object")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("billing upgrade arguments contain multiple values")
	}
	return nil
}

func validateBillingUpgradeResult(result BillingUpgradeResult, dashboardBaseURL string) error {
	if result.ResultType != "complete" || result.State != "awaiting_owner" ||
		result.OfferID != starterOfferID || len(result.UpgradeURL) > 2048 {
		return errors.New("invalid billing upgrade result")
	}
	base, err := url.Parse(dashboardBaseURL)
	if err != nil || !validBillingDashboardOrigin(dashboardBaseURL) {
		return errors.New("invalid dashboard origin")
	}
	target, err := url.Parse(result.UpgradeURL)
	if err != nil || target.Scheme != base.Scheme || target.Host != base.Host ||
		target.User != nil || target.Fragment != "" || target.Path != "/billing/upgrade" ||
		target.RawPath != "" || target.Opaque != "" {
		return errors.New("invalid billing upgrade URL")
	}
	query, err := url.ParseQuery(target.RawQuery)
	if err != nil {
		return errors.New("invalid billing upgrade query")
	}
	if len(query) != 1 || len(query["intent"]) != 1 {
		return errors.New("invalid billing upgrade intent")
	}
	intent, err := uuid.Parse(query.Get("intent"))
	if err != nil || intent == uuid.Nil || intent.String() != query.Get("intent") {
		return errors.New("invalid billing upgrade intent")
	}
	return nil
}

func validBillingDashboardOrigin(value string) bool {
	base, err := url.Parse(value)
	return err == nil && base.Scheme == "https" && base.Host != "" &&
		base.User == nil && base.RawQuery == "" && base.Fragment == "" &&
		strings.TrimRight(base.Path, "/") == "" && base.Opaque == ""
}
