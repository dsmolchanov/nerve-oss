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

	"neuralmail/internal/emailtransport"
)

type Config struct {
	APIKey  string
	BaseURL string

	HTTPClient *http.Client
}

type OutboundAdapter struct {
	apiKey string
	base   string
	client *http.Client
}

func NewOutboundAdapter(cfg Config) *OutboundAdapter {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		base = "https://api.resend.com"
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &OutboundAdapter{
		apiKey: strings.TrimSpace(cfg.APIKey),
		base:   strings.TrimRight(base, "/"),
		client: client,
	}
}

func (a *OutboundAdapter) Name() string { return "resend" }

func (a *OutboundAdapter) SendMessage(ctx context.Context, msg emailtransport.OutboundMessage, idempotencyKey string) (string, error) {
	if a.apiKey == "" {
		return "", errors.New("resend api key not configured")
	}
	if strings.TrimSpace(msg.From) == "" {
		return "", errors.New("missing from")
	}
	if len(msg.To) == 0 || strings.TrimSpace(msg.To[0]) == "" {
		return "", errors.New("missing to")
	}
	if strings.TrimSpace(msg.Subject) == "" {
		return "", errors.New("missing subject")
	}
	if strings.TrimSpace(msg.TextBody) == "" && strings.TrimSpace(msg.HTMLBody) == "" {
		return "", errors.New("missing body")
	}

	payload := map[string]any{
		"from":    msg.From,
		"to":      msg.To,
		"subject": msg.Subject,
	}
	if strings.TrimSpace(msg.TextBody) != "" {
		payload["text"] = msg.TextBody
	}
	if strings.TrimSpace(msg.HTMLBody) != "" {
		payload["html"] = msg.HTMLBody
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/emails", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	if strings.TrimSpace(idempotencyKey) != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("resend send failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var parsed struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", err
	}
	if strings.TrimSpace(parsed.ID) == "" {
		return "", errors.New("resend response missing id")
	}
	return parsed.ID, nil
}

func (a *OutboundAdapter) GetDeliveryStatus(context.Context, string) (emailtransport.DeliveryStatus, error) {
	return emailtransport.DeliveryStatusUnknown, emailtransport.ErrNotSupported
}

var _ emailtransport.OutboundAdapter = (*OutboundAdapter)(nil)
