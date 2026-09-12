package hybrid

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// pairingPollInterval is how often Connect re-offers the pairing proof while
// it waits for a person. Cloud expires a pairing after ten minutes, so this
// is slow enough to be polite and fast enough to finish promptly once the
// owner clicks approve.
const pairingPollInterval = 2 * time.Second

// runtimeScopes is what the runtime asks for, in the canonical order the
// authorization server requires. Reading is for the pull loop, replying is
// for answering mail; composing is deliberately absent so a paired runtime
// cannot start cold outreach on its own.
var runtimeScopes = []string{"nerve:email.read", "nerve:email.reply"}

// ConnectParams are the Cloud coordinates an operator supplies once. They
// describe where to connect and as whom, never what this runtime is allowed
// to do: Cloud decides that.
type ConnectParams struct {
	CloudBaseURL  string
	TokenEndpoint string
	Resource      string
	ClientID      string
	Generation    int64
	InboxID       string
	AuthorityID   string
}

// Begin generates the installation key and records the connection parameters
// without contacting Cloud.
//
// It is separate from completing the pairing because the key must be admitted
// to the protected Cloud M2M inventory by an operator first, and that is not
// something this runtime can do for itself. Re-running it on an existing
// pairing keeps the key, so an operator who has already admitted one is not
// asked to admit another.
func Begin(store Store, params ConnectParams) (State, error) {
	if existing, err := store.Load(); err == nil {
		if existing.Installed() {
			return State{}, errors.New("hybrid runtime is already connected; disconnect first")
		}
		return existing, nil
	} else if !errors.Is(err, ErrNotConnected) {
		return State{}, err
	}
	key, err := GenerateKey()
	if err != nil {
		return State{}, err
	}
	state := State{
		Version: StateVersion, Phase: PhaseConnecting,
		CloudBaseURL: params.CloudBaseURL, TokenEndpoint: params.TokenEndpoint,
		Resource: params.Resource, ClientID: params.ClientID, Generation: params.Generation,
		InboxID: params.InboxID, AuthorityID: params.AuthorityID, Key: key,
	}
	if err := store.Save(state); err != nil {
		return State{}, err
	}
	return state, nil
}

// AdmissionRecord is what an operator needs in order to admit this runtime's
// key to the Cloud M2M inventory. It carries no private material.
type AdmissionRecord struct {
	ClientID   string          `json:"client_id"`
	Generation int64           `json:"generation"`
	KeyID      string          `json:"key_id"`
	PublicJWK  json.RawMessage `json:"public_jwk"`
}

func admissionRecord(state State, key Key) (AdmissionRecord, error) {
	jwk, err := key.PublicJWK()
	if err != nil {
		return AdmissionRecord{}, err
	}
	return AdmissionRecord{
		ClientID: state.ClientID, Generation: state.Generation, KeyID: key.KID, PublicJWK: jwk,
	}, nil
}

// Admission returns the record for whichever key currently needs admitting:
// the replacement when a rotation is prepared, otherwise the active key.
func Admission(state State) (AdmissionRecord, error) {
	if state.PendingKey != nil {
		return admissionRecord(state, *state.PendingKey)
	}
	return admissionRecord(state, state.Key)
}

