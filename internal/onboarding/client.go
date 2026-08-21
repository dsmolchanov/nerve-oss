package onboarding

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"neuralmail/internal/auth"
	"neuralmail/internal/mcp"
)

const (
	delegationKeyIDHeader     = "X-Nerve-Delegation-Key-Id"
	delegationNonceHeader     = "X-Nerve-Delegation-Nonce"
	delegationTimestampHeader = "X-Nerve-Delegation-Timestamp"
	delegationBodyHashHeader  = "X-Nerve-Delegation-Body-Sha256"
	delegationSignatureHeader = "X-Nerve-Delegation-Signature"
	maxDelegationBodyBytes    = 64 << 10
)

var operationPaths = map[string]string{
	"start":         "/internal/v1/agent-onboarding/start",
	"status":        "/internal/v1/agent-onboarding/status",
	"verify_domain": "/internal/v1/agent-onboarding/verify-domain",
	"close":         "/internal/v1/agent-onboarding/close",
}

type Client struct {
	baseURL    *url.URL
	httpClient *http.Client
	keyID      string
	secret     []byte
	timeout    time.Duration
	now        func() time.Time
	nonce      func() string
}

type ClientConfig struct {
	BaseURL    string
	KeyID      string
	Secret     string
	Timeout    time.Duration
	HTTPClient *http.Client
	Now        func() time.Time
	Nonce      func() string
}

var _ mcp.OnboardingProvisioner = (*Client)(nil)

type delegationPrincipal struct {
	Kind       auth.PrincipalKind `json:"kind"`
	ClientID   string             `json:"client_id"`
	Generation int64              `json:"generation"`
	TokenID    string             `json:"token_id"`
}

type delegationRequest struct {
	Principal delegationPrincipal `json:"principal"`
	Input     any                 `json:"input"`
}

type delegationResponse struct {
	Result *mcp.OnboardingResult        `json:"result,omitempty"`
	Error  *mcp.OnboardingBusinessError `json:"error,omitempty"`
}

func NewClient(cfg ClientConfig) (*Client, error) {
	baseURL, err := validateBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	keyID := strings.TrimSpace(cfg.KeyID)
	if keyID == "" || strings.ContainsAny(keyID, "\r\n") {
		return nil, errors.New("onboarding delegation key ID is required")
	}
	if cfg.Secret == "" {
		return nil, errors.New("onboarding delegation secret is required")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	sourceClient := cfg.HTTPClient
	if sourceClient == nil {
		sourceClient = http.DefaultClient
	}
	httpClient := *sourceClient
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	nonce := cfg.Nonce
	if nonce == nil {
		nonce = func() string {
			return base64.RawURLEncoding.EncodeToString([]byte(uuid.NewString()))
		}
	}
	return &Client{
		baseURL: baseURL, httpClient: &httpClient, keyID: keyID,
		secret: []byte(cfg.Secret), timeout: timeout, now: now, nonce: nonce,
	}, nil
}

func (client *Client) Start(ctx context.Context, caller mcp.OnboardingCaller, input mcp.OnboardingStartInput) (mcp.OnboardingResult, error) {
	return client.call(ctx, "start", caller, input)
}

func (client *Client) Status(ctx context.Context, caller mcp.OnboardingCaller) (mcp.OnboardingResult, error) {
	return client.call(ctx, "status", caller, struct{}{})
}

func (client *Client) VerifyDomain(ctx context.Context, caller mcp.OnboardingCaller) (mcp.OnboardingResult, error) {
	return client.call(ctx, "verify_domain", caller, struct{}{})
}

func (client *Client) Close(ctx context.Context, caller mcp.OnboardingCaller, input mcp.OnboardingCloseInput) (mcp.OnboardingResult, error) {
	return client.call(ctx, "close", caller, input)
}

func (client *Client) call(ctx context.Context, operation string, caller mcp.OnboardingCaller, input any) (mcp.OnboardingResult, error) {
	var empty mcp.OnboardingResult
	if err := validateCaller(caller); err != nil {
		return empty, err
	}
	requestBody, err := json.Marshal(delegationRequest{
		Principal: delegationPrincipal{
			Kind: caller.Principal.Kind, ClientID: caller.Principal.ClientID,
			Generation: caller.Principal.Generation, TokenID: caller.Principal.TokenID,
		},
		Input: input,
	})
	if err != nil {
		return empty, fmt.Errorf("encode onboarding delegation: %w", err)
	}
	if len(requestBody) > maxDelegationBodyBytes {
		return empty, errors.New("onboarding delegation request exceeds 64 KiB")
	}
	requestContext, cancel := context.WithTimeout(ctx, client.timeout)
	defer cancel()
	endpoint := *client.baseURL
	endpoint.Path = operationPaths[operation]
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, endpoint.String(), bytes.NewReader(requestBody))
	if err != nil {
		return empty, fmt.Errorf("construct onboarding delegation: %w", err)
	}
	timestamp := strconv.FormatInt(client.now().UTC().Unix(), 10)
	nonce := client.nonce()
	if nonce == "" || strings.ContainsAny(nonce, "\r\n") {
		return empty, errors.New("onboarding delegation nonce is invalid")
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
	request.Header.Set(delegationSignatureHeader, client.signature(request.Method, request.URL.EscapedPath(), nonce, timestamp, bodyHashHex))

	response, err := client.httpClient.Do(request)
	if err != nil {
		// Once RoundTrip begins, the server may have consumed and committed the
		// request even when no response headers reach us (EOF, reset, timeout,
		// and similar transport failures).  Only validation and construction
		// errors above this point are provably pre-dispatch.
		return empty, mcp.ErrOnboardingOutcomeUnknown
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, maxDelegationBodyBytes+1)
	responseBody, err := io.ReadAll(limited)
	if err != nil {
		// Headers do not prove that a mutating handler did not commit.  A short
		// or interrupted body therefore has the same poll-before-retry contract
		// as a transport timeout.
		return empty, mcp.ErrOnboardingOutcomeUnknown
	}
	if len(responseBody) > maxDelegationBodyBytes {
		return empty, delegationProtocolFailure(operation, errors.New("onboarding delegation response exceeds 64 KiB"))
	}
	if mediaType := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0])); mediaType != "application/json" {
		return empty, delegationProtocolFailure(operation, errors.New("onboarding delegation response is not application/json"))
	}
	var decoded delegationResponse
	if err := decodeDelegationResponse(responseBody, &decoded); err != nil {
		return empty, delegationProtocolFailure(operation, fmt.Errorf("decode onboarding delegation response: %w", err))
	}
	if err := validateDelegationEnvelope(response.StatusCode, decoded, caller.Principal.Generation); err != nil {
		return empty, delegationProtocolFailure(operation, err)
	}
	if decoded.Error != nil {
		return empty, decoded.Error
	}
	return *decoded.Result, nil
}

