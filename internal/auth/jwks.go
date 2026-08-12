package auth

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxAccessTokenJWKSKeys  = 32
	maxAccessTokenJWKSBytes = 256 << 10
	maxJWKSFreshness        = 5 * time.Minute
	maxJWKSStaleUse         = 16 * time.Minute
	jwksRequestTimeout      = 3 * time.Second
)

// RSAPublicKeySet is an immutable, kid-addressed access-token verification set.
// Client assertion keys are registry data and must never be loaded here.
type RSAPublicKeySet struct {
	keys map[string]*rsa.PublicKey
}

type rsaJWKS struct {
	Keys []rsaJWK `json:"keys"`
}

type rsaJWK struct {
	KTY    string   `json:"kty"`
	Kid    string   `json:"kid"`
	Use    string   `json:"use,omitempty"`
	Alg    string   `json:"alg,omitempty"`
	N      string   `json:"n"`
	E      string   `json:"e"`
	KeyOps []string `json:"key_ops,omitempty"`
}

// RemoteJWKS caches the authorization server's access-token keys. A key that
// was already known may be used for a bounded stale interval when refresh
// fails. An unknown kid always causes one bounded refresh and is never served
// from stale state.
type RemoteJWKS struct {
	url    string
	client *http.Client
	now    func() time.Time

	mu         sync.Mutex
	keys       *RSAPublicKeySet
	etag       string
	freshUntil time.Time
	staleUntil time.Time
}

func NewRemoteJWKS(rawURL string, client *http.Client) (*RemoteJWKS, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, errors.New("access-token JWKS URL must be an absolute HTTPS URL without userinfo or fragment")
	}
	if client == nil {
		client = http.DefaultClient
	}
	boundedClient := *client
	boundedClient.Timeout = jwksRequestTimeout
	boundedClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return errors.New("access-token JWKS redirects are forbidden")
	}
	return &RemoteJWKS{
		url: parsed.String(), client: &boundedClient,
		now: func() time.Time { return time.Now().UTC() },
	}, nil
}