// Connect finishes a pairing that Begin started.
//
// It blocks until the owner approves, because the two steps in between are
// human: an operator admits the key and an owner approves the pairing in the
// dashboard. Printing a pairing proof and exiting would leave the operator
// polling by hand. The pairing proof is written to progress once and never
// stored: it is a bearer secret for the pairing, and a runtime that has
// completed has no further use for it.
func Connect(ctx context.Context, store Store, httpClient *http.Client, progress io.Writer) (State, error) {
	state, err := store.Load()
	if err != nil {
		return State{}, err
	}
	if state.Installed() {
		return state, nil
	}
	client := &Client{
		HTTPClient: httpClient, BaseURL: state.CloudBaseURL, InboxID: state.InboxID,
		Tokens: tokenSourceFor(state, state.Key, httpClient),
	}
	var pairing struct {
		Pairing struct {
			ID string `json:"id"`
		} `json:"pairing"`
		Secret string `json:"secret"`
	}
	// begin and complete are the only actions Cloud refuses an installation ID
	// on. The client sends the empty one it was built with, which is what
	// Cloud requires before a pairing has named an installation.
	if err := client.call(ctx, "begin", map[string]any{}, &pairing); err != nil {
		return State{}, err
	}
	if _, err := uuid.Parse(pairing.Pairing.ID); err != nil || pairing.Secret == "" {
		return State{}, fmt.Errorf("%w: cloud returned an unusable pairing", ErrUnavailable)
	}
	writeProgress(progress, "Pairing %s created. Ask the mailbox owner to approve it.\n", pairing.Pairing.ID)

	var installation struct {
		ID    string `json:"id"`
		OrgID string `json:"org_id"`
	}
	complete := map[string]any{"pairing_id": pairing.Pairing.ID, "pairing_secret": pairing.Secret}
	for {
		err := client.call(ctx, "complete", complete, &installation)
		if err == nil {
			break
		}
		// Cloud reports an unapproved pairing the same way it reports one that
		// expired or never existed. Keep offering the proof until the context
		// ends or Cloud refuses for a reason approval cannot fix.
		if !errors.Is(err, ErrMissing) {
			return State{}, err
		}
		select {
		case <-ctx.Done():
			return State{}, fmt.Errorf("pairing %s was not approved: %w", pairing.Pairing.ID, ctx.Err())
		case <-time.After(pairingPollInterval):
		}
	}
	if _, err := uuid.Parse(installation.ID); err != nil {
		return State{}, fmt.Errorf("%w: cloud completed without an installation", ErrUnavailable)
	}
	state.Phase = PhaseInstalled
	state.InstallationID = installation.ID
	state.OrgID = installation.OrgID
	if err := store.Save(state); err != nil {
		return State{}, err
	}
	return state, nil
}

// Report is what `status` prints. It never carries key material, a token, or
// the content of any message.
type Report struct {
	Phase          string `json:"phase"`
	CloudBaseURL   string `json:"cloud_base_url"`
	ClientID       string `json:"client_id"`
	Generation     int64  `json:"generation"`
	KeyID          string `json:"key_id"`
	PendingKeyID   string `json:"pending_key_id,omitempty"`
	OrgID          string `json:"org_id,omitempty"`
	InstallationID string `json:"installation_id,omitempty"`
	InboxID        string `json:"inbox_id"`
	// CloudState is what Cloud says about this installation right now, or the
	// reason the check could not be made. It is the only field that requires
	// the network, so a local status still works while Cloud is unreachable.
	CloudState string `json:"cloud_state,omitempty"`
	CloudError string `json:"cloud_error,omitempty"`
}

// Status reports the local installation and, when reachable, what Cloud says
// about it. An unreachable Cloud is reported in the result rather than
// returned as an error: an operator running status during an outage needs the
// local half, which is exactly when they need it most.
func Status(ctx context.Context, store Store, httpClient *http.Client) (Report, error) {
	state, err := store.Load()
	if err != nil {
		return Report{}, err
	}
	report := Report{
		Phase: state.Phase, CloudBaseURL: state.CloudBaseURL, ClientID: state.ClientID,
		Generation: state.Generation, KeyID: state.Key.KID, OrgID: state.OrgID,
		InstallationID: state.InstallationID, InboxID: state.InboxID,
	}
	if state.PendingKey != nil {
		report.PendingKeyID = state.PendingKey.KID
	}
	if !state.Installed() {
		return report, nil
	}
	if cloudState, err := newClient(state, state.Key, httpClient).Status(ctx); err != nil {
		report.CloudError = err.Error()
	} else {
		report.CloudState = cloudState
	}
	return report, nil
}

// PrepareRotation generates a replacement key and keeps it alongside the
// active one.
//
// It does not switch over. The replacement is useless until an operator
// admits it and the mailbox owner rotates the installation onto it at Cloud,
// and switching before then would leave this runtime holding a key Cloud does
// not know. Re-running returns the same replacement so an operator who has
// already admitted one is not asked to admit another.
func PrepareRotation(store Store) (AdmissionRecord, error) {
	state, err := store.Load()
	if err != nil {
		return AdmissionRecord{}, err
	}
	if !state.Installed() {
		return AdmissionRecord{}, ErrPairingIncomplete
	}
	if state.PendingKey == nil {
		key, err := GenerateKey()
		if err != nil {
			return AdmissionRecord{}, err
		}
		state.PendingKey = &key
		if err := store.Save(state); err != nil {
			return AdmissionRecord{}, err
		}
	}
	return admissionRecord(state, *state.PendingKey)
}