func decodeDelegationResponse(body []byte, target *delegationResponse) error {
	if err := rejectDuplicateJSONFields(body); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("onboarding delegation response contains multiple JSON values")
	}
	return nil
}

func rejectDuplicateJSONFields(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("onboarding delegation response has a non-string object key")
				}
				if _, duplicate := seen[key]; duplicate {
					return fmt.Errorf("onboarding delegation response repeats field %q", key)
				}
				seen[key] = struct{}{}
				if err := walk(); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim('}') {
				return errors.New("onboarding delegation response has an unterminated object")
			}
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim(']') {
				return errors.New("onboarding delegation response has an unterminated array")
			}
		default:
			return errors.New("onboarding delegation response starts with an unexpected delimiter")
		}
		return nil
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("onboarding delegation response contains multiple JSON values")
	}
	return nil
}

func validateDelegationEnvelope(statusCode int, decoded delegationResponse, expectedGeneration int64) error {
	if (decoded.Result == nil) == (decoded.Error == nil) {
		return errors.New("onboarding delegation response must contain exactly one result or error")
	}
	if decoded.Error != nil {
		if statusCode < http.StatusBadRequest || statusCode > 599 {
			return fmt.Errorf("onboarding delegation business error has inconsistent HTTP %d", statusCode)
		}
		if decoded.Error.Code == "" || len(decoded.Error.Code) > 128 || strings.ContainsAny(decoded.Error.Code, " \t\r\n") {
			return errors.New("onboarding delegation business error code is invalid")
		}
		return nil
	}
	if statusCode < http.StatusOK || statusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("onboarding delegation result has inconsistent HTTP %d", statusCode)
	}
	result := decoded.Result
	if result.ResultType != "complete" {
		return errors.New("onboarding delegation resultType must be complete")
	}
	if _, err := uuid.Parse(result.OnboardingID); err != nil {
		return errors.New("onboarding delegation onboarding_id is invalid")
	}
	if result.Generation != expectedGeneration {
		return errors.New("onboarding delegation generation does not match the authenticated principal")
	}
	switch result.State {
	case "provisioning", "dns_pending", "active", "deprovisioning", "closed":
	default:
		return errors.New("onboarding delegation state is invalid")
	}
	switch result.Mode {
	case mcp.OnboardingMailboxManaged, mcp.OnboardingMailboxCustomDomain:
	default:
		return errors.New("onboarding delegation mode is invalid")
	}
	if len(result.Address) > 320 {
		return errors.New("onboarding delegation address exceeds 320 bytes")
	}
	return nil
}

// Post-dispatch invariant: once a lifecycle mutation may have reached the
// control plane, only a decoded durable result or business error is definitive.
// Every other response/protocol failure is ambiguous and requires polling the
// same generation/key before retrying. Status is read-only and may retain a
// diagnostic protocol error.
func delegationProtocolFailure(operation string, err error) error {
	if operation == "status" {
		return err
	}
	return fmt.Errorf("%w: %v", mcp.ErrOnboardingOutcomeUnknown, err)
}

func (client *Client) signature(method, escapedPath, nonce, timestamp, bodyHash string) string {
	canonical := strings.Join([]string{client.keyID, nonce, timestamp, method, escapedPath, bodyHash}, "\n")
	mac := hmac.New(sha256.New, client.secret)
	_, _ = mac.Write([]byte(canonical))
	return hex.EncodeToString(mac.Sum(nil))
}

func validateCaller(caller mcp.OnboardingCaller) error {
	if caller.Principal.Kind != auth.PrincipalM2MOnboarding || caller.Principal.ClientID == "" || caller.Principal.Generation <= 0 {
		return errors.New("onboarding caller is not a generation-bound M2M onboarding principal")
	}
	authorization := strings.TrimSpace(caller.Authorization)
	parts := strings.Fields(authorization)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" || strings.ContainsAny(authorization, "\r\n") {
		return errors.New("onboarding caller bearer is required")
	}
	return nil
}

func validateBaseURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, errors.New("onboarding control-plane base URL must be an origin")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())) {
		return nil, errors.New("onboarding control-plane base URL must use HTTPS")
	}
	parsed.Path = ""
	return parsed, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if address := net.ParseIP(host); address != nil {
		return address.IsLoopback()
	}
	return false
}
