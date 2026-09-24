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

	"github.com/google/uuid"

	"neuralmail/internal/emailtransport"
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

	// ErrPairingExpired means an unconsumed pairing proof reached its deadline.
	// A completed pairing is replayable even after that deadline, so Cloud only
	// returns this for a proof that is safe to replace under the same admitted
	// key.
	ErrPairingExpired = errors.New("hybrid pairing expired")
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
	// InReplyTo and References carry RFC 5322 threading. Omitted when empty,
	// so a first message in a conversation sends no empty members.
	InReplyTo  string `json:"in_reply_to,omitempty"`
	References string `json:"references,omitempty"`
}

// Client speaks the Cloud hybrid machine API for exactly one installation.
type Client struct {
	HTTPClient *http.Client
	BaseURL    string
	Tokens     *TokenSource

	// OrgID is empty until a pairing completes. Once set, every response
	// must name it: Cloud is multi-tenant, and a response for another
	// organization stored here would be somebody else's mail.
	OrgID          string
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
	if err := c.checkDelivery(*body.Delivery); err != nil {
		return Delivery{}, err
	}
	return *body.Delivery, nil
}

// checkDelivery refuses a delivery that is not this installation's.
//
// A misrouted or malformed response would otherwise be written into the local
// mailbox and acknowledged, and the deduplication key is built from the
// organization and installation the response carries — so an unchecked
// response also decides where future replays land.
func (c *Client) checkDelivery(d Delivery) error {
	if _, err := uuid.Parse(d.ID); err != nil {
		return emailtransport.NewTransientError(0, "server_error",
			fmt.Errorf("%w: poll returned a delivery without a usable id", ErrUnavailable))
	}
	if _, err := uuid.Parse(d.LeaseToken); err != nil {
		return emailtransport.NewTransientError(0, "server_error",
			fmt.Errorf("%w: poll returned delivery %s without a lease", ErrUnavailable, d.ID))
	}
	if err := c.checkBinding("poll", d.OrgID, d.InstallationID); err != nil {
		// Inbound has no outbox row to strand. A delivery that is not this
		// installation's must never be stored, however often it arrives.
		return emailtransport.NewPermanentError(0, "forbidden", err)
	}
	if d.InboxID != c.InboxID {
		return emailtransport.NewPermanentError(0, "forbidden",
			fmt.Errorf("%w: poll returned a delivery for mailbox %s", ErrAuthority, d.InboxID))
	}
	// Content-free fields are allowed to be empty; a sender is not. Storing a
	// message with no sender would produce a thread nobody can reply to.
	if strings.TrimSpace(d.Sender) == "" || strings.TrimSpace(d.ProviderMessageID) == "" {
		return emailtransport.NewTransientError(0, "server_error",
			fmt.Errorf("%w: poll returned an incomplete delivery %s", ErrUnavailable, d.ID))
	}
	return nil
}

// checkBinding requires a response to name this tenant and installation.
//
// Absence is not acceptance. A check that only rejected a *different* value
// would pass any response that simply omitted the field, which is the easy
// case for a broken or hostile peer to produce: an empty org_id would then be
// stored as this tenant's mail, and an empty installation_id would let a
// receipt finalize an outbox row it does not describe.
// The result is deliberately unclassified. What an unverifiable response means
// depends on what the caller was doing: a delivery that is not ours is an
// authority failure, while a send whose receipt cannot be verified is an
// unknown outcome, because Cloud may already have delivered the message.
func (c *Client) checkBinding(action, orgID, installationID string) error {
	// The client itself must know what to compare against. An installed
	// runtime always does; anything else cannot verify and must not guess.
	if c.OrgID == "" || c.InstallationID == "" {
		return fmt.Errorf("%w: %s cannot be verified without an installation binding", ErrAuthority, action)
	}
	if orgID != c.OrgID {
		return fmt.Errorf("%w: %s did not answer for this organization", ErrAuthority, action)
	}
	if installationID != c.InstallationID {
		return fmt.Errorf("%w: %s did not answer for this installation", ErrAuthority, action)
	}
	return nil
}

// checkReceipt refuses a receipt that does not answer the operation asked
// about. A foreign "sent" receipt would finalize the wrong outbox row.
func (c *Client) checkReceipt(action, operationKey string, receipt SendReceipt) error {
	if err := c.checkBinding(action, receipt.OrgID, receipt.InstallationID); err != nil {
		return err
	}
	// Likewise exact: a receipt with no operation key does not answer the
	// operation this call asked about.
	if receipt.OperationKey != operationKey {
		return fmt.Errorf("%w: %s did not answer for this operation", ErrAuthority, action)
	}
	if receipt.Status == "" {
		return fmt.Errorf("%w: %s returned a receipt with no status", ErrUnavailable, action)
	}
	return nil
}

