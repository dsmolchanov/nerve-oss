package resend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
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

type Domain struct {
	ID      string         `json:"id"`
	Name    string         `json:"name"`
	Status  string         `json:"status"`
	Records []DomainRecord `json:"records,omitempty"`
	Region  string         `json:"region,omitempty"`
}

type DomainsClient struct {
	apiKey string
	base   string
	client *http.Client
}

const (
	maxResendRequestAttempts = 4
	resendRetryBaseDelay     = 250 * time.Millisecond
	resendRetryMaxDelay      = 2 * time.Second
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
	status, respBody, err := c.doRequest(ctx, http.MethodPost, "/domains", body, "application/json")
	if err != nil {
		return out, err
	}
	if status < 200 || status >= 300 {
		return out, fmt.Errorf("resend create domain failed: status=%d body=%s", status, strings.TrimSpace(string(respBody)))
	}
	return parseDomain(respBody)
}

func (c *DomainsClient) GetDomain(ctx context.Context, id string) (Domain, error) {
	var out Domain
	id = strings.TrimSpace(id)
	if id == "" {
		return out, errors.New("missing domain id")
	}
	status, respBody, err := c.doRequest(ctx, http.MethodGet, "/domains/"+id, nil, "")
	if err != nil {
		return out, err
	}
	if status < 200 || status >= 300 {
		return out, fmt.Errorf("resend get domain failed: status=%d body=%s", status, strings.TrimSpace(string(respBody)))
	}
	return parseDomain(respBody)
}

func (c *DomainsClient) VerifyDomain(ctx context.Context, id string) (Domain, error) {
	var out Domain
	id = strings.TrimSpace(id)
	if id == "" {
		return out, errors.New("missing domain id")
	}
	status, respBody, err := c.doRequest(ctx, http.MethodPost, "/domains/"+id+"/verify", nil, "")
	if err != nil {
		return out, err
	}
	if status < 200 || status >= 300 {
		return out, fmt.Errorf("resend verify domain failed: status=%d body=%s", status, strings.TrimSpace(string(respBody)))
	}
	return parseDomain(respBody)
}

func (c *DomainsClient) ListDomains(ctx context.Context) ([]Domain, error) {
	status, respBody, err := c.doRequest(ctx, http.MethodGet, "/domains", nil, "")
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("resend list domains failed: status=%d body=%s", status, strings.TrimSpace(string(respBody)))
	}

	return parseDomainList(respBody)
}

// EnableReceiving enables the receiving capability on a Resend domain.
// Idempotent — safe to call multiple times. Per Resend's Update Domain API,
// omitted capability fields keep their current values.
func (c *DomainsClient) EnableReceiving(ctx context.Context, domainID string) error {
	domainID = strings.TrimSpace(domainID)
	if domainID == "" {
		return errors.New("missing domain id")
	}
	payload := map[string]any{
		"capabilities": map[string]string{
			"receiving": "enabled",
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	status, respBody, err := c.doRequest(ctx, http.MethodPatch, "/domains/"+domainID, body, "application/json")
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("resend enable receiving failed: status=%d body=%s", status, strings.TrimSpace(string(respBody)))
	}
	return nil
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

func parseDomainList(body []byte) ([]Domain, error) {
	var wrapped struct {
		Data []Domain `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapped); err == nil && wrapped.Data != nil {
		return wrapped.Data, nil
	}

	var direct []Domain
	if err := json.Unmarshal(body, &direct); err == nil {
		return direct, nil
	}

	return nil, errors.New("resend response missing domains list")
}

func (c *DomainsClient) doRequest(ctx context.Context, method, path string, body []byte, contentType string) (int, []byte, error) {
	for attempt := 0; attempt < maxResendRequestAttempts; attempt++ {
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
		respBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return 0, nil, readErr
		}

		if !isRetryableResendStatus(resp.StatusCode) || attempt == maxResendRequestAttempts-1 {
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
			return time.Duration(seconds) * time.Second
		}
		if at, err := http.ParseTime(token); err == nil {
			wait := time.Until(at)
			if wait > 0 {
				return wait
			}
		}
	}

	delay := resendRetryBaseDelay * time.Duration(1<<attempt)
	if delay > resendRetryMaxDelay {
		delay = resendRetryMaxDelay
	}
	return delay
}
