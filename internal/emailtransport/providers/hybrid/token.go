package hybrid

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	assertionAlgorithm = "PS256"
	assertionType      = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"

	// The authorization server refuses an assertion whose lifetime exceeds 60
	// seconds. Stay well inside that so ordinary clock skew cannot push a valid
	// assertion over the edge.
	assertionLifetime = 30 * time.Second

	// Re-mint this long before expiry. A token that expires mid-request costs a
	// retry; minting early costs one signature, so the margin is generous.
	tokenRenewalMargin = 60 * time.Second

	// An access token this short-lived is a bug or a hostile server; refuse it
	// rather than spin on the token endpoint.
	minimumTokenLifetime = 30 * time.Second
	maximumTokenLifetime = 15 * time.Minute

	maxTokenResponseBytes = 64 << 10
)

// ErrTokenDenied is a refusal the authorization server will keep making until
// something changes on Cloud: the key was retired, issuance is disabled, the
// client lost the scope. Retrying the same request cannot fix it.
var ErrTokenDenied = errors.New("hybrid token request denied")

// ErrTokenUnavailable is a transport or server-side failure. The same request
// may well succeed later.
var ErrTokenUnavailable = errors.New("hybrid token endpoint unavailable")

// TokenSource mints short-lived Cloud access tokens from the installation
// private key using OAuth client credentials with private_key_jwt.
//
// There is deliberately no refresh token. A refresh token is a second
// long-lived bearer secret that has to be stored, rotated and revoked; the
// private key already proves the same identity and never leaves this host, so
// every renewal is just another signature. Losing an access token therefore
// costs nothing beyond one round trip.
type TokenSource struct {
	HTTPClient *http.Client
	Endpoint   string
	Resource   string
	ClientID   string
	Scopes     []string
	Key        Key
	Now        func() time.Time

	mu      sync.Mutex
	cached  string
	expires time.Time
	// inflight collapses concurrent renewals. The pull loop, the outbound
	// worker and a CLI command can all want a token at once; without this they
	// would each burn an assertion against the server's per-client rate limit.
	inflight *tokenFlight
}

type tokenFlight struct {
	done  chan struct{}
	token string
	err   error
}

func (t *TokenSource) now() time.Time {
	if t.Now != nil {
		return t.Now().UTC()
	}
	return time.Now().UTC()
}

func (t *TokenSource) client() *http.Client {
	if t.HTTPClient != nil {
		return t.HTTPClient
	}
	return http.DefaultClient
}