// CommitRotation promotes the prepared key once Cloud actually accepts it.
//
// The proof is a token minted with the replacement: nothing else shows that
// Cloud has both admitted the key and rotated the installation onto it. Until
// that succeeds the active key is left untouched, so a rotation that was
// prepared but never carried out at Cloud cannot strand the runtime without a
// usable identity.
func CommitRotation(ctx context.Context, store Store, httpClient *http.Client) (State, error) {
	state, err := store.Load()
	if err != nil {
		return State{}, err
	}
	if !state.Installed() {
		return State{}, ErrPairingIncomplete
	}
	if state.PendingKey == nil {
		return State{}, ErrNoPendingKey
	}
	replacement := *state.PendingKey
	// Reach a real installation-scoped action, not only the token endpoint:
	// a token proves the key was admitted, and status proves the installation
	// was rotated onto it.
	if _, err := newClient(state, replacement, httpClient).Status(ctx); err != nil {
		return State{}, fmt.Errorf("cloud does not yet accept the replacement key: %w", err)
	}
	state.Key = replacement
	state.PendingKey = nil
	if err := store.Save(state); err != nil {
		return State{}, err
	}
	return state, nil
}

// AbandonRotation discards a prepared replacement that was never admitted.
func AbandonRotation(store Store) error {
	state, err := store.Load()
	if err != nil {
		return err
	}
	if state.PendingKey == nil {
		return ErrNoPendingKey
	}
	state.PendingKey = nil
	return store.Save(state)
}

// Disconnect removes the local installation, including the private key.
//
// It does not revoke anything at Cloud: revocation is the mailbox owner's
// operation and is what actually stops this installation from being used.
// Deleting the key here is still worth doing, because a retained key that
// Cloud has revoked is only a way to leak an identity later; callers are
// expected to tell the operator to revoke as well.
func Disconnect(store Store) error {
	if _, err := store.Load(); err != nil && !errors.Is(err, ErrNotConnected) {
		// An unreadable or unsafe state file must still be removable: refusing
		// would leave the private key on disk with no supported way to delete
		// it.
		if !errors.Is(err, ErrStateUnsafePermissions) && !errors.Is(err, ErrStateVersion) {
			return err
		}
	}
	return store.Remove()
}

func tokenSourceFor(state State, key Key, httpClient *http.Client) *TokenSource {
	return &TokenSource{
		HTTPClient: httpClient, Endpoint: state.TokenEndpoint, Resource: state.Resource,
		ClientID: state.ClientID, Scopes: runtimeScopes, Key: key,
	}
}

func newClient(state State, key Key, httpClient *http.Client) *Client {
	return &Client{
		HTTPClient: httpClient, BaseURL: state.CloudBaseURL,
		InstallationID: state.InstallationID, InboxID: state.InboxID,
		Tokens: tokenSourceFor(state, key, httpClient),
	}
}

func writeProgress(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	_, _ = fmt.Fprintf(w, format, args...)
}

// Runtime is the pair of adapters a connected installation contributes to the
// transport registry, plus the mailbox they are bound to.
type Runtime struct {
	Inbound  *InboundAdapter
	Outbound *OutboundAdapter
	State    State
}

// NewRuntime builds the adapters for a completed installation. A runtime that
// is still pairing has no identity Cloud would accept, so it fails closed
// rather than registering adapters that refuse every call.
func NewRuntime(state State, httpClient *http.Client) (*Runtime, error) {
	if !state.Installed() {
		return nil, ErrPairingIncomplete
	}
	client := newClient(state, state.Key, httpClient)
	return &Runtime{
		Inbound:  &InboundAdapter{Client: client, AuthorityID: state.AuthorityID},
		Outbound: &OutboundAdapter{Client: client, Kind: "reply"},
		State:    state,
	}, nil
}