// unverifiedOutcome is how Send and Receipt report a receipt they could not
// verify.
//
// Cloud has already been asked to send, and this response is the only thing
// that would say whether it did. Terminating the outbox row here would report
// failure for a message the recipient may well have received, and would stop
// the stable operation key from ever being replayed to read the authoritative
// receipt back. So the row stays unresolved and retryable.
func unverifiedOutcome(cause error) error {
	return emailtransport.NewTransientError(0, "outcome_unknown", cause)
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
	// Only when set: Cloud treats these as optional, and sending empty members
	// would change the payload a first message is recorded under.
	if request.InReplyTo != "" {
		input["in_reply_to"] = request.InReplyTo
	}
	if request.References != "" {
		input["references"] = request.References
	}
	if err := c.call(ctx, "send", input, &receipt); err != nil {
		return SendReceipt{}, err
	}
	if err := c.checkReceipt("send", request.OperationKey, receipt); err != nil {
		return SendReceipt{}, unverifiedOutcome(err)
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
	if err := c.checkReceipt("receipt", operationKey, receipt); err != nil {
		return SendReceipt{}, unverifiedOutcome(err)
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
	// Rotation treats a successful status as proof that Cloud accepts the
	// replacement key for this installation, so an empty or foreign answer
	// must not be allowed to stand in for that. The status body names no
	// organization, so this compares the installation exactly.
	if c.InstallationID == "" || body.InstallationID != c.InstallationID {
		return "", emailtransport.NewPermanentError(0, "forbidden",
			fmt.Errorf("%w: status did not answer for this installation", ErrAuthority))
	}
	if body.State == "" {
		return "", emailtransport.NewTransientError(0, "server_error",
			fmt.Errorf("%w: status returned no installation state", ErrUnavailable))
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
			return emailtransport.NewTransientError(0, "server_error",
				fmt.Errorf("%w: unreadable %s response", ErrUnavailable, action))
		}
		return nil
	}
	return emailtransport.NewPermanentError(http.StatusUnauthorized, "unauthorized",
		fmt.Errorf("%w: %s rejected the renewed token", ErrAuthority, action))
}

func (c *Client) attempt(ctx context.Context, action string, raw []byte) (int, []byte, error) {
	token, err := c.Tokens.Token(ctx)
	if err != nil {
		// A denial the authorization server will keep making is terminal for
		// this message; anything else is worth another attempt.
		if errors.Is(err, ErrTokenDenied) {
			return 0, nil, emailtransport.NewPermanentError(0, "unauthorized",
				fmt.Errorf("%w: %v", ErrAuthority, err))
		}
		return 0, nil, emailtransport.NewTransientError(0, "network_error",
			fmt.Errorf("%w: %v", ErrUnavailable, err))
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
		return 0, nil, emailtransport.NewTransientError(0, "network_error",
			fmt.Errorf("%w: %s", ErrUnavailable, action))
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return 0, nil, emailtransport.NewTransientError(0, "network_error",
			fmt.Errorf("%w: %s response body", ErrUnavailable, action))
	}
	return response.StatusCode, body, nil
}

// statusError maps the Cloud contract onto the caller's decision: stop, back
// off, or treat the operation as already recorded.
//
// Every result is a *emailtransport.ProviderError, because the outbox worker
// decides retry budget from that type alone. An untyped error is classified
// transient, so a revoked installation or a malformed request would be
// retried until the message was quarantined as an ambiguous outcome instead
// of terminating as the known rejection it is.
//
// The response body is never included: it is attacker-influenced and these
// errors reach runtime logs.
func statusError(action string, status int) error {
	permanent := func(reason string, sentinel error) error {
		return emailtransport.NewPermanentError(status, reason,
			fmt.Errorf("%w: hybrid %s", sentinel, action))
	}
	transient := func(reason string, sentinel error) error {
		return emailtransport.NewTransientError(status, reason,
			fmt.Errorf("%w: hybrid %s", sentinel, action))
	}
	switch status {
	case http.StatusUnauthorized:
		return permanent("unauthorized", ErrAuthority)
	case http.StatusForbidden:
		return permanent("forbidden", ErrAuthority)
	case http.StatusConflict:
		// This operation key is already bound to different content. Sending
		// it again cannot change that, and must not overwrite what Cloud
		// accepted first.
		return permanent("payload_conflict", ErrPayloadConflict)
	case http.StatusTooManyRequests:
		return transient("rate_limited", ErrSendLimit)
	case http.StatusNotFound:
		return permanent("not_found", ErrMissing)
	case http.StatusGone:
		return permanent("pairing_expired", ErrPairingExpired)
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		// Cloud rejected the request as formed. Resending it unchanged cannot
		// succeed, so this is terminal for this message, not an outage.
		return permanent("bad_request", ErrAuthority)
	default:
		return transient("server_error", ErrUnavailable)
	}
}
