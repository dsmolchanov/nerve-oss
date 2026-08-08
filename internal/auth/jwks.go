package auth

import (
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
)

const maxAccessTokenJWKSKeys = 32

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