func (cache *RemoteJWKS) Lookup(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	if cache == nil || kid == "" || len(kid) > 128 {
		return nil, errors.New("invalid access-token kid")
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()

	now := cache.now()
	known, wasKnown := cache.keys.Lookup(kid)
	if wasKnown && now.Before(cache.freshUntil) {
		return known, nil
	}

	if err := cache.refreshLocked(ctx, now); err != nil {
		if wasKnown && now.Before(cache.staleUntil) {
			return known, nil
		}
		return nil, err
	}
	key, ok := cache.keys.Lookup(kid)
	if !ok {
		return nil, errors.New("access-token kid not found after JWKS refresh")
	}
	return key, nil
}

func (cache *RemoteJWKS) refreshLocked(ctx context.Context, now time.Time) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, cache.url, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	if cache.etag != "" {
		request.Header.Set("If-None-Match", cache.etag)
	}
	response, err := cache.client.Do(request)
	if err != nil {
		return fmt.Errorf("fetch access-token JWKS: %w", err)
	}
	defer response.Body.Close()

	freshFor, err := jwksMaxAge(response.Header.Get("Cache-Control"))
	if err != nil {
		return err
	}
	if response.StatusCode == http.StatusNotModified {
		if cache.keys == nil {
			return errors.New("access-token JWKS returned 304 without cached keys")
		}
		cache.setFreshness(now, freshFor)
		return nil
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("access-token JWKS returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxAccessTokenJWKSBytes+1))
	if err != nil {
		return fmt.Errorf("read access-token JWKS: %w", err)
	}
	if len(body) > maxAccessTokenJWKSBytes {
		return errors.New("access-token JWKS response is too large")
	}
	keys, err := ParseRSAPublicJWKS(body)
	if err != nil {
		return err
	}
	cache.keys = keys
	cache.etag = strings.TrimSpace(response.Header.Get("ETag"))
	cache.setFreshness(now, freshFor)
	return nil
}

func (cache *RemoteJWKS) setFreshness(now time.Time, freshFor time.Duration) {
	cache.freshUntil = now.Add(freshFor)
	cache.staleUntil = cache.freshUntil.Add(maxJWKSStaleUse)
}

func jwksMaxAge(value string) (time.Duration, error) {
	for _, directive := range strings.Split(value, ",") {
		name, raw, found := strings.Cut(strings.TrimSpace(directive), "=")
		if !found || !strings.EqualFold(name, "max-age") {
			continue
		}
		seconds, err := strconv.ParseInt(strings.Trim(raw, `"`), 10, 64)
		if err != nil || seconds < 0 {
			return 0, errors.New("access-token JWKS has invalid Cache-Control max-age")
		}
		if seconds >= int64(maxJWKSFreshness/time.Second) {
			return maxJWKSFreshness, nil
		}
		return time.Duration(seconds) * time.Second, nil
	}
	return 0, errors.New("access-token JWKS requires Cache-Control max-age")
}

func ParseRSAPublicJWKS(raw []byte) (*RSAPublicKeySet, error) {
	var document rsaJWKS
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("decode JWKS: %w", err)
	}
	if len(document.Keys) == 0 || len(document.Keys) > maxAccessTokenJWKSKeys {
		return nil, fmt.Errorf("JWKS must contain 1-%d keys", maxAccessTokenJWKSKeys)
	}
	keys := make(map[string]*rsa.PublicKey, len(document.Keys))
	for _, jwk := range document.Keys {
		if jwk.KTY != "RSA" || jwk.Kid == "" || len(jwk.Kid) > 128 {
			return nil, errors.New("JWKS key requires kty=RSA and a 1-128 byte kid")
		}
		if jwk.Use != "" && jwk.Use != "sig" {
			return nil, fmt.Errorf("JWKS key %q is not a signing key", jwk.Kid)
		}
		if jwk.Alg != "" && jwk.Alg != "PS256" {
			return nil, fmt.Errorf("JWKS key %q advertises unsupported alg %q", jwk.Kid, jwk.Alg)
		}
		if len(jwk.KeyOps) > 0 && !containsString(jwk.KeyOps, "verify") {
			return nil, fmt.Errorf("JWKS key %q cannot verify", jwk.Kid)
		}
		if _, exists := keys[jwk.Kid]; exists {
			return nil, fmt.Errorf("duplicate JWKS kid %q", jwk.Kid)
		}
		publicKey, err := parseRSAPublicJWK(jwk)
		if err != nil {
			return nil, fmt.Errorf("JWKS key %q: %w", jwk.Kid, err)
		}
		keys[jwk.Kid] = publicKey
	}
	return &RSAPublicKeySet{keys: keys}, nil
}

func (s *RSAPublicKeySet) Lookup(kid string) (*rsa.PublicKey, bool) {
	if s == nil || kid == "" {
		return nil, false
	}
	key, ok := s.keys[kid]
	return key, ok
}

func (s *RSAPublicKeySet) Kids() []string {
	if s == nil {
		return nil
	}
	kids := make([]string, 0, len(s.keys))
	for kid := range s.keys {
		kids = append(kids, kid)
	}
	sort.Strings(kids)
	return kids
}

// RSAJWKThumbprint returns the RFC 7638 SHA-256 thumbprint used as durable key
// identity by the machine-client registry.
func RSAJWKThumbprint(key *rsa.PublicKey) (string, error) {
	if err := validateRSAPublicKey(key); err != nil {
		return "", err
	}
	n := base64.RawURLEncoding.EncodeToString(key.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())
	canonical := []byte(fmt.Sprintf(`{"e":"%s","kty":"RSA","n":"%s"}`, e, n))
	digest := sha256.Sum256(canonical)
	return base64.RawURLEncoding.EncodeToString(digest[:]), nil
}

func parseRSAPublicJWK(jwk rsaJWK) (*rsa.PublicKey, error) {
	modulus, err := base64.RawURLEncoding.DecodeString(jwk.N)
	if err != nil || len(modulus) == 0 {
		return nil, errors.New("invalid modulus")
	}
	exponentBytes, err := base64.RawURLEncoding.DecodeString(jwk.E)
	if err != nil || len(exponentBytes) == 0 || len(exponentBytes) > 4 {
		return nil, errors.New("invalid exponent")
	}
	exponentValue := new(big.Int).SetBytes(exponentBytes)
	if !exponentValue.IsInt64() {
		return nil, errors.New("exponent overflows int64")
	}
	key := &rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: int(exponentValue.Int64())}
	if err := validateRSAPublicKey(key); err != nil {
		return nil, err
	}
	return key, nil
}

func validateRSAPublicKey(key *rsa.PublicKey) error {
	if key == nil || key.N == nil || key.N.Sign() <= 0 || key.N.BitLen() < 2048 {
		return errors.New("RSA modulus must be at least 2048 bits")
	}
	if key.E < 3 || key.E%2 == 0 {
		return errors.New("RSA exponent must be an odd integer of at least 3")
	}
	return nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