// Token returns a valid access token, minting one when the cached token is
// missing or close enough to expiry that a caller could not finish with it.
func (t *TokenSource) Token(ctx context.Context) (string, error) {
	t.mu.Lock()
	if t.cached != "" && t.now().Add(tokenRenewalMargin).Before(t.expires) {
		token := t.cached
		t.mu.Unlock()
		return token, nil
	}
	if flight := t.inflight; flight != nil {
		t.mu.Unlock()
		select {
		case <-flight.done:
			return flight.token, flight.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	flight := &tokenFlight{done: make(chan struct{})}
	t.inflight = flight
	t.mu.Unlock()

	token, expires, err := t.mint(ctx)

	t.mu.Lock()
	if err == nil {
		t.cached, t.expires = token, expires
	}
	t.inflight = nil
	t.mu.Unlock()

	flight.token, flight.err = token, err
	close(flight.done)
	return token, err
}

// Forget drops the cached token. Callers use it after Cloud rejects a token the
// client still believed was live, so the next attempt mints a fresh one instead
// of replaying the rejected bearer.
func (t *TokenSource) Forget() {
	t.mu.Lock()
	t.cached, t.expires = "", time.Time{}
	t.mu.Unlock()
}

func (t *TokenSource) mint(ctx context.Context) (string, time.Time, error) {
	now := t.now()
	assertion, err := t.Assertion(now)
	if err != nil {
		return "", time.Time{}, err
	}
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_assertion_type", assertionType)
	form.Set("client_assertion", assertion)
	form.Set("resource", t.Resource)
	form.Set("scope", strings.Join(t.Scopes, " "))
	form.Set("client_id", t.ClientID)

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, t.Endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := t.client().Do(request)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%w: %v", ErrTokenUnavailable, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxTokenResponseBytes))
		response.Body.Close()
	}()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxTokenResponseBytes))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%w: %v", ErrTokenUnavailable, err)
	}
	if response.StatusCode != http.StatusOK {
		return "", time.Time{}, t.statusError(response.StatusCode, raw)
	}
	var body struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
		Scope       string `json:"scope"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&body); err != nil {
		return "", time.Time{}, fmt.Errorf("%w: unreadable token response", ErrTokenUnavailable)
	}
	if !strings.EqualFold(body.TokenType, "Bearer") || body.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("%w: token response is not a bearer token", ErrTokenUnavailable)
	}
	lifetime := time.Duration(body.ExpiresIn) * time.Second
	if lifetime < minimumTokenLifetime || lifetime > maximumTokenLifetime {
		return "", time.Time{}, fmt.Errorf("%w: implausible token lifetime %ds", ErrTokenUnavailable, body.ExpiresIn)
	}
	// The server may narrow the grant. Continuing with fewer scopes than asked
	// for turns a clear failure here into a confusing 403 on the first send.
	if err := requireScopes(t.Scopes, body.Scope); err != nil {
		return "", time.Time{}, fmt.Errorf("%w: %v", ErrTokenDenied, err)
	}
	return body.AccessToken, now.Add(lifetime), nil
}

func (t *TokenSource) statusError(status int, raw []byte) error {
	var body struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.Unmarshal(raw, &body)
	detail := body.Error
	if detail == "" {
		detail = fmt.Sprintf("status %d", status)
	}
	// 4xx other than 408/429 means the request as formed will keep failing.
	// Everything else is worth another attempt on the next tick.
	if status >= 400 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests {
		return fmt.Errorf("%w: %s", ErrTokenDenied, detail)
	}
	return fmt.Errorf("%w: %s", ErrTokenUnavailable, detail)
}

func requireScopes(want []string, granted string) error {
	have := make(map[string]bool, len(want))
	for _, scope := range strings.Fields(granted) {
		have[scope] = true
	}
	for _, scope := range want {
		if !have[scope] {
			return fmt.Errorf("scope %q was not granted", scope)
		}
	}
	return nil
}

// assertionClaims is exactly the claim set the authorization server accepts. It
// decodes the payload with unknown members rejected, so an extra claim here —
// however harmless it looks — makes every token request fail.
type assertionClaims struct {
	Issuer   string `json:"iss"`
	Subject  string `json:"sub"`
	Audience string `json:"aud"`
	JTI      string `json:"jti"`
	IssuedAt int64  `json:"iat"`
	Expires  int64  `json:"exp"`
}

func (c assertionClaims) GetExpirationTime() (*jwt.NumericDate, error) {
	return jwt.NewNumericDate(time.Unix(c.Expires, 0)), nil
}
func (c assertionClaims) GetIssuedAt() (*jwt.NumericDate, error) {
	return jwt.NewNumericDate(time.Unix(c.IssuedAt, 0)), nil
}
func (c assertionClaims) GetNotBefore() (*jwt.NumericDate, error) { return nil, nil }
func (c assertionClaims) GetIssuer() (string, error)              { return c.Issuer, nil }
func (c assertionClaims) GetSubject() (string, error)             { return c.Subject, nil }
func (c assertionClaims) GetAudience() (jwt.ClaimStrings, error) {
	return jwt.ClaimStrings{c.Audience}, nil
}

// Assertion signs one private_key_jwt client assertion.
//
// The audience is the token endpoint this client is configured to call, not a
// fixed constant: an assertion minted for a staging or test endpoint then
// cannot be replayed against production by whoever runs that endpoint.
func (t *TokenSource) Assertion(now time.Time) (string, error) {
	private, err := ParsePrivateKeyPEM(t.Key.PrivateKeyPEM)
	if err != nil {
		return "", err
	}
	claims := assertionClaims{
		Issuer: t.ClientID, Subject: t.ClientID, Audience: t.Endpoint,
		JTI: uuid.NewString(), IssuedAt: now.Unix(), Expires: now.Add(assertionLifetime).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodPS256, claims)
	token.Header["kid"] = t.Key.KID
	return token.SignedString(private)
}
