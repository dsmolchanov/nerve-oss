package hybrid

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// pairingCloud is a stand-in Cloud that reproduces the two-person pairing
// flow: the operator admits the key, then the owner approves the pairing.
// Until both have happened Cloud refuses, and the runtime must keep waiting
// rather than declaring itself connected.
type pairingCloud struct {
	t              *testing.T
	admittedKID    atomic.Value // string
	approved       atomic.Bool
	installationID string
	orgID          string
	pairingID      string
	secret         string
	rotatedTo      atomic.Value // string
	server         *httptest.Server
	tokens         *httptest.Server
}

func (c *pairingCloud) admit(key Key)  { c.admittedKID.Store(key.KID) }
func (c *pairingCloud) rotate(key Key) { c.rotatedTo.Store(key.KID) }
func (c *pairingCloud) liveKID() string {
	if rotated, ok := c.rotatedTo.Load().(string); ok && rotated != "" {
		return rotated
	}
	admitted, _ := c.admittedKID.Load().(string)
	return admitted
}

func newPairingCloud(t *testing.T) *pairingCloud {
	t.Helper()
	cloud := &pairingCloud{
		t: t, installationID: uuid.NewString(), orgID: uuid.NewString(),
		pairingID: uuid.NewString(), secret: strings.Repeat("x", 43),
	}
	// The token endpoint mints only for a key Cloud has admitted, and after a
	// rotation only for the key the installation was rotated onto.
	cloud.tokens = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 32<<10))
		values, err := url.ParseQuery(string(raw))
		if err != nil {
			http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
			return
		}
		kid, err := assertionKID(values.Get("client_assertion"))
		if err != nil || kid == "" || kid != cloud.liveKID() {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "access-for-" + kid, "token_type": "Bearer",
			"expires_in": 900, "scope": values.Get("scope"),
		})
	}))
	t.Cleanup(cloud.tokens.Close)

	cloud.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := strings.TrimPrefix(r.URL.Path, "/v1/hybrid/")
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer access-for-") {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		var input map[string]any
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		_ = json.Unmarshal(raw, &input)
		switch action {
		case "begin":
			if input["installation_id"] != "" {
				c := input["installation_id"]
				cloud.t.Errorf("begin carried installation id %v", c)
				http.Error(w, "invalid installation", http.StatusBadRequest)
				return
			}
			writeJSON(w, map[string]any{
				"pairing": map[string]any{"id": cloud.pairingID}, "secret": cloud.secret,
			})
		case "complete":
			if !cloud.approved.Load() {
				http.Error(w, "pairing unavailable", http.StatusNotFound)
				return
			}
			if input["pairing_id"] != cloud.pairingID || input["pairing_secret"] != cloud.secret {
				http.Error(w, "pairing unavailable", http.StatusNotFound)
				return
			}
			writeJSON(w, map[string]any{"id": cloud.installationID, "org_id": cloud.orgID})
		case "status":
			if input["installation_id"] != cloud.installationID {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			writeJSON(w, map[string]any{"installation_id": cloud.installationID, "state": "active"})
		default:
			writeJSON(w, map[string]any{})
		}
	}))
	t.Cleanup(cloud.server.Close)
	return cloud
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func assertionKID(raw string) (string, error) {
	segments := strings.Split(raw, ".")
	if len(segments) != 3 {
		return "", errors.New("not a JWT")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(segments[0])
	if err != nil {
		return "", err
	}
	var header struct {
		KID string `json:"kid"`
	}
	if err := json.Unmarshal(decoded, &header); err != nil {
		return "", err
	}
	return header.KID, nil
}

func (c *pairingCloud) params() ConnectParams {
	return ConnectParams{
		CloudBaseURL: c.server.URL, TokenEndpoint: c.tokens.URL,
		Resource: "https://runtime.example.test/mcp", ClientID: "hybrid-client",
		Generation: 1, InboxID: uuid.NewString(), AuthorityID: "cloud.example.test",
	}
}

func lifecycleStore(t *testing.T) Store {
	t.Helper()
	return Store{Path: filepath.Join(t.TempDir(), "installation.json")}
}

// The full pairing: the key is generated locally, an operator admits it, the
// owner approves, and only then does the runtime hold an installation.
func TestHybridConnectWaitsForAdmissionAndApproval(t *testing.T) {
	cloud := newPairingCloud(t)
	store := lifecycleStore(t)

	begun, err := Begin(store, cloud.params())
	if err != nil {
		t.Fatal(err)
	}
	if begun.Installed() {
		t.Fatal("generating a key must not make the runtime installed")
	}
	record, err := Admission(begun)
	if err != nil {
		t.Fatal(err)
	}
	if record.KeyID != begun.Key.KID || strings.Contains(string(record.PublicJWK), "PRIVATE") {
		t.Fatal("admission record is wrong or carries private material")
	}

	// Before the operator admits the key, Cloud mints no token at all.
	refused, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := Connect(refused, store, nil, io.Discard); !errors.Is(err, ErrAuthority) {
		t.Fatalf("connected with an unadmitted key: %v", err)
	}

	cloud.admit(begun.Key)
	// The key is admitted but the owner has not approved: Connect must keep
	// waiting rather than report success or give up.
	waiting, stop := context.WithTimeout(context.Background(), 3*pairingPollInterval)
	defer stop()
	if _, err := Connect(waiting, store, nil, io.Discard); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unapproved pairing: %v", err)
	}
	if state, err := store.Load(); err != nil || state.Installed() {
		t.Fatalf("an unapproved pairing was recorded as installed: %+v err=%v", state, err)
	}

	cloud.approved.Store(true)
	ctx, done := context.WithTimeout(context.Background(), 30*time.Second)
	defer done()
	connected, err := Connect(ctx, store, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if connected.InstallationID != cloud.installationID || connected.OrgID != cloud.orgID || !connected.Installed() {
		t.Fatalf("connected state %+v", connected.Redacted())
	}
	// Cloud names the installation; the runtime stores what it was told.
	reloaded, err := store.Load()
	if err != nil || reloaded.InstallationID != cloud.installationID {
		t.Fatalf("installation was not persisted: %+v err=%v", reloaded.Redacted(), err)
	}
}

// Re-running Begin must not mint a second key: the operator has already gone
// through Cloud's protected inventory to admit the first one.
func TestHybridBeginKeepsAnAlreadyAdmittedKey(t *testing.T) {
	cloud := newPairingCloud(t)
	store := lifecycleStore(t)
	first, err := Begin(store, cloud.params())
	if err != nil {
		t.Fatal(err)
	}
	second, err := Begin(store, cloud.params())
	if err != nil {
		t.Fatal(err)
	}
	if second.Key.KID != first.Key.KID {
		t.Fatal("re-running begin replaced a key the operator may already have admitted")
	}
	cloud.admit(first.Key)
	cloud.approved.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := Connect(ctx, store, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	// A connected runtime must not be re-paired by accident.
	if _, err := Begin(store, cloud.params()); err == nil {
		t.Fatal("begin re-paired a connected runtime")
	}
}

func connectedStore(t *testing.T) (Store, *pairingCloud) {
	t.Helper()
	cloud := newPairingCloud(t)
	store := lifecycleStore(t)
	state, err := Begin(store, cloud.params())
	if err != nil {
		t.Fatal(err)
	}
	cloud.admit(state.Key)
	cloud.approved.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := Connect(ctx, store, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	return store, cloud
}

// Rotation prepares a replacement but must not switch to it until Cloud
// actually accepts it, or the runtime is left holding a key Cloud never
// admitted and cannot reach its own mailbox.
func TestHybridRotationSwitchesOnlyAfterCloudAcceptsTheReplacement(t *testing.T) {
	store, cloud := connectedStore(t)
	before, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, err := CommitRotation(ctx, store, nil); !errors.Is(err, ErrNoPendingKey) {
		t.Fatalf("committed without preparing: %v", err)
	}
	record, err := PrepareRotation(store)
	if err != nil {
		t.Fatal(err)
	}
	if record.KeyID == before.Key.KID {
		t.Fatal("prepared replacement is the active key")
	}
	// Re-preparing must return the same replacement, for the same reason
	// re-running begin keeps its key.
	again, err := PrepareRotation(store)
	if err != nil || again.KeyID != record.KeyID {
		t.Fatalf("re-prepare produced %q, want %q (err=%v)", again.KeyID, record.KeyID, err)
	}
	// Cloud has not admitted or rotated onto it yet.
	if _, err := CommitRotation(ctx, store, nil); err == nil {
		t.Fatal("committed a rotation cloud has not accepted")
	}
	held, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if held.Key.KID != before.Key.KID || held.PendingKey == nil {
		t.Fatal("a failed commit disturbed the active key")
	}
	// The runtime can still work throughout: the old key is untouched.
	if report, err := Status(ctx, store, nil); err != nil || report.CloudState != "active" {
		t.Fatalf("runtime lost access while a rotation was pending: %+v err=%v", report, err)
	}

	pending := *held.PendingKey
	cloud.rotate(pending)
	rotated, err := CommitRotation(ctx, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Key.KID != pending.KID || rotated.PendingKey != nil {
		t.Fatalf("rotation did not promote the replacement: %+v", rotated.Redacted())
	}
	// The superseded key is gone from disk, not merely unused.
	raw, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), before.Key.PrivateKeyPEM) {
		t.Fatal("the superseded private key is still on disk")
	}
}

func TestHybridRotationCanBeAbandoned(t *testing.T) {
	store, _ := connectedStore(t)
	if _, err := PrepareRotation(store); err != nil {
		t.Fatal(err)
	}
	if err := AbandonRotation(store); err != nil {
		t.Fatal(err)
	}
	state, err := store.Load()
	if err != nil || state.PendingKey != nil {
		t.Fatalf("abandon left a replacement: %+v err=%v", state.Redacted(), err)
	}
	if err := AbandonRotation(store); !errors.Is(err, ErrNoPendingKey) {
		t.Fatalf("abandon with nothing prepared: %v", err)
	}
}

// Status must work during a Cloud outage: that is exactly when an operator
// runs it. The unreachable half is reported, not returned as a failure.
func TestHybridStatusReportsCloudOutageWithoutLosingLocalFacts(t *testing.T) {
	store, cloud := connectedStore(t)
	ctx := context.Background()
	report, err := Status(ctx, store, nil)
	if err != nil || report.CloudState != "active" || report.CloudError != "" {
		t.Fatalf("healthy status: %+v err=%v", report, err)
	}
	cloud.server.Close()
	offline, err := Status(ctx, store, nil)
	if err != nil {
		t.Fatalf("status failed during an outage: %v", err)
	}
	if offline.CloudError == "" || offline.CloudState != "" {
		t.Fatalf("outage not reported: %+v", offline)
	}
	if offline.InstallationID != cloud.installationID || offline.KeyID == "" {
		t.Fatal("status dropped the local facts during an outage")
	}
	// Nothing a status prints may carry key material.
	raw, err := json.Marshal(offline)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "PRIVATE KEY") {
		t.Fatal("status carries private key material")
	}
}

// Disconnect removes the key. A retained key that Cloud has revoked is only a
// way to leak an identity later.
func TestHybridDisconnectRemovesThePrivateKey(t *testing.T) {
	store, _ := connectedStore(t)
	if err := Disconnect(store); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("installation state survived disconnect: %v", err)
	}
	if _, err := Status(context.Background(), store, nil); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("status after disconnect: %v", err)
	}
	// A second disconnect is not an error: a partially completed one must be
	// repeatable.
	if err := Disconnect(store); err != nil {
		t.Fatal(err)
	}
}

