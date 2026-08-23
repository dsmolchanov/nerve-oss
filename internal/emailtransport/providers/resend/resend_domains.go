package resend

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"neuralmail/internal/domains"
)

type DomainRecord struct {
	Record   string `json:"record"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	TTL      string `json:"ttl,omitempty"`
	Status   string `json:"status,omitempty"`
	Value    string `json:"value"`
	Priority int    `json:"priority,omitempty"`
}

type DomainCapabilities struct {
	Sending   string `json:"sending"`
	Receiving string `json:"receiving"`
}

type Domain struct {
	ID           string             `json:"id"`
	Name         string             `json:"name"`
	Status       string             `json:"status"`
	Records      []DomainRecord     `json:"records,omitempty"`
	Region       string             `json:"region,omitempty"`
	Capabilities DomainCapabilities `json:"capabilities"`
}

// DomainQuarantineObservation is an inventory-only provider identity. It is
// safe to persist as a discrepancy record but never authorizes adoption or a
// provider mutation.
type DomainQuarantineObservation struct {
	ProviderDomainID string
	InventorySHA256  string
}

// APIError is a bounded, redacted representation of a non-success Resend
// response. Provider response messages and bodies are intentionally omitted:
// they may contain addresses, request data, or other tenant-controlled bytes.
type APIError struct {
	Operation  string
	StatusCode int
	Code       string
	Retryable  bool
}

func (err *APIError) Error() string {
	if err == nil {
		return "resend api request failed"
	}
	return fmt.Sprintf("resend %s failed: status=%d code=%s", err.Operation, err.StatusCode, err.Code)
}

// IsNotFound reports whether an error is an authoritative provider 404.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// IsDefinitiveCreateRejection reports the narrow provider response that proves
// POST /domains was rejected by validation before materialization. Auth,
// conflict, rate-limit, transport, and server failures are all ambiguous and
// must remain provider-unknown.
func IsDefinitiveCreateRejection(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr == nil || apiErr.Retryable ||
		apiErr.Operation != "create_domain" || apiErr.Code != "validation_error" {
		return false
	}
	return apiErr.StatusCode == http.StatusBadRequest || apiErr.StatusCode == http.StatusUnprocessableEntity
}

// BuildDomainQuarantineObservations validates and deterministically hashes a
// bounded canonical-name inventory. The resulting identities are deliberately
// observation-only: callers may quarantine them but must not bind them to a
// local workflow without a separately protected adoption receipt.
func BuildDomainQuarantineObservations(matches []Domain) ([]DomainQuarantineObservation, error) {
	if len(matches) == 0 || len(matches) > 128 {
		return nil, errors.New("resend domain quarantine inventory is invalid")
	}
	ordered := append([]Domain(nil), matches...)
	sort.Slice(ordered, func(left, right int) bool {
		if ordered[left].ID == ordered[right].ID {
			return ordered[left].Name < ordered[right].Name
		}
		return ordered[left].ID < ordered[right].ID
	})
	var inventory strings.Builder
	seen := make(map[string]struct{}, len(ordered))
	for _, match := range ordered {
		canonical, err := domains.CanonicalizeDomain(match.Name)
		if err != nil || ValidateDomainID(match.ID) != nil {
			return nil, errors.New("resend domain quarantine inventory is invalid")
		}
		providerID := match.ID
		if _, duplicate := seen[providerID]; duplicate {
			return nil, errors.New("resend domain quarantine inventory has duplicate identity")
		}
		seen[providerID] = struct{}{}
		inventory.WriteString(providerID)
		inventory.WriteByte(0)
		inventory.WriteString(canonical)
		inventory.WriteByte(0)
		inventory.WriteString(strings.ToLower(strings.TrimSpace(match.Status)))
		inventory.WriteByte(0)
	}
	digest := sha256.Sum256([]byte(inventory.String()))
	inventorySHA := fmt.Sprintf("%x", digest[:])
	observations := make([]DomainQuarantineObservation, 0, len(ordered))
	for _, match := range ordered {
		observations = append(observations, DomainQuarantineObservation{
			ProviderDomainID: match.ID,
			InventorySHA256:  inventorySHA,
		})
	}
	return observations, nil
}

// ValidateDomainID accepts exactly the bounded, path-safe provider identity
// that may be persisted and later used by the exact-ID Resend operations. It
// deliberately rejects surrounding whitespace rather than silently changing
// provider-returned identity bytes.
func ValidateDomainID(value string) error {
	if !domains.IsExactProviderResourceID(value, maxResendDomainIDBytes) {
		return errors.New("invalid domain id")
	}
	return nil
}

var (
	ErrDomainPaginationBound = errors.New("resend domain pagination exceeded the bounded lookup window")
	ErrDomainPaginationCycle = errors.New("resend domain pagination repeated a cursor")
)

type DomainsClient struct {
	apiKey string
	base   string
	client *http.Client
}

const (
	maxResendRequestAttempts = 4
	resendRetryBaseDelay     = 250 * time.Millisecond
	resendRetryMaxDelay      = 2 * time.Second
	maxResendResponseBytes   = 2 << 20
	maxResendDomainIDBytes   = 256
	maxResendDomainPages     = 10
	resendDomainPageSize     = 100
	maxResendErrorCodeBytes  = 64
)

func NewDomainsClient(cfg Config) *DomainsClient {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		base = "https://api.resend.com"
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &DomainsClient{
		apiKey: strings.TrimSpace(cfg.APIKey),
		base:   strings.TrimRight(base, "/"),
		client: client,
	}
}

func (c *DomainsClient) auth(req *http.Request) error {
	if strings.TrimSpace(c.apiKey) == "" {
		return errors.New("resend api key not configured")
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("User-Agent", "nerve-email/1.0")
	return nil
}

func (c *DomainsClient) CreateDomain(ctx context.Context, name string) (Domain, error) {
	var out Domain
	name = strings.TrimSpace(name)
	if name == "" {
		return out, errors.New("missing domain name")
	}

	body, err := json.Marshal(map[string]any{"name": name})
	if err != nil {
		return out, err
	}
	// Domain creation has no provider idempotency key. An HTTP failure can be
	// ambiguous after Resend receives the request, so this operation must never
	// be retried by the client. The onboarding lifecycle quarantines a later
	// canonical-name-only observation instead of risking a duplicate create.
	status, respBody, err := c.doRequest(ctx, http.MethodPost, "/domains", body, "application/json", false)
	if err != nil {
		return out, err
	}
	if status < 200 || status >= 300 {
		return out, apiError("create_domain", status, respBody)
	}
	out, err = parseDomain(respBody)
	if err != nil {
		return out, err
	}
	return out, nil
}

func (c *DomainsClient) GetDomain(ctx context.Context, id string) (Domain, error) {
	var out Domain
	var err error
	id, err = boundedDomainID(id)
	if err != nil {
		return out, err
	}
	status, respBody, err := c.doRequest(ctx, http.MethodGet, "/domains/"+url.PathEscape(id), nil, "", true)
	if err != nil {
		return out, err
	}
	if status < 200 || status >= 300 {
		return out, apiError("get_domain", status, respBody)
	}
	return parseDomain(respBody)
}

// VerifyDomain accepts Resend's ID-only acknowledgement and then performs an
// authoritative GET. POST success starts an asynchronous verification cycle;
// it is not itself readiness evidence.
func (c *DomainsClient) VerifyDomain(ctx context.Context, id string) (Domain, error) {
	var acknowledged Domain
	var err error
	id, err = boundedDomainID(id)
	if err != nil {
		return acknowledged, err
	}
	status, respBody, err := c.doRequest(ctx, http.MethodPost, "/domains/"+url.PathEscape(id)+"/verify", nil, "", true)
	if err != nil {
		return acknowledged, err
	}
	if status < 200 || status >= 300 {
		return acknowledged, apiError("verify_domain", status, respBody)
	}
	acknowledged, err = parseDomain(respBody)
	if err != nil {
		return acknowledged, err
	}
	if acknowledged.ID != id {
		return acknowledged, errors.New("resend verify acknowledgement changed domain identity")
	}
	// Preserve the legacy full-response behavior used by local test doubles,
	// while handling Resend's documented ID-only production acknowledgement.
	if strings.TrimSpace(acknowledged.Name) == "" {
		authoritative, readErr := c.GetDomain(ctx, id)
		if readErr != nil {
			return acknowledged, readErr
		}
		return authoritative, nil
	}
	return acknowledged, nil
}

func (c *DomainsClient) ListDomains(ctx context.Context) ([]Domain, error) {
	return c.listDomains(ctx, "")
}

// FindDomainsByCanonicalName returns every provider record with the same DNS
// identity. It deliberately does not choose or adopt a winner: a name-only
// match after an uncertain create must remain quarantined until an explicit
// adoption receipt binds one exact provider ID.
func (c *DomainsClient) FindDomainsByCanonicalName(ctx context.Context, name string) ([]Domain, error) {
	canonical, err := domains.CanonicalizeDomain(name)
	if err != nil {
		return nil, err
	}
	return c.listDomains(ctx, canonical)
}

func (c *DomainsClient) listDomains(ctx context.Context, canonicalFilter string) ([]Domain, error) {
	var matches []Domain
	after := ""
	seenCursors := make(map[string]struct{}, maxResendDomainPages)
	for pageNumber := 0; pageNumber < maxResendDomainPages; pageNumber++ {
		page, err := c.listDomainsPage(ctx, after)
		if err != nil {
			return nil, err
		}
		for _, item := range page.Data {
			if canonicalFilter != "" {
				candidate, canonicalErr := domains.CanonicalizeDomain(item.Name)
				if canonicalErr != nil {
					return nil, errors.New("resend domain inventory contains an invalid canonical name")
				}
				if candidate != canonicalFilter {
					continue
				}
			}
			matches = append(matches, item)
		}
		if !page.HasMore {
			return matches, nil
		}
		if len(page.Data) == 0 {
			return nil, errors.New("resend domain inventory has_more without a cursor")
		}
		next, err := boundedDomainID(page.Data[len(page.Data)-1].ID)
		if err != nil {
			return nil, errors.New("resend domain inventory returned an invalid cursor")
		}
		if _, exists := seenCursors[next]; exists {
			return nil, ErrDomainPaginationCycle
		}
		seenCursors[next] = struct{}{}
		after = next
	}
	return nil, ErrDomainPaginationBound
}

type domainListPage struct {
	Data    []Domain `json:"data"`
	HasMore bool     `json:"has_more"`
}

func (c *DomainsClient) listDomainsPage(ctx context.Context, after string) (domainListPage, error) {
	values := url.Values{}
	values.Set("limit", strconv.Itoa(resendDomainPageSize))
	if after != "" {
		values.Set("after", after)
	}
	status, respBody, err := c.doRequest(ctx, http.MethodGet, "/domains?"+values.Encode(), nil, "", true)
	if err != nil {
		return domainListPage{}, err
	}
	if status < 200 || status >= 300 {
		return domainListPage{}, apiError("list_domains", status, respBody)
	}
	return parseDomainListPage(respBody)
}

// DeleteDomain is idempotent. Lifecycle cleanup follows it with GetDomain so
// local cleanup never advances without an authoritative absence readback.
func (c *DomainsClient) DeleteDomain(ctx context.Context, id string) error {
	var err error
	id, err = boundedDomainID(id)
	if err != nil {
		return err
	}
	status, respBody, err := c.doRequest(ctx, http.MethodDelete, "/domains/"+url.PathEscape(id), nil, "", true)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return nil
	}
	if status < 200 || status >= 300 {
		return apiError("delete_domain", status, respBody)
	}
	return nil
}

// SetReceiving changes either receiving capability state and returns a fresh
// authoritative snapshot. Resend's PATCH response is ID-only.
func (c *DomainsClient) SetReceiving(ctx context.Context, domainID string, enabled bool) (Domain, error) {
	return c.setReceiving(ctx, domainID, enabled, true)
}

func (c *DomainsClient) setReceiving(ctx context.Context, domainID string, enabled, authoritative bool) (Domain, error) {
	var acknowledged Domain
	var err error
	domainID, err = boundedDomainID(domainID)
	if err != nil {
		return acknowledged, err
	}
	state := "disabled"
	if enabled {
		state = "enabled"
	}
	payload := map[string]any{
		"capabilities": map[string]string{
			"receiving": state,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return acknowledged, err
	}
	status, respBody, err := c.doRequest(ctx, http.MethodPatch, "/domains/"+url.PathEscape(domainID), body, "application/json", true)
	if err != nil {
		return acknowledged, err
	}
	if status < 200 || status >= 300 {
		return acknowledged, apiError("set_receiving", status, respBody)
	}
	acknowledged, err = parseDomain(respBody)
	if err != nil {
		return acknowledged, err
	}
	if acknowledged.ID != domainID {
		return acknowledged, errors.New("resend update acknowledgement changed domain identity")
	}
	if !authoritative {
		return acknowledged, nil
	}
	readback, readErr := c.GetDomain(ctx, domainID)
	if readErr != nil {
		return acknowledged, readErr
	}
	return readback, nil
}

// EnableReceiving preserves the legacy call surface.
func (c *DomainsClient) EnableReceiving(ctx context.Context, domainID string) error {
	_, err := c.setReceiving(ctx, domainID, true, false)
	return err
}

func parseDomain(body []byte) (Domain, error) {
	var direct Domain
	if err := json.Unmarshal(body, &direct); err == nil && strings.TrimSpace(direct.ID) != "" {
		return direct, nil
	}

	var wrapped struct {
		Data Domain `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapped); err == nil && strings.TrimSpace(wrapped.Data.ID) != "" {
		return wrapped.Data, nil
	}

	return Domain{}, errors.New("resend response missing domain id")
}

