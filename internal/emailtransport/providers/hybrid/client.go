package hybrid

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

const (
	// Cloud bounds an ordinary hybrid body at 4 KiB and a send at 1 MiB. Read
	// no more than a send's worth back so a hostile or broken peer cannot make
	// this runtime allocate without limit.
	maxResponseBytes = 1 << 20
	defaultTimeout   = 10 * time.Second
)

var (
	// ErrAuthority is Cloud refusing this installation: revoked, wrong key,
	// wrong generation, scope withdrawn. Retrying unchanged cannot succeed.
	ErrAuthority = errors.New("hybrid installation authority denied")

	// ErrUnavailable is a transport fault or a Cloud-side error. The same call
	// may succeed on the next tick.
	ErrUnavailable = errors.New("hybrid cloud unavailable")

	// ErrPayloadConflict means this operation key was already accepted with
	// different content. The caller must not rewrite it under the same key.
	ErrPayloadConflict = errors.New("hybrid operation payload conflict")

	// ErrSendLimit is a bounded refusal: a recipient, rate or capacity ceiling.
	ErrSendLimit = errors.New("hybrid send limit reached")

	// ErrMissing is a delivery or receipt Cloud does not have. For a lease it
	// usually means the lease expired and the delivery went back to the queue.
	ErrMissing = errors.New("hybrid delivery or receipt unavailable")
)

// Delivery is one inbound message Cloud is holding for this installation.
type Delivery struct {
	ID                string `json:"id"`
	OrgID             string `json:"org_id"`
	InstallationID    string `json:"installation_id"`
	InboxID           string `json:"inbox_id"`
	ProviderMessageID string `json:"provider_message_id"`
	Subject           string `json:"subject"`
	Body              string `json:"body"`
	Sender            string `json:"sender"`
	Recipient         string `json:"recipient"`
	State             string `json:"state"`
	LeaseToken        string `json:"lease_token"`
}

// SendReceipt is Cloud's durable record of one outbound operation. Status
// walks accepted -> claimed -> sent, or stops at uncertain when Cloud crashed
// after the provider may already have taken the message.
type SendReceipt struct {
	OrgID             string `json:"org_id"`
	InstallationID    string `json:"installation_id"`
	OperationKey      string `json:"operation_key"`
	Status            string `json:"status"`
	ProviderMessageID string `json:"provider_message_id"`
	TechnicalUnits    int64  `json:"technical_units"`
}

// SendRequest is one outbound message. The operation key is the caller's
// identity for this send: Cloud returns the first accepted result for a repeat
// and refuses a different payload under the same key.
type SendRequest struct {
	OperationKey string   `json:"operation_key"`
	Kind         string   `json:"kind"`
	To           []string `json:"to"`
	Subject      string   `json:"subject"`
	Body         string   `json:"body"`
}

// Client speaks the Cloud hybrid machine API for exactly one installation.
type Client struct {
	HTTPClient *http.Client
	BaseURL    string
	Tokens     *TokenSource

	InstallationID string
	InboxID        string
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: defaultTimeout}
}

// Poll leases the next inbound delivery, or returns ErrMissing when the
// mailbox is empty. The lease is short: fetch or ack before it expires, or
// Cloud hands the same delivery out again.
func (c *Client) Poll(ctx context.Context) (Delivery, error) {
	var body struct {
		Delivery *Delivery `json:"delivery"`
	}
	if err := c.call(ctx, "poll", map[string]any{}, &body); err != nil {
		return Delivery{}, err
	}
	if body.Delivery == nil || body.Delivery.ID == "" {
		return Delivery{}, ErrMissing
	}
	return *body.Delivery, nil
}

// Ack tells Cloud the delivery is durably stored here. Cloud may then purge
// its content copy, so never ack before the local write has committed.
func (c *Client) Ack(ctx context.Context, deliveryID, leaseToken string) error {
	return c.call(ctx, "ack", map[string]any{"delivery_id": deliveryID, "lease_token": leaseToken}, nil)
}

// Send submits one outbound message for the Cloud provider to deliver.
func (c *Client) Send(ctx context.Context, request SendRequest) (SendReceipt, error) {
	var receipt SendReceipt
	input := map[string]any{
		"operation_key": request.OperationKey, "kind": request.Kind,
		"to": request.To, "subject": request.Subject, "body": request.Body,
	}
	if err := c.call(ctx, "send", input, &receipt); err != nil {
		return SendReceipt{}, err
	}
	return receipt, nil
}

// Receipt reads back the durable record for one operation key. It is how a
// runtime that crashed mid-send learns whether Cloud accepted the message,
// without submitting it a second time.
func (c *Client) Receipt(ctx context.Context, operationKey string) (SendReceipt, error) {
	var receipt SendReceipt
	if err := c.call(ctx, "receipt", map[string]any{"operation_key": operationKey}, &receipt); err != nil {
		return SendReceipt{}, err
	}
	return receipt, nil
}

// Status confirms the installation is still live at Cloud.
func (c *Client) Status(ctx context.Context) (string, error) {
	var body struct {
		InstallationID string `json:"installation_id"`
		State          string `json:"state"`
	}
	if err := c.call(ctx, "status", map[string]any{}, &body); err != nil {
		return "", err
	}
	return body.State, nil
}

func (c *Client) call(ctx context.Context, action string, input map[string]any, output any) error {
	// Every machine action is scoped to one installation and mailbox. Setting
	// them here rather than at each call site means no action can be sent
	// without the binding Cloud checks them against.
	input["installation_id"] = c.InstallationID
	input["inbox_id"] = c.InboxID

	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	// One retry, and only after dropping a token Cloud rejected. A 401 on a
	// token this client still believed was live is the one failure a retry
	// reliably fixes; anything else would just repeat the same refusal.
	for attempt := range 2 {
		status, body, err := c.attempt(ctx, action, raw)
		if err != nil {
			return err
		}
		if status == http.StatusUnauthorized && attempt == 0 {
			c.Tokens.Forget()
			continue
		}
		if status != http.StatusOK {
			return statusError(action, status)
		}
		if output == nil {
			return nil
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		if err := decoder.Decode(output); err != nil {
			return fmt.Errorf("%w: unreadable %s response", ErrUnavailable, action)
		}
		return nil
	}
	return fmt.Errorf("%w: %s rejected the renewed token", ErrAuthority, action)
}

func (c *Client) attempt(ctx context.Context, action string, raw []byte) (int, []byte, error) {
	token, err := c.Tokens.Token(ctx)
	if err != nil {
		if errors.Is(err, ErrTokenDenied) {
			return 0, nil, fmt.Errorf("%w: %v", ErrAuthority, err)
		}
		return 0, nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	url := strings.TrimSuffix(c.BaseURL, "/") + "/v1/hybrid/" + action
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := c.httpClient().Do(request)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return response.StatusCode, body, nil
}

// statusError maps the Cloud contract onto the caller's decision: stop, back
// off, or treat the operation as already recorded. The response body is never
// included — it is attacker-influenced and would land in runtime logs.
func statusError(action string, status int) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: %s", ErrAuthority, action)
	case http.StatusConflict:
		return ErrPayloadConflict
	case http.StatusTooManyRequests:
		return fmt.Errorf("%w: %s", ErrSendLimit, action)
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrMissing, action)
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		// Cloud rejected the request as formed. Resending it unchanged cannot
		// succeed, so this is terminal for this message, not an outage.
		return fmt.Errorf("%w: %s rejected the request", ErrAuthority, action)
	default:
		return fmt.Errorf("%w: %s status %d", ErrUnavailable, action, status)
	}
}