// A state file the runtime refuses to load must still be removable, or the
// private key is stranded on disk with no supported way to delete it.
func TestHybridDisconnectRemovesUnloadableState(t *testing.T) {
	store, _ := connectedStore(t)
	if err := os.Chmod(store.Path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); !errors.Is(err, ErrStateUnsafePermissions) {
		t.Fatalf("precondition: %v", err)
	}
	if err := Disconnect(store); err != nil {
		t.Fatalf("could not disconnect unloadable state: %v", err)
	}
	if _, err := os.Stat(store.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unloadable state survived disconnect")
	}
}

// A runtime still pairing has no identity Cloud would accept, so it must not
// contribute adapters that refuse every call.
func TestHybridRuntimeRequiresACompletedPairing(t *testing.T) {
	cloud := newPairingCloud(t)
	store := lifecycleStore(t)
	pairing, err := Begin(store, cloud.params())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewRuntime(pairing, nil); !errors.Is(err, ErrPairingIncomplete) {
		t.Fatalf("built a runtime from an incomplete pairing: %v", err)
	}
	if _, err := PrepareRotation(store); !errors.Is(err, ErrPairingIncomplete) {
		t.Fatalf("prepared a rotation before pairing: %v", err)
	}
	if _, err := CommitRotation(context.Background(), store, nil); !errors.Is(err, ErrPairingIncomplete) {
		t.Fatalf("committed a rotation before pairing: %v", err)
	}

	connected, _ := connectedStore(t)
	state, err := connected.Load()
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := NewRuntime(state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Inbound.Name() != ProviderName || runtime.Outbound.Name() != ProviderName {
		t.Fatal("adapters are not registered under the hybrid provider name")
	}
	if runtime.Inbound.AuthorityID != state.AuthorityID || runtime.Outbound.Kind != "reply" {
		t.Fatal("adapters were built without their binding")
	}
	if state, err := runtime.Outbound.Client.Status(context.Background()); err != nil || state != "active" {
		t.Fatalf("runtime client cannot reach cloud: %q %v", state, err)
	}
}
