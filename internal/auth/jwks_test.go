package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestParseRSAPublicJWKSAndThumbprint(t *testing.T) {
	privateKey := mustRSAKey(t)
	document := mustJWKS(t, "access-key-1", &privateKey.PublicKey, "PS256")

	keys, err := ParseRSAPublicJWKS(document)
	if err != nil {
		t.Fatalf("parse JWKS: %v", err)
	}
	if !reflect.DeepEqual(keys.Kids(), []string{"access-key-1"}) {
		t.Fatalf("unexpected kids: %#v", keys.Kids())
	}
	parsed, ok := keys.Lookup("access-key-1")
	if !ok || parsed.N.Cmp(privateKey.N) != 0 || parsed.E != privateKey.E {
		t.Fatal("parsed public key does not match source key")
	}
	first, err := RSAJWKThumbprint(parsed)
	if err != nil {
		t.Fatalf("derive thumbprint: %v", err)
	}
	second, err := RSAJWKThumbprint(&privateKey.PublicKey)
	if err != nil {
		t.Fatalf("derive source thumbprint: %v", err)
	}
	if first == "" || first != second {
		t.Fatalf("thumbprint is not stable: %q != %q", first, second)
	}
}

func TestParseRSAPublicJWKSRejectsUnsafeKeys(t *testing.T) {
	privateKey := mustRSAKey(t)
	tests := map[string][]byte{
		"wrong algorithm": mustJWKS(t, "key-1", &privateKey.PublicKey, "RS256"),
		"small modulus":   mustJWKS(t, "key-1", &rsa.PublicKey{N: big.NewInt(65537), E: 65537}, "PS256"),
		"empty set":       []byte(`{"keys":[]}`),
	}
	for name, document := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseRSAPublicJWKS(document); err == nil {
				t.Fatal("expected unsafe JWKS to be rejected")
			}
		})
	}
}

func TestRemoteJWKSRefreshAndBoundedStaleUse(t *testing.T) {
	current := mustRSAKey(t)
	next := mustRSAKey(t)
	document := mustJWKS(t, "current", &current.PublicKey, "PS256")
	fail := false
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if fail {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=60, must-revalidate")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(document)
	}))
	defer server.Close()

	cache, err := NewRemoteJWKS(server.URL, server.Client())
	if err != nil {
		t.Fatalf("create remote JWKS: %v", err)
	}
	now := time.Unix(1_000, 0)
	cache.now = func() time.Time { return now }

	if _, err := cache.Lookup(context.Background(), "current"); err != nil {
		t.Fatalf("load current key: %v", err)
	}
	if _, err := cache.Lookup(context.Background(), "current"); err != nil || requests != 1 {
		t.Fatalf("fresh lookup refreshed unexpectedly: err=%v requests=%d", err, requests)
	}

	document = mustJWKSSet(t, map[string]*rsa.PublicKey{
		"current": &current.PublicKey,
		"next":    &next.PublicKey,
	})
	if _, err := cache.Lookup(context.Background(), "next"); err != nil || requests != 2 {
		t.Fatalf("unknown kid did not force one successful refresh: err=%v requests=%d", err, requests)
	}

	now = now.Add(61 * time.Second)
	fail = true
	if _, err := cache.Lookup(context.Background(), "current"); err != nil {
		t.Fatalf("known key should allow bounded stale use: %v", err)
	}
	if _, err := cache.Lookup(context.Background(), "unknown"); err == nil {
		t.Fatal("unknown key was authorized from stale cache")
	}

	now = now.Add(maxJWKSStaleUse + time.Minute)
	if _, err := cache.Lookup(context.Background(), "current"); err == nil {
		t.Fatal("known key remained authorized beyond stale bound")
	}
}

func mustRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return key
}

func mustJWKS(t *testing.T, kid string, key *rsa.PublicKey, algorithm string) []byte {
	t.Helper()
	document := rsaJWKS{Keys: []rsaJWK{{
		KTY: "RSA", Kid: kid, Use: "sig", Alg: algorithm,
		N:      base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		E:      base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		KeyOps: []string{"verify"},
	}}}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal JWKS: %v", err)
	}
	return raw
}

func mustJWKSSet(t *testing.T, keys map[string]*rsa.PublicKey) []byte {
	t.Helper()
	document := rsaJWKS{Keys: make([]rsaJWK, 0, len(keys))}
	for kid, key := range keys {
		document.Keys = append(document.Keys, rsaJWK{
			KTY: "RSA", Kid: kid, Use: "sig", Alg: "PS256",
			N:      base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			E:      base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
			KeyOps: []string{"verify"},
		})
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal JWKS set: %v", err)
	}
	return raw
}
