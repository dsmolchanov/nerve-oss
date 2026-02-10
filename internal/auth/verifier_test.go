package auth

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"neuralmail/internal/config"
	"neuralmail/internal/store"
)

func TestAuthenticateRequestJWT(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Issuer = "https://auth.nerve.email"
	cfg.Auth.Audience = "nerve-runtime"

	svc := &Service{
		Config: cfg,
		Now:    func() time.Time { return time.Unix(1000, 0) },
	}

	token := unsignedJWT(t, map[string]any{
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
	svc := &Service{
		Config: cfg,
		Now:    func() time.Time { return time.Unix(1000, 0) },
	}

	token := unsignedJWT(t, map[string]any{
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

func unsignedJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	headerBytes, err := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	claimsBytes, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(headerBytes) + "." + base64.RawURLEncoding.EncodeToString(claimsBytes) + "."
}
