package hybrid

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// tokenServer is a strict stand-in for the Cloud authorization server. Every
// check here mirrors one the real endpoint makes, because the two repositories
// cannot import each other and an assertion that only this package accepts is
// worth nothing. Loosening a rule here silently removes the coverage.
type tokenServer struct {
	t      *testing.T
	key    Key
	public *rsa.PublicKey
	// clock is shared with the client under test. Real clocks advance
	// together; a test that moves only the client's would fail the server's
	// window check for a reason that cannot happen in production.
	clock      *testClock
	scope      string
	lifetime   int64
	status     int
	body       string
	requests   atomic.Int64
	seenJTI    sync.Map
	lastScopes string
}

// testClock is read from the server goroutine while the test advances it.
type testClock struct{ nanos atomic.Int64 }

func newTestClock() *testClock {
	clock := &testClock{}
	clock.nanos.Store(time.Now().UTC().UnixNano())
	return clock
}
func (c *testClock) Now() time.Time { return time.Unix(0, c.nanos.Load()).UTC() }
func (c *testClock) Advance(d time.Duration) {
	c.nanos.Add(int64(d))
}

func newTokenServer(t *testing.T, key Key) (*httptest.Server, *tokenServer) {
	t.Helper()
	private, err := ParsePrivateKeyPEM(key.PrivateKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	state := &tokenServer{t: t, key: key, public: &private.PublicKey, clock: newTestClock(), lifetime: 900}
	server := httptest.NewServer(http.HandlerFunc(state.serve))
	t.Cleanup(server.Close)
	return server, state
}

func (s *tokenServer) serve(w http.ResponseWriter, r *http.Request) {
	s.requests.Add(1)
	if s.status != 0 {
		w.WriteHeader(s.status)
		_, _ = io.WriteString(w, s.body)
		return
	}
	if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<10))
	if err != nil {
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}
	values, err := url.ParseQuery(string(raw))
	if err != nil {
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}
	allowed := map[string]bool{
		"grant_type": true, "client_assertion_type": true, "client_assertion": true,
		"resource": true, "scope": true, "client_id": true,
	}
	for key, entries := range values {
		if !allowed[key] || len(entries) != 1 || entries[0] == "" {
			s.t.Errorf("token request sent unsupported or duplicate parameter %q", key)
			http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
			return
		}
	}
	for _, required := range []string{"grant_type", "client_assertion_type", "client_assertion", "resource", "scope"} {
		if values.Get(required) == "" {
			s.t.Errorf("token request omitted %q", required)
			http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
			return
		}
	}
	if values.Get("grant_type") != "client_credentials" {
		http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
		return
	}
	if values.Get("client_assertion_type") != assertionType {
		http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
		return
	}
	if err := s.checkAssertion(values.Get("client_assertion"), r); err != nil {
		s.t.Errorf("assertion rejected by the Cloud contract: %v", err)
		http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
		return
	}
	s.lastScopes = values.Get("scope")
	scope := s.scope
	if scope == "" {
		scope = values.Get("scope")
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": "access-" + fmt.Sprint(s.requests.Load()),
		"token_type":   "Bearer",
		"expires_in":   s.lifetime,
		"scope":        scope,
	})
}

