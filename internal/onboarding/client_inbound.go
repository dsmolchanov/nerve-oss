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

// Inbound uses the original read-scoped bearer plus the existing authenticated
// runtime delegation. Recovery has no automatic retry: a lost response reports
// an unknown outcome, and the caller reads receipts before requesting it again.
func (client *Client) Inbound(ctx context.Context, caller mcp.InboundCaller, operation string, input mcp.InboundInput) (json.RawMessage, error) {
	unavailable := func() (json.RawMessage, error) {
		return nil, &mcp.InboundBusinessError{Code: "inbound_retry_later", Retryable: true}
	}
	if !mcp.ValidInboundCaller(caller) {
		return nil, &mcp.InboundBusinessError{Code: "inbound_unavailable"}
	}
	if !mcp.ValidInboundInput(operation, input) {
		return nil, &mcp.InboundBusinessError{Code: "inbound_invalid_request"}
	}
	body, e := json.Marshal(delegationRequest{Principal: delegationPrincipal{Kind: caller.Principal.Kind, ClientID: caller.Principal.ClientID, OrgID: caller.Principal.OrgID, Generation: caller.Principal.Generation, TokenID: caller.Principal.TokenID}, Input: input})
	if e != nil || len(body) > maxDelegationBodyBytes {
		return unavailable()
	}
	requestContext, cancel := context.WithTimeout(ctx, client.timeout)
	defer cancel()
	endpoint := *client.baseURL
	endpoint.Path = mcp.InboundDelegationPath + operation
	request, e := http.NewRequestWithContext(requestContext, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if e != nil {
		return unavailable()
	}
	timestamp := strconv.FormatInt(client.now().UTC().Unix(), 10)
	nonce := client.nonce()
	if nonce == "" || strings.ContainsAny(nonce, "\r\n") {
		return unavailable()
	}
	hash := sha256.Sum256(body)
	hashHex := hex.EncodeToString(hash[:])
	request.Header.Set("Authorization", caller.Authorization)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set(delegationKeyIDHeader, client.keyID)
	request.Header.Set(delegationNonceHeader, nonce)
	request.Header.Set(delegationTimestampHeader, timestamp)
	request.Header.Set(delegationBodyHashHeader, hashHex)
	request.Header.Set(delegationSignatureHeader, client.signature(request.Method, request.URL.EscapedPath(), nonce, timestamp, hashHex))
	unknown := func() (json.RawMessage, error) {
		if operation == "recover" {
			return nil, &mcp.InboundBusinessError{Code: "inbound_outcome_unknown", Retryable: true}
		}
		return unavailable()
	}
	response, e := client.httpClient.Do(request)
	if e != nil {
		return unknown()
	}
	defer response.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(response.Body, maxDelegationBodyBytes+1))
	if e != nil || len(raw) > maxDelegationBodyBytes {
		return unknown()
	}
	if response.StatusCode == http.StatusOK {
		if strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0])) != "application/json" {
			return unknown()
		}
		if _, e = mcp.ValidateInboundResult(operation, input, raw); e != nil {
			return unknown()
		}
		return raw, nil
	}
	// Never reflect a proxy/provider error body, identifiers or diagnostics.
	switch response.StatusCode {
	case http.StatusBadRequest:
		return nil, &mcp.InboundBusinessError{Code: "inbound_invalid_request"}
	case http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden:
		return nil, &mcp.InboundBusinessError{Code: "inbound_unavailable"}
	case http.StatusConflict:
		if rejectDuplicateJSONFields(raw) == nil {
			d, e := decodeExactJSONObject(raw, "inbound error", "error")
			var code string
			if e == nil && json.Unmarshal(d["error"], &code) == nil {
				switch code {
				case "inbound_unavailable":
					return nil, &mcp.InboundBusinessError{Code: code}
				case "recovery_in_progress_or_changed":
					if operation != "recover" {
						return unavailable()
					}
					return nil, &mcp.InboundBusinessError{Code: code, Retryable: true}
				case "recovery_response_exceeds_limit":
					if operation != "recover" {
						return unavailable()
					}
					return nil, &mcp.InboundBusinessError{Code: code}
				}
			}
		}
	case http.StatusBadGateway:
		if operation != "recover" {
			return unavailable()
		}
		return nil, &mcp.InboundBusinessError{Code: "recovery_provider_or_save_unavailable", Retryable: true}
	}
	return unknown()
}
