package resend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ReceivedEmail is the response from GET /emails/receiving/{id}
type ReceivedEmail struct {
	ID          string               `json:"id"`
	Object      string               `json:"object"`
	From        string               `json:"from"`
	To          []string             `json:"to"`
	CC          []string             `json:"cc"`
	BCC         []string             `json:"bcc"`
	ReplyTo     []string             `json:"reply_to"`
	Subject     string               `json:"subject"`
	HTML        string               `json:"html"`
	Text        string               `json:"text"`
	Headers     map[string]string    `json:"headers"`
	MessageID   string               `json:"message_id"`
	CreatedAt   string               `json:"created_at"`
	Attachments []ReceivedAttachment `json:"attachments"`
}

type ReceivedAttachment struct {
	ID                 string `json:"id"`
	Filename           string `json:"filename"`
	ContentType        string `json:"content_type"`
	ContentDisposition string `json:"content_disposition"`
	ContentID          string `json:"content_id"`
}

// ReceivingClient fetches inbound email content from the Resend Receiving API.
type ReceivingClient struct {
	apiKey string
	base   string
	client *http.Client
}

func NewReceivingClient(cfg Config) *ReceivingClient {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		base = "https://api.resend.com"
	}
	client := cfg.HTTPClient
	if client == nil {
		// Timeout must be shorter than Resend's webhook timeout (~5s).
		// If this fetch hangs past the webhook timeout, Resend will retry
		// the webhook, and its own API latency creates a retry loop.
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return &ReceivingClient{
		apiKey: strings.TrimSpace(cfg.APIKey),
		base:   strings.TrimRight(base, "/"),
		client: client,
	}
}

// GetReceivedEmail fetches full content of an inbound email by its Resend email ID.
func (c *ReceivingClient) GetReceivedEmail(ctx context.Context, emailID string) (*ReceivedEmail, error) {
	url := fmt.Sprintf("%s/emails/receiving/%s", c.base, emailID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("resend receiving: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var email ReceivedEmail
	if err := json.Unmarshal(body, &email); err != nil {
		return nil, fmt.Errorf("resend receiving: unmarshal: %w", err)
	}
	return &email, nil
}

// AttachmentDownload holds the response from fetching a single attachment.
type AttachmentDownload struct {
	ID          string `json:"id"`
	DownloadURL string `json:"download_url"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
}

// GetAttachment fetches a single attachment by its ID from a received email.
// Returns a fresh download_url (valid ~1 hour) that can be proxied to the client.
func (c *ReceivingClient) GetAttachment(ctx context.Context, emailID, attachmentID string) (*AttachmentDownload, error) {
	url := fmt.Sprintf("%s/emails/receiving/%s/attachments/%s", c.base, emailID, attachmentID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("attachment not found (Resend retention may have expired)")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("resend attachment: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var dl AttachmentDownload
	if err := json.Unmarshal(body, &dl); err != nil {
		return nil, fmt.Errorf("resend attachment: unmarshal: %w", err)
	}
	return &dl, nil
}