func (s *tokenServer) checkAssertion(raw string, r *http.Request) error {
	segments := strings.Split(raw, ".")
	if len(segments) != 3 {
		return errors.New("assertion is not a three-segment JWT")
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(segments[0])
	if err != nil {
		return err
	}
	// The server decodes the header with unknown members rejected.
	var header struct {
		Alg string `json:"alg"`
		KID string `json:"kid"`
		Typ string `json:"typ,omitempty"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(headerRaw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&header); err != nil {
		return fmt.Errorf("assertion header carries an unknown member: %w", err)
	}
	if header.Alg != "PS256" {
		return fmt.Errorf("assertion alg %q, want PS256", header.Alg)
	}
	if header.KID != s.key.KID {
		return fmt.Errorf("assertion kid %q is not the registered thumbprint", header.KID)
	}
	if header.Typ != "" && header.Typ != "JWT" {
		return fmt.Errorf("assertion typ %q", header.Typ)
	}
	payloadRaw, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil {
		return err
	}
	// And the payload likewise: an extra claim is a hard failure upstream.
	var claims struct {
		Issuer   string `json:"iss"`
		Subject  string `json:"sub"`
		Audience string `json:"aud"`
		JTI      string `json:"jti"`
		IssuedAt int64  `json:"iat"`
		Expires  int64  `json:"exp"`
	}
	decoder = json.NewDecoder(strings.NewReader(string(payloadRaw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&claims); err != nil {
		return fmt.Errorf("assertion payload carries an unknown claim: %w", err)
	}
	if claims.Issuer == "" || claims.Issuer != claims.Subject {
		return fmt.Errorf("assertion iss %q must equal sub %q and be set", claims.Issuer, claims.Subject)
	}
	want := "http://" + r.Host
	if claims.Audience != want {
		return fmt.Errorf("assertion aud %q, want the called token endpoint %q", claims.Audience, want)
	}
	if claims.JTI == "" {
		return errors.New("assertion has no jti")
	}
	if _, replay := s.seenJTI.LoadOrStore(claims.JTI, true); replay {
		return fmt.Errorf("assertion jti %q was replayed", claims.JTI)
	}
	if claims.Expires <= claims.IssuedAt || claims.Expires-claims.IssuedAt > 60 {
		return fmt.Errorf("assertion lifetime %ds must be positive and at most 60s", claims.Expires-claims.IssuedAt)
	}
	now := s.clock.Now()
	if time.Unix(claims.IssuedAt, 0).After(now.Add(30*time.Second)) || time.Unix(claims.Expires, 0).Before(now.Add(-30*time.Second)) {
		return errors.New("assertion is outside the accepted clock window")
	}
	parsed, err := jwt.Parse(raw, func(token *jwt.Token) (any, error) { return s.public, nil },
		jwt.WithValidMethods([]string{"PS256"}), jwt.WithExpirationRequired(), jwt.WithIssuedAt(),
		jwt.WithAudience(want), jwt.WithIssuer(claims.Issuer), jwt.WithLeeway(30*time.Second),
		jwt.WithTimeFunc(now.UTC))
	if err != nil || !parsed.Valid {
		return fmt.Errorf("assertion signature: %w", err)
	}
	return nil
}

func newTestTokenSource(endpoint string, key Key) *TokenSource {
	return &TokenSource{
		Endpoint: endpoint, Resource: "https://runtime.example.test/mcp",
		ClientID: "hybrid-client", Scopes: []string{"nerve:email.read", "nerve:email.reply"}, Key: key,
	}
}

func TestHybridTokenSourceMintsAcceptedAssertion(t *testing.T) {
	key := testKey(t)
	server, state := newTokenServer(t, key)
	source := newTestTokenSource(server.URL, key)
	token, err := source.Token(context.Background())
	if err != nil || token == "" {
		t.Fatalf("token=%q err=%v", token, err)
	}
	if state.lastScopes != "nerve:email.read nerve:email.reply" {
		t.Fatalf("requested scopes %q are not the canonical space-joined form", state.lastScopes)
	}
}

// Every renewal signs a fresh assertion. Reusing one would be refused as a
// replay by the server's jti ledger, which the stand-in also enforces.
func TestHybridTokenSourceNeverReplaysAnAssertion(t *testing.T) {
	key := testKey(t)
	server, _ := newTokenServer(t, key)
	source := newTestTokenSource(server.URL, key)
	for range 3 {
		if _, err := source.Token(context.Background()); err != nil {
			t.Fatal(err)
		}
		source.Forget()
	}
}

// A live token is reused rather than re-minted, and it is renewed before it
// expires so no caller is handed a token that dies mid-request. There is no
// refresh token anywhere in this path.
func TestHybridTokenSourceCachesAndRenewsBeforeExpiry(t *testing.T) {
	key := testKey(t)
	server, state := newTokenServer(t, key)
	state.lifetime = 900
	source := newTestTokenSource(server.URL, key)
	source.Now = state.clock.Now

	first, err := source.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state.requests.Load() != 1 {
		t.Fatalf("cached token was re-minted: %d requests", state.requests.Load())
	}
	// Still outside the renewal margin.
	state.clock.Advance(time.Duration(state.lifetime)*time.Second - tokenRenewalMargin - time.Second)
	if token, err := source.Token(context.Background()); err != nil || token != first {
		t.Fatalf("renewed too early: token=%q err=%v requests=%d", token, err, state.requests.Load())
	}
	// Inside it: renew while the old token is still valid.
	state.clock.Advance(2 * time.Second)
	second, err := source.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second == first || state.requests.Load() != 2 {
		t.Fatalf("did not renew inside the margin: same=%v requests=%d", second == first, state.requests.Load())
	}
}

// The pull loop, the outbound worker and a CLI command can all want a token at
// the same moment. Each one minting its own would burn the server's per-client
// rate limit for no gain.
func TestHybridTokenSourceCollapsesConcurrentRenewals(t *testing.T) {
	key := testKey(t)
	server, state := newTokenServer(t, key)
	source := newTestTokenSource(server.URL, key)
	var wait sync.WaitGroup
	tokens := make([]string, 8)
	for index := range tokens {
		wait.Add(1)
		go func() {
			defer wait.Done()
			token, err := source.Token(context.Background())
			if err != nil {
				t.Errorf("concurrent token: %v", err)
				return
			}
			tokens[index] = token
		}()
	}
	wait.Wait()
	if requests := state.requests.Load(); requests != 1 {
		t.Fatalf("%d concurrent callers made %d token requests, want 1", len(tokens), requests)
	}
	for _, token := range tokens {
		if token != tokens[0] {
			t.Fatal("concurrent callers received different tokens")
		}
	}
}

// A refusal the server will keep making is distinct from a transient one: the
// caller backs off on the first and retries the second.
func TestHybridTokenSourceSeparatesDenialFromOutage(t *testing.T) {
	cases := map[int]error{
		http.StatusBadRequest:          ErrTokenDenied,
		http.StatusUnauthorized:        ErrTokenDenied,
		http.StatusForbidden:           ErrTokenDenied,
		http.StatusNotFound:            ErrTokenDenied,
		http.StatusRequestTimeout:      ErrTokenUnavailable,
		http.StatusTooManyRequests:     ErrTokenUnavailable,
		http.StatusInternalServerError: ErrTokenUnavailable,
		http.StatusBadGateway:          ErrTokenUnavailable,
		http.StatusServiceUnavailable:  ErrTokenUnavailable,
	}
	for status, want := range cases {
		t.Run(http.StatusText(status), func(t *testing.T) {
			key := testKey(t)
			server, state := newTokenServer(t, key)
			state.status = status
			state.body = `{"error":"invalid_client","error_description":"refused"}`
			source := newTestTokenSource(server.URL, key)
			if _, err := source.Token(context.Background()); !errors.Is(err, want) {
				t.Fatalf("status %d gave %v, want %v", status, err, want)
			}
		})
	}
}

// A response that narrows the grant, or an implausible lifetime, must fail here
// rather than surface later as a confusing 403 on the first send.
func TestHybridTokenSourceRefusesUnusableGrants(t *testing.T) {
	for name, mutate := range map[string]func(*tokenServer){
		"narrowed scope":   func(s *tokenServer) { s.scope = "nerve:email.read" },
		"no scope":         func(s *tokenServer) { s.scope = "nerve:onboarding" },
		"expired on issue": func(s *tokenServer) { s.lifetime = 1 },
		"absurd lifetime":  func(s *tokenServer) { s.lifetime = 86400 },
	} {
		t.Run(name, func(t *testing.T) {
			key := testKey(t)
			server, state := newTokenServer(t, key)
			mutate(state)
			source := newTestTokenSource(server.URL, key)
			if _, err := source.Token(context.Background()); err == nil {
				t.Fatal("accepted an unusable grant")
			}
		})
	}
}

// Forget exists so a rejected bearer is never replayed: the next call mints.
func TestHybridTokenSourceForgetDropsTheCachedToken(t *testing.T) {
	key := testKey(t)
	server, state := newTokenServer(t, key)
	source := newTestTokenSource(server.URL, key)
	first, err := source.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	source.Forget()
	second, err := source.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first == second || state.requests.Load() != 2 {
		t.Fatalf("Forget did not force a new token: requests=%d", state.requests.Load())
	}
}

// The assertion is bound to the endpoint it is sent to, so whoever runs a
// staging or test token endpoint cannot replay it against production.
func TestHybridAssertionAudienceIsTheCalledEndpoint(t *testing.T) {
	key := testKey(t)
	source := newTestTokenSource("https://auth.staging.example.test/oauth/token", key)
	raw, err := source.Assertion(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	payload := strings.Split(raw, ".")[1]
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(decoded, &claims); err != nil {
		t.Fatal(err)
	}
	if claims["aud"] != "https://auth.staging.example.test/oauth/token" {
		t.Fatalf("assertion aud = %v, want the configured endpoint", claims["aud"])
	}
	if len(claims) != 6 {
		t.Fatalf("assertion carries %d claims, want exactly iss/sub/aud/jti/iat/exp: %v", len(claims), claims)
	}
}