func parseDomainListPage(body []byte) (domainListPage, error) {
	var wrapped domainListPage
	if err := json.Unmarshal(body, &wrapped); err == nil && wrapped.Data != nil {
		return wrapped, nil
	}

	var direct []Domain
	if err := json.Unmarshal(body, &direct); err == nil {
		return domainListPage{Data: direct}, nil
	}

	return domainListPage{}, errors.New("resend response missing domains list")
}

func apiError(operation string, status int, body []byte) error {
	var envelope struct {
		Name string `json:"name"`
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &envelope)
	code := safeAPIErrorCode(envelope.Name)
	if code == "" {
		code = safeAPIErrorCode(envelope.Code)
	}
	if code == "" {
		code = "http_status_" + strconv.Itoa(status)
	}
	return &APIError{
		Operation: operation, StatusCode: status, Code: code,
		Retryable: isRetryableResendStatus(status),
	}
}

func safeAPIErrorCode(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxResendErrorCodeBytes {
		return ""
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z') && !(character >= '0' && character <= '9') && character != '_' && character != '-' {
			return ""
		}
	}
	return value
}

func boundedDomainID(value string) (string, error) {
	if err := ValidateDomainID(value); err != nil {
		return "", err
	}
	return value, nil
}

// canRetry describes provider semantics rather than just the HTTP verb:
// verify and desired-state capability updates are exact-ID idempotent, while
// POST /domains is not and must always use a single attempt.
func (c *DomainsClient) doRequest(ctx context.Context, method, path string, body []byte, contentType string, canRetry bool) (int, []byte, error) {
	attemptLimit := 1
	if canRetry {
		attemptLimit = maxResendRequestAttempts
	}
	for attempt := 0; attempt < attemptLimit; attempt++ {
		var bodyReader io.Reader
		if body != nil {
			bodyReader = bytes.NewReader(body)
		}

		req, err := http.NewRequestWithContext(ctx, method, c.base+path, bodyReader)
		if err != nil {
			return 0, nil, err
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if err := c.auth(req); err != nil {
			return 0, nil, err
		}

		resp, err := c.client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResendResponseBytes+1))
		resp.Body.Close()
		if readErr != nil {
			return 0, nil, errors.New("read bounded resend response")
		}
		if len(respBody) > maxResendResponseBytes {
			return 0, nil, errors.New("resend response exceeds bounded size")
		}

		if !isRetryableResendStatus(resp.StatusCode) || attempt == attemptLimit-1 {
			return resp.StatusCode, respBody, nil
		}

		delay := resendRetryDelay(resp.Header.Get("Retry-After"), attempt)
		select {
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	return 0, nil, errors.New("resend request retry exhausted")
}

func isRetryableResendStatus(status int) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	return status >= 500 && status < 600
}

func resendRetryDelay(retryAfter string, attempt int) time.Duration {
	token := strings.TrimSpace(retryAfter)
	if token != "" {
		if seconds, err := strconv.Atoi(token); err == nil && seconds > 0 {
			return minDuration(time.Duration(seconds)*time.Second, resendRetryMaxDelay)
		}
		if at, err := http.ParseTime(token); err == nil {
			wait := time.Until(at)
			if wait > 0 {
				return minDuration(wait, resendRetryMaxDelay)
			}
		}
	}

	delay := resendRetryBaseDelay * time.Duration(1<<attempt)
	return minDuration(delay, resendRetryMaxDelay)
}

func minDuration(left, right time.Duration) time.Duration {
	if left < right {
		return left
	}
	return right
}
