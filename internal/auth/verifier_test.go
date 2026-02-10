package auth

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"neuralmail/internal/config"
	"neuralmail/internal/store"
)

const testSigningKey = "test-signing-key-for-unit-tests"

func TestAuthenticateRequestJWT(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Issuer = "https://auth.nerve.email"
	cfg.Auth.Audience = "nerve-runtime"
	cfg.Security.TokenSigningKey = testSigningKey

	svc := &Service{
		Config: cfg,
		Now:    func() time.Time { return time.Unix(1000, 0) },
	}

	token := signedJWT(t, jwt.MapClaims{
		"iss":    "https://auth.nerve.email",
		"aud":    "nerve-runtime",
		"exp":    2000,
		"nbf":    500,
		"org_id": "org-1",
		"sub":    "user-1",
		"jti":    "token-1",
		"scope":  "nerve:email.read nerve:email.search",
	})

	req, err := http.NewRequest(http.MethodPost, "/mcp", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	principal, err := svc.AuthenticateRequest(req)
	if err != nil {
		t.Fatalf("authenticate request: %v", err)
	}
	if principal.OrgID != "org-1" || principal.ActorID != "user-1" || principal.TokenID != "token-1" {
		t.Fatalf("unexpected principal identity: %+v", principal)
	}
	if principal.AuthMethod != "jwt" {
		t.Fatalf("expected jwt auth method, got %s", principal.AuthMethod)
	}
	if len(principal.Scopes) != 2 {
		t.Fatalf("expected 2 scopes, got %d", len(principal.Scopes))
	}
}

func TestAuthenticateRequestJWTRequiresOrgID(t *testing.T) {
	cfg := config.Default()
	cfg.Security.TokenSigningKey = testSigningKey
	svc := &Service{
		Config: cfg,
		Now:    func() time.Time { return time.Unix(1000, 0) },
	}

	token := signedJWT(t, jwt.MapClaims{
		"exp": 2000,
		"sub": "user-1",
	})
	req, err := http.NewRequest(http.MethodPost, "/mcp", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	if _, err := svc.AuthenticateRequest(req); err == nil {
		t.Fatalf("expected missing org_id to fail authentication")
	}
}

func TestAuthenticateRequestCloudAPIKey(t *testing.T) {
	cfg := config.Default()
	svc := &Service{
		Config: cfg,
		Now:    time.Now,
		LookupCloudKey: func(ctx context.Context, keyHash string) (store.CloudAPIKey, error) {
			if keyHash == "" {
				return store.CloudAPIKey{}, sql.ErrNoRows
			}
			return store.CloudAPIKey{
				ID:     "key-1",
				OrgID:  "org-2",
				Scopes: []string{"nerve:email.read", "nerve:email.search"},
			}, nil
		},
	}

	req, err := http.NewRequest(http.MethodPost, "/mcp", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("X-Nerve-Cloud-Key", "nrv_live_test")

	principal, err := svc.AuthenticateRequest(req)
	if err != nil {
		t.Fatalf("authenticate request: %v", err)
	}
	if principal.AuthMethod != "cloud_api_key" {
		t.Fatalf("expected cloud_api_key auth method, got %s", principal.AuthMethod)
	}
	if principal.OrgID != "org-2" || principal.TokenID != "key-1" {
		t.Fatalf("unexpected cloud principal: %+v", principal)
	}
}

func TestAuthenticateRequestServiceJWTUsesStoreRecord(t *testing.T) {
	cfg := config.Default()
	cfg.Security.TokenSigningKey = testSigningKey
	svc := &Service{
		Config: cfg,
		Now:    func() time.Time { return time.Unix(1000, 0) },
		LookupServiceToken: func(ctx context.Context, tokenID string) (store.ServiceToken, error) {
			if tokenID != "svc-1" {
				return store.ServiceToken{}, sql.ErrNoRows
			}
			return store.ServiceToken{
				ID:        "svc-1",
				OrgID:     "org-from-store",
				Actor:     "svc-actor",
				Scopes:    []string{"nerve:email.read"},
				ExpiresAt: time.Unix(2000, 0),
			}, nil
		},
	}

	token := signedJWT(t, jwt.MapClaims{
		"exp":    2000,
		"org_id": "forged-org",
		"sub":    "forged-subject",
		"jti":    "svc-1",
		"scope":  "nerve:admin.billing",
	})

	req, err := http.NewRequest(http.MethodPost, "/mcp", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	principal, err := svc.AuthenticateRequest(req)
	if err != nil {
		t.Fatalf("authenticate service token request: %v", err)
	}
	if principal.OrgID != "org-from-store" || principal.ActorID != "svc-actor" || principal.TokenID != "svc-1" {
		t.Fatalf("expected principal to be sourced from store record, got %+v", principal)
	}
	if len(principal.Scopes) != 1 || principal.Scopes[0] != "nerve:email.read" {
		t.Fatalf("expected store scopes to be enforced, got %#v", principal.Scopes)
	}
}

func TestAuthenticateRequestServiceJWTRejectsRevokedToken(t *testing.T) {
	cfg := config.Default()
	cfg.Security.TokenSigningKey = testSigningKey
	svc := &Service{
		Config: cfg,
		Now:    func() time.Time { return time.Unix(1000, 0) },
		LookupServiceToken: func(ctx context.Context, tokenID string) (store.ServiceToken, error) {
			if tokenID != "svc-revoked" {
				return store.ServiceToken{}, sql.ErrNoRows
			}
			return store.ServiceToken{
				ID:        "svc-revoked",
				OrgID:     "org-1",
				Actor:     "svc-actor",
				Scopes:    []string{"nerve:email.read"},
				ExpiresAt: time.Unix(2000, 0),
				RevokedAt: sql.NullTime{Time: time.Unix(1100, 0), Valid: true},
			}, nil
		},
	}

	token := signedJWT(t, jwt.MapClaims{
		"exp":    2000,
		"org_id": "org-1",
		"sub":    "svc-actor",
		"jti":    "svc-revoked",
		"scope":  "nerve:email.read",
	})

	req, err := http.NewRequest(http.MethodPost, "/mcp", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	if _, err := svc.AuthenticateRequest(req); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected revoked service token to be unauthorized, got %v", err)
	}
}

func TestValidateScopes(t *testing.T) {
	svc := &Service{}
	principal := Principal{Scopes: []string{"nerve:email.*"}}
	if err := svc.ValidateScopes(principal, "nerve:email.send"); err != nil {
		t.Fatalf("expected wildcard scope to allow send: %v", err)
	}
	if err := svc.ValidateScopes(principal, "nerve:admin.billing"); err == nil {
		t.Fatalf("expected admin scope to be denied")
	}
}

func signedJWT(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte(testSigningKey))
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	return signed
}
