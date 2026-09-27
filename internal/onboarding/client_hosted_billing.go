package onboarding

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"neuralmail/internal/mcp"
)

const hostedUpgradeDelegationPath = "/internal/v1/agent-billing/upgrade"

type hostedUpgradeResponseWire struct {
	Result *struct {
		ResultType *string `json:"resultType"`
		State      *string `json:"state"`
		OfferID    *string `json:"offer_id"`
		UpgradeURL *string `json:"upgrade_url"`
	} `json:"result,omitempty"`
	Error *billingErrorWire `json:"error,omitempty"`
}

func (client *Client) Upgrade(ctx context.Context, caller mcp.BillingCaller,
	input mcp.BillingUpgradeInput) (mcp.BillingUpgradeResult, error) {
	empty := mcp.BillingUpgradeResult{}
	if err := validateBillingCaller(caller); err != nil {
		return empty, err
	}
	if input.IdempotencyKey == "" || len(input.IdempotencyKey) > 128 ||
		strings.TrimSpace(input.IdempotencyKey) != input.IdempotencyKey {
		return empty, &mcp.BillingBusinessError{Code: mcp.BillingErrorInvalidRequest}
	}
	requestBody, err := json.Marshal(delegationRequest{
		Principal: delegationPrincipal{
			Kind: caller.Principal.Kind, ClientID: caller.Principal.ClientID,
			OrgID: caller.Principal.OrgID, Generation: caller.Principal.Generation,
			TokenID: caller.Principal.TokenID,
		}, Input: input,
	})
	if err != nil || len(requestBody) > maxDelegationBodyBytes {
		return empty, hostedUpgradeUnavailable()
	}
	requestContext, cancel := context.WithTimeout(ctx, client.timeout)
	defer cancel()
	endpoint := *client.baseURL
	endpoint.Path = hostedUpgradeDelegationPath
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost,
		endpoint.String(), bytes.NewReader(requestBody))
	if err != nil {
		return empty, hostedUpgradeUnavailable()
	}
	timestamp := strconv.FormatInt(client.now().UTC().Unix(), 10)
	nonce := client.nonce()
	if nonce == "" || strings.ContainsAny(nonce, "\r\n") {
		return empty, hostedUpgradeUnavailable()
	}
	bodyHash := sha256.Sum256(requestBody)
	bodyHashHex := hex.EncodeToString(bodyHash[:])
	request.Header.Set("Authorization", strings.TrimSpace(caller.Authorization))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set(delegationKeyIDHeader, client.keyID)
	request.Header.Set(delegationNonceHeader, nonce)
	request.Header.Set(delegationTimestampHeader, timestamp)
	request.Header.Set(delegationBodyHashHeader, bodyHashHex)
	request.Header.Set(delegationSignatureHeader,
		client.signature(request.Method, request.URL.EscapedPath(), nonce, timestamp, bodyHashHex))
	response, err := client.httpClient.Do(request)
	if err != nil {
		return empty, hostedUpgradeUnavailable()
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxDelegationBodyBytes+1))
	if err != nil || len(responseBody) > maxDelegationBodyBytes ||
		strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0])) != "application/json" {
		return empty, hostedUpgradeUnavailable()
	}
	decoded, err := decodeHostedUpgradeResponse(responseBody)
	if err != nil || (decoded.Result == nil) == (decoded.Error == nil) {
		return empty, hostedUpgradeUnavailable()
	}
	if decoded.Error != nil {
		if response.StatusCode < 400 || response.StatusCode > 599 || !validDelegatedBillingError(decoded.Error) {
			return empty, hostedUpgradeUnavailable()
		}
		return empty, decoded.Error
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return empty, hostedUpgradeUnavailable()
	}
	return *decoded.Result, nil
}

func decodeHostedUpgradeResponse(body []byte) (struct {
	Result *mcp.BillingUpgradeResult
	Error  *mcp.BillingBusinessError
}, error) {
	var decoded struct {
		Result *mcp.BillingUpgradeResult
		Error  *mcp.BillingBusinessError
	}
	if err := rejectDuplicateJSONFields(body); err != nil {
		return decoded, err
	}
	top, err := decodeExactJSONObject(body, "hosted upgrade response", "result", "error")
	if err != nil {
		return decoded, err
	}
	if raw, ok := top["result"]; ok {
		if _, err := decodeExactJSONObject(raw, "hosted upgrade result", "resultType", "state", "offer_id", "upgrade_url"); err != nil {
			return decoded, err
		}
	}
	if raw, ok := top["error"]; ok {
		if _, err := decodeExactJSONObject(raw, "hosted upgrade error", "code", "retryable", "retry_at"); err != nil {
			return decoded, err
		}
	}
	var wire hostedUpgradeResponseWire
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return decoded, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return decoded, errors.New("hosted upgrade response has trailing content")
	}
	if wire.Result != nil {
		if wire.Result.ResultType == nil || wire.Result.State == nil ||
			wire.Result.OfferID == nil || wire.Result.UpgradeURL == nil ||
			*wire.Result.ResultType != "complete" || !validHostedUpgradeState(*wire.Result.State) ||
			*wire.Result.OfferID != "starter_2026_09_v2" || *wire.Result.UpgradeURL == "" ||
			len(*wire.Result.UpgradeURL) > 2048 {
			return decoded, errors.New("invalid hosted upgrade result")
		}
		decoded.Result = &mcp.BillingUpgradeResult{ResultType: *wire.Result.ResultType,
			State: *wire.Result.State, OfferID: *wire.Result.OfferID,
			UpgradeURL: *wire.Result.UpgradeURL}
	}
	if wire.Error != nil {
		if wire.Error.Code == nil || wire.Error.Retryable == nil {
			return decoded, errors.New("hosted upgrade error omits a required field")
		}
		decoded.Error = &mcp.BillingBusinessError{Code: *wire.Error.Code,
			Retryable: *wire.Error.Retryable, RetryAt: wire.Error.RetryAt}
	}
	return decoded, nil
}

func hostedUpgradeUnavailable() error {
	return &mcp.BillingBusinessError{Code: mcp.BillingErrorTemporarilyUnavailable, Retryable: true}
}

func validHostedUpgradeState(state string) bool {
	switch state {
	case "awaiting_owner", "session_prepared", "session_open", "provider_unknown":
		return true
	default:
		return false
	}
}
