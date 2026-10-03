package onboarding

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"neuralmail/internal/mcp"
)

// FreeSetup preserves the original setup-only bearer. After any ambiguous
// mutation response the agent must poll this same generation; no retry occurs.
func (client *Client) FreeSetup(ctx context.Context, caller mcp.OnboardingCaller, operation string, input json.RawMessage) (mcp.FreeSetupResult, error) {
	empty := mcp.FreeSetupResult{}
	unavailable := func() error {
		return &mcp.OnboardingBusinessError{Code: mcp.OnboardingErrorTemporarilyUnavailable, Retryable: true}
	}
	if client == nil || !mcp.ValidFreeSetupCaller(caller) {
		return empty, unavailable()
	}
	normalized, err := mcp.DecodeFreeSetupInput(operation, caller.Principal.Generation, input)
	if err != nil {
		return empty, err
	}
	body, err := json.Marshal(delegationRequest{Principal: delegationPrincipal{Kind: caller.Principal.Kind, ClientID: caller.Principal.ClientID, Generation: caller.Principal.Generation, TokenID: caller.Principal.TokenID}, Input: normalized})
	if err != nil || len(body) > maxDelegationBodyBytes {
		return empty, unavailable()
	}
	bounded, cancel := context.WithTimeout(ctx, client.timeout)
	defer cancel()
	endpoint := *client.baseURL
	endpoint.Path = mcp.FreeSetupDelegationPath + operation
	request, err := http.NewRequestWithContext(bounded, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return empty, unavailable()
	}
	timestamp := strconv.FormatInt(client.now().UTC().Unix(), 10)
	nonce := client.nonce()
	if nonce == "" || strings.ContainsAny(nonce, "\r\n") {
		return empty, unavailable()
	}
	hash := sha256.Sum256(body)
	digest := hex.EncodeToString(hash[:])
	request.Header.Set("Authorization", caller.Authorization)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set(delegationKeyIDHeader, client.keyID)
	request.Header.Set(delegationNonceHeader, nonce)
	request.Header.Set(delegationTimestampHeader, timestamp)
	request.Header.Set(delegationBodyHashHeader, digest)
	request.Header.Set(delegationSignatureHeader, client.signature(request.Method, request.URL.EscapedPath(), nonce, timestamp, digest))
	unknown := func() error {
		if operation != "status" {
			return mcp.ErrOnboardingOutcomeUnknown
		}
		return unavailable()
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return empty, unknown()
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxDelegationBodyBytes+1))
	if err != nil || len(raw) > maxDelegationBodyBytes || strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0])) != "application/json" {
		return empty, unknown()
	}
	// Apply the strict duplicate/null/closed schema parser before typed decoding.
	if rejectDuplicateJSONFields(raw) != nil {
		return empty, unknown()
	}
	top, err := decodeExactJSONObject(raw, "Free setup response", "result", "error")
	if err != nil || len(top) != 1 {
		return empty, unknown()
	}
	if value, ok := top["result"]; ok {
		result, err := mcp.DecodeFreeSetupResult(value, caller.Principal.Generation)
		if err != nil || response.StatusCode != http.StatusOK || mcp.ValidateFreeSetupOperationResult(operation, normalized, result, caller.Principal.Generation) != nil {
			return empty, unknown()
		}
		return result, nil
	}
	value := top["error"]
	fields, err := decodeExactJSONObject(value, "Free setup business error", "code", "retryable", "retry_at")
	if err != nil {
		return empty, unknown()
	}
	for _, key := range []string{"code", "retryable"} {
		if _, ok := fields[key]; !ok {
			return empty, unknown()
		}
	}
	var wire onboardingBusinessErrorWire
	if json.Unmarshal(value, &wire) != nil || wire.Code == nil || wire.Retryable == nil {
		return empty, unknown()
	}
	business := &mcp.OnboardingBusinessError{Code: *wire.Code, Retryable: *wire.Retryable, RetryAt: wire.RetryAt}
	if !mcp.IsPublicOnboardingBusinessErrorCode(business.Code) || response.StatusCode < 400 || response.StatusCode > 599 {
		return empty, unknown()
	}
	return empty, business
}
