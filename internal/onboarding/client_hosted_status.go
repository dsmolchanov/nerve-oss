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

const hostedStatusDelegationPath = "/internal/v1/agent-billing/status"

func (client *Client) BillingStatus(ctx context.Context, caller mcp.BillingCaller) (mcp.BillingStatusResult, error) {
	empty := mcp.BillingStatusResult{}
	if err := validateBillingCaller(caller); err != nil {
		return empty, err
	}
	requestBody, err := json.Marshal(delegationRequest{
		Principal: delegationPrincipal{
			Kind: caller.Principal.Kind, ClientID: caller.Principal.ClientID,
			OrgID: caller.Principal.OrgID, Generation: caller.Principal.Generation,
			TokenID: caller.Principal.TokenID,
		}, Input: struct{}{},
	})
	if err != nil || len(requestBody) > maxDelegationBodyBytes {
		return empty, hostedUpgradeUnavailable()
	}
	requestContext, cancel := context.WithTimeout(ctx, client.timeout)
	defer cancel()
	endpoint := *client.baseURL
	endpoint.Path = hostedStatusDelegationPath
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
	result, businessErr, err := decodeHostedStatusResponse(responseBody)
	if err != nil || (result == nil) == (businessErr == nil) {
		return empty, hostedUpgradeUnavailable()
	}
	if businessErr != nil {
		if response.StatusCode < 400 || response.StatusCode > 599 || !validDelegatedBillingError(businessErr) {
			return empty, hostedUpgradeUnavailable()
		}
		return empty, businessErr
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return empty, hostedUpgradeUnavailable()
	}
	return *result, nil
}

func decodeHostedStatusResponse(body []byte) (*mcp.BillingStatusResult, *mcp.BillingBusinessError, error) {
	if err := rejectDuplicateJSONFields(body); err != nil {
		return nil, nil, err
	}
	top, err := decodeExactJSONObject(body, "hosted status response", "result", "error")
	if err != nil {
		return nil, nil, err
	}
	var wire struct {
		Result *struct {
			ResultType    *string `json:"resultType"`
			HostedState   *string `json:"hosted_state"`
			StarterActive *bool   `json:"starter_active"`
			ActiveTier    *string `json:"active_tier"`
		} `json:"result"`
		Error *billingErrorWire `json:"error"`
	}
	if raw, ok := top["result"]; ok {
		if _, err := decodeExactJSONObject(raw, "hosted status result", "resultType", "hosted_state", "starter_active", "active_tier"); err != nil {
			return nil, nil, err
		}
	}
	if raw, ok := top["error"]; ok {
		if _, err := decodeExactJSONObject(raw, "hosted status error", "code", "retryable", "retry_at"); err != nil {
			return nil, nil, err
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return nil, nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, nil, errors.New("hosted status response has trailing content")
	}
	var result *mcp.BillingStatusResult
	var businessErr *mcp.BillingBusinessError
	if wire.Result != nil {
		if wire.Result.ResultType == nil || *wire.Result.ResultType != "complete" ||
			wire.Result.HostedState == nil ||
			wire.Result.StarterActive == nil || wire.Result.ActiveTier == nil {
			return nil, nil, errors.New("invalid hosted status result")
		}
		result = &mcp.BillingStatusResult{ResultType: *wire.Result.ResultType,
			HostedState: *wire.Result.HostedState, StarterActive: *wire.Result.StarterActive, ActiveTier: *wire.Result.ActiveTier}
		if !mcp.ValidHostedBillingStatus(*result) {
			return nil, nil, errors.New("contradictory hosted paid tier status")
		}
	}
	if wire.Error != nil {
		if wire.Error.Code == nil || wire.Error.Retryable == nil {
			return nil, nil, errors.New("invalid hosted status error")
		}
		businessErr = &mcp.BillingBusinessError{Code: *wire.Error.Code,
			Retryable: *wire.Error.Retryable, RetryAt: wire.Error.RetryAt}
	}
	if (result == nil) == (businessErr == nil) {
		return nil, nil, errors.New("hosted status response requires one outcome")
	}
	return result, businessErr, nil
}
