package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"reflect"
	"testing"
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
