package hybrid

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"neuralmail/internal/emailtransport"
)

// cloudServer is a stand-in for the Cloud hybrid machine API. It enforces the
// parts of the contract a client can get wrong on its own: the bearer, the
// installation binding carried in every body, and the action path.
type cloudServer struct {
	t              *testing.T
	installationID string
	inboxID        string
	token          atomic.Value // string
	requests       atomic.Int64
	unauthorized   atomic.Int64 // reject this many calls with 401 first
	handler        func(action string, input map[string]any) (int, any)
}

func newCloudServer(t *testing.T, installationID, inboxID string) (*httptest.Server, *cloudServer) {
	t.Helper()
	state := &cloudServer{t: t, installationID: installationID, inboxID: inboxID}
	server := httptest.NewServer(http.HandlerFunc(state.serve))
	t.Cleanup(server.Close)
	return server, state
}

func (s *cloudServer) serve(w http.ResponseWriter, r *http.Request) {
	s.requests.Add(1)
	action := strings.TrimPrefix(r.URL.Path, "/v1/hybrid/")
	if action == r.URL.Path {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if bearer == "" {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	if s.unauthorized.Load() > 0 {
		s.unauthorized.Add(-1)
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	if want, ok := s.token.Load().(string); ok && want != "" && bearer != want {
		s.t.Errorf("call %s used bearer %q, want %q", action, bearer, want)
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var input map[string]any
	if err := json.Unmarshal(raw, &input); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// Cloud scopes every machine action to one installation and mailbox and
	// rejects a body without them.
	if input["installation_id"] != s.installationID || input["inbox_id"] != s.inboxID {
		s.t.Errorf("call %s omitted or mismatched the installation binding: %v", action, input)
		http.Error(w, "invalid installation", http.StatusBadRequest)
		return
	}
	status, body := http.StatusOK, any(map[string]any{})
	if s.handler != nil {
		status, body = s.handler(action, input)
	}
	if status != http.StatusOK {
		http.Error(w, http.StatusText(status), status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// validDelivery is one Cloud would actually return for this client: real
// identifiers and the client's own binding, which Poll now checks.
func validDelivery(c *Client) Delivery {
	return Delivery{
		ID: "44444444-4444-4444-8444-444444444444", OrgID: c.OrgID,
		InstallationID: c.InstallationID, InboxID: c.InboxID,
		ProviderMessageID: "provider-1", Sender: "sender@example.test",
		Recipient: "agent@example.test", Subject: "Synthetic", Body: "synthetic",
		LeaseToken: "55555555-5555-4555-8555-555555555555",
	}
}

func newTestClient(t *testing.T, cloudURL string) (*Client, *tokenServer) {
	t.Helper()
	key := testKey(t)
	tokenEndpoint, tokenState := newTokenServer(t, key)
	return &Client{
		BaseURL: cloudURL, Tokens: newTestTokenSource(tokenEndpoint.URL, key),
		InstallationID: "11111111-1111-4111-8111-111111111111",
		InboxID:        "22222222-2222-4222-8222-222222222222",
	}, tokenState
}

func TestHybridClientCarriesTheInstallationBindingOnEveryAction(t *testing.T) {
	client, _ := newTestClient(t, "")
	server, state := newCloudServer(t, client.InstallationID, client.InboxID)
	client.BaseURL = server.URL
	seen := map[string]bool{}
	state.handler = func(action string, _ map[string]any) (int, any) {
		seen[action] = true
		switch action {
		case "poll":
			return http.StatusOK, map[string]any{"delivery": validDelivery(client)}
		case "send", "receipt":
			return http.StatusOK, SendReceipt{Status: "sent", ProviderMessageID: "p1", OperationKey: "k"}
		case "status":
			return http.StatusOK, map[string]any{"installation_id": client.InstallationID, "state": "active"}
		default:
			return http.StatusOK, map[string]any{"acked": true}
		}
	}
	ctx := context.Background()
	if _, err := client.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.Ack(ctx, validDelivery(client).ID, validDelivery(client).LeaseToken); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Send(ctx, SendRequest{OperationKey: "k", Kind: "reply", To: []string{"a@example.test"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Receipt(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if state, err := client.Status(ctx); err != nil || state != "active" {
		t.Fatalf("status=%q err=%v", state, err)
	}
	for _, action := range []string{"poll", "ack", "send", "receipt", "status"} {
		if !seen[action] {
			t.Fatalf("action %s never reached cloud", action)
		}
	}
}

// An empty mailbox is the ordinary case, not a failure, and must be
// distinguishable from one so the pull loop does not log on every tick.
func TestHybridClientReportsAnEmptyMailboxAsMissing(t *testing.T) {
	client, _ := newTestClient(t, "")
	server, state := newCloudServer(t, client.InstallationID, client.InboxID)
	client.BaseURL = server.URL
	state.handler = func(string, map[string]any) (int, any) {
		return http.StatusOK, map[string]any{"delivery": nil}
	}
	if _, err := client.Poll(context.Background()); !errors.Is(err, ErrMissing) {
		t.Fatalf("empty mailbox: %v", err)
	}
}

// A 401 on a token this client still believed was live is the one failure a
// retry fixes. It must mint a new token rather than replay the rejected one,
// and it must not loop.
func TestHybridClientRenewsOnceAfterAnUnauthorizedCall(t *testing.T) {
	client, tokens := newTestClient(t, "")
	server, state := newCloudServer(t, client.InstallationID, client.InboxID)
	client.BaseURL = server.URL
	state.handler = func(string, map[string]any) (int, any) {
		return http.StatusOK, map[string]any{"delivery": validDelivery(client)}
	}
	if _, err := client.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	minted := tokens.requests.Load()

	state.unauthorized.Store(1)
	if _, err := client.Poll(context.Background()); err != nil {
		t.Fatalf("did not recover from a stale token: %v", err)
	}
	if tokens.requests.Load() != minted+1 {
		t.Fatalf("token requests %d, want exactly one renewal after the 401", tokens.requests.Load())
	}

	// A persistent 401 is authority, not a stale token, and must stop.
	state.unauthorized.Store(10)
	if _, err := client.Poll(context.Background()); !errors.Is(err, ErrAuthority) {
		t.Fatalf("persistent 401: %v", err)
	}
}

// Each Cloud status means a different decision for the caller: stop, back off,
// or treat the operation as already recorded.
func TestHybridClientMapsEveryCloudRefusal(t *testing.T) {
	cases := map[int]error{
		http.StatusForbidden:             ErrAuthority,
		http.StatusBadRequest:            ErrAuthority,
		http.StatusRequestEntityTooLarge: ErrAuthority,
		http.StatusConflict:              ErrPayloadConflict,
		http.StatusTooManyRequests:       ErrSendLimit,
		http.StatusNotFound:              ErrMissing,
		http.StatusInternalServerError:   ErrUnavailable,
		http.StatusBadGateway:            ErrUnavailable,
		http.StatusServiceUnavailable:    ErrUnavailable,
		http.StatusGatewayTimeout:        ErrUnavailable,
	}
	for status, want := range cases {
		t.Run(http.StatusText(status), func(t *testing.T) {
			client, _ := newTestClient(t, "")
			server, state := newCloudServer(t, client.InstallationID, client.InboxID)
			client.BaseURL = server.URL
			state.handler = func(string, map[string]any) (int, any) { return status, nil }
			_, err := client.Send(context.Background(), SendRequest{OperationKey: "k", Kind: "reply", To: []string{"a@example.test"}})
			if !errors.Is(err, want) {
				t.Fatalf("status %d gave %v, want %v", status, err, want)
			}
		})
	}
}

// The Cloud response body is attacker-influenced and must never be pasted into
// a runtime error, where it would land in logs.
func TestHybridClientDoesNotEchoCloudResponseBodies(t *testing.T) {
	client, _ := newTestClient(t, "")
	server, state := newCloudServer(t, client.InstallationID, client.InboxID)
	client.BaseURL = server.URL
	state.handler = func(string, map[string]any) (int, any) { return http.StatusForbidden, nil }
	_, err := client.Poll(context.Background())
	if err == nil {
		t.Fatal("expected refusal")
	}
	if strings.Contains(err.Error(), http.StatusText(http.StatusForbidden)) {
		t.Fatalf("error echoes the response body: %v", err)
	}
}

// A token the authorization server will keep refusing is an authority failure
// for the caller, not a transient one, so the pull loop stops rather than
// hammering a revoked installation.
func TestHybridClientSurfacesTokenDenialAsAuthority(t *testing.T) {
	client, tokens := newTestClient(t, "")
	server, _ := newCloudServer(t, client.InstallationID, client.InboxID)
	client.BaseURL = server.URL
	tokens.status = http.StatusUnauthorized
	tokens.body = `{"error":"invalid_client"}`
	if _, err := client.Poll(context.Background()); !errors.Is(err, ErrAuthority) {
		t.Fatalf("revoked key: %v", err)
	}
}

// The outbox worker decides retry budget from the error type alone. An
// untyped error is classified transient, so a revoked installation or a
// malformed request would be retried until the message was quarantined as an
// ambiguous outcome instead of terminating as the known rejection it is.
func TestHybridClientClassifiesEveryRefusalForTheOutboxWorker(t *testing.T) {
	cases := map[int]struct {
		permanent bool
		reason    string
		sentinel  error
	}{
		http.StatusUnauthorized:          {true, "unauthorized", ErrAuthority},
		http.StatusForbidden:             {true, "forbidden", ErrAuthority},
		http.StatusBadRequest:            {true, "bad_request", ErrAuthority},
		http.StatusRequestEntityTooLarge: {true, "bad_request", ErrAuthority},
		http.StatusConflict:              {true, "payload_conflict", ErrPayloadConflict},
		http.StatusNotFound:              {true, "not_found", ErrMissing},
		http.StatusTooManyRequests:       {false, "rate_limited", ErrSendLimit},
		http.StatusInternalServerError:   {false, "server_error", ErrUnavailable},
		http.StatusBadGateway:            {false, "server_error", ErrUnavailable},
		http.StatusServiceUnavailable:    {false, "server_error", ErrUnavailable},
		http.StatusGatewayTimeout:        {false, "server_error", ErrUnavailable},
	}
	for status, want := range cases {
		t.Run(http.StatusText(status), func(t *testing.T) {
			client, _ := newTestClient(t, "")
			server, state := newCloudServer(t, client.InstallationID, client.InboxID)
			client.BaseURL = server.URL
			// 401 is retried once with a fresh token before it is reported,
			// so make every attempt fail the same way.
			state.handler = func(string, map[string]any) (int, any) { return status, nil }
			_, err := client.Send(context.Background(), SendRequest{
				OperationKey: "k", Kind: "reply", To: []string{"a@example.test"},
			})
			if !errors.Is(err, want.sentinel) {
				t.Fatalf("status %d gave %v, want %v", status, err, want.sentinel)
			}
			classified := emailtransport.ClassifyProviderError(err)
			if classified.Permanent != want.permanent || classified.Reason != want.reason {
				t.Fatalf("status %d classified %+v, want permanent=%v reason=%q",
					status, classified, want.permanent, want.reason)
			}
		})
	}

	// A transport fault is transient and carries no status.
	client, _ := newTestClient(t, "")
	server, _ := newCloudServer(t, client.InstallationID, client.InboxID)
	client.BaseURL = server.URL
	server.Close()
	_, err := client.Poll(context.Background())
	classified := emailtransport.ClassifyProviderError(err)
	if classified.Permanent || classified.Reason != "network_error" {
		t.Fatalf("unreachable cloud classified %+v", classified)
	}
}

// Cloud is multi-tenant. A misrouted or malformed 200 that this client
// accepted would be stored as somebody else's mail, finalize the wrong outbox
// row, or stand in as proof that a rotation succeeded.
func TestHybridClientRejectsResponsesForAnotherBinding(t *testing.T) {
	good := func(c *Client) Delivery {
		return Delivery{
			ID: "44444444-4444-4444-8444-444444444444", OrgID: c.OrgID,
			InstallationID: c.InstallationID, InboxID: c.InboxID,
			ProviderMessageID: "provider-1", Sender: "s@example.test",
			LeaseToken: "55555555-5555-4555-8555-555555555555",
		}
	}
	deliveries := map[string]func(*Delivery){
		"foreign organization": func(d *Delivery) { d.OrgID = "66666666-6666-4666-8666-666666666666" },
		"foreign installation": func(d *Delivery) { d.InstallationID = "77777777-7777-4777-8777-777777777777" },
		"foreign mailbox":      func(d *Delivery) { d.InboxID = "88888888-8888-4888-8888-888888888888" },
		"unusable id":          func(d *Delivery) { d.ID = "not-a-uuid" },
		"no lease":             func(d *Delivery) { d.LeaseToken = "" },
		"no sender":            func(d *Delivery) { d.Sender = "" },
		"no provider id":       func(d *Delivery) { d.ProviderMessageID = "" },
	}
	for name, mutate := range deliveries {
		t.Run("poll "+name, func(t *testing.T) {
			client, _ := newTestClient(t, "")
			client.OrgID = "33333333-3333-4333-8333-333333333333"
			server, state := newCloudServer(t, client.InstallationID, client.InboxID)
			client.BaseURL = server.URL
			delivery := good(client)
			mutate(&delivery)
			state.handler = func(string, map[string]any) (int, any) {
				return http.StatusOK, map[string]any{"delivery": delivery}
			}
			if _, err := client.Poll(context.Background()); err == nil {
				t.Fatal("accepted a delivery that is not this installation's")
			}
		})
	}

	receipts := map[string]SendReceipt{
		"foreign organization": {OrgID: "66666666-6666-4666-8666-666666666666", Status: "sent", OperationKey: "op-1", ProviderMessageID: "p"},
		"foreign installation": {InstallationID: "77777777-7777-4777-8777-777777777777", Status: "sent", OperationKey: "op-1", ProviderMessageID: "p"},
		"foreign operation":    {Status: "sent", OperationKey: "someone-elses", ProviderMessageID: "p"},
		"no status":            {OperationKey: "op-1"},
	}
	for name, receipt := range receipts {
		for _, action := range []string{"send", "receipt"} {
			t.Run(action+" "+name, func(t *testing.T) {
				client, _ := newTestClient(t, "")
				client.OrgID = "33333333-3333-4333-8333-333333333333"
				server, state := newCloudServer(t, client.InstallationID, client.InboxID)
				client.BaseURL = server.URL
				state.handler = func(string, map[string]any) (int, any) { return http.StatusOK, receipt }
				var err error
				if action == "send" {
					_, err = client.Send(context.Background(), SendRequest{
						OperationKey: "op-1", Kind: "reply", To: []string{"a@example.test"}})
				} else {
					_, err = client.Receipt(context.Background(), "op-1")
				}
				if err == nil {
					t.Fatal("accepted a receipt that does not answer this operation")
				}
			})
		}
	}

	// Rotation treats a successful status as proof Cloud accepts the new key
	// for this installation, so an empty or foreign answer must not pass.
	for name, body := range map[string]map[string]any{
		"foreign installation": {"installation_id": "77777777-7777-4777-8777-777777777777", "state": "active"},
		"no installation":      {"state": "active"},
		"no state":             {"installation_id": "11111111-1111-4111-8111-111111111111"},
	} {
		t.Run("status "+name, func(t *testing.T) {
			client, _ := newTestClient(t, "")
			server, state := newCloudServer(t, client.InstallationID, client.InboxID)
			client.BaseURL = server.URL
			state.handler = func(string, map[string]any) (int, any) { return http.StatusOK, body }
			if _, err := client.Status(context.Background()); err == nil {
				t.Fatal("accepted a status that does not describe this installation")
			}
		})
	}
}
