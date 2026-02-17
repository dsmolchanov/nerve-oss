package resend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	ID     string         `json:"id"`
	Name   string         `json:"name"`
	Status string         `json:"status"`
	Records []DomainRecord `json:"records,omitempty"`
	Region string         `json:"region,omitempty"`
}

type DomainsClient struct {
	apiKey string
	base   string
	client *http.Client
}

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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/domains", bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.auth(req); err != nil {
		return out, err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, fmt.Errorf("resend create domain failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var parsed struct {
		Data Domain `json:"data"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return out, err
	}
	if strings.TrimSpace(parsed.Data.ID) == "" {
		return out, errors.New("resend response missing domain id")
	}
	return parsed.Data, nil
}

func (c *DomainsClient) GetDomain(ctx context.Context, id string) (Domain, error) {
	var out Domain
	id = strings.TrimSpace(id)
	if id == "" {
		return out, errors.New("missing domain id")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/domains/"+id, nil)
	if err != nil {
		return out, err
	}
	if err := c.auth(req); err != nil {
		return out, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, fmt.Errorf("resend get domain failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var parsed struct {
		Data Domain `json:"data"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return out, err
	}
	if strings.TrimSpace(parsed.Data.ID) == "" {
		return out, errors.New("resend response missing domain id")
	}
	return parsed.Data, nil
}

func (c *DomainsClient) VerifyDomain(ctx context.Context, id string) (Domain, error) {
	var out Domain
	id = strings.TrimSpace(id)
	if id == "" {
		return out, errors.New("missing domain id")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/domains/"+id+"/verify", nil)
	if err != nil {
		return out, err
	}
	if err := c.auth(req); err != nil {
		return out, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, fmt.Errorf("resend verify domain failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var parsed struct {
		Data Domain `json:"data"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return out, err
	}
	if strings.TrimSpace(parsed.Data.ID) == "" {
		return out, errors.New("resend response missing domain id")
	}
	return parsed.Data, nil
}

func (c *DomainsClient) ListDomains(ctx context.Context) ([]Domain, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/domains", nil)
	if err != nil {
		return nil, err
	}
	if err := c.auth(req); err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("resend list domains failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var parsed struct {
		Data []Domain `json:"data"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, err
	}
	return parsed.Data, nil
}
