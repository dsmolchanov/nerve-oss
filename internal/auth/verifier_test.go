package auth

import (
	"context"
	"crypto/rsa"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"neuralmail/internal/config"
	"neuralmail/internal/store"
)

const testSigningKey = "test-signing-key-for-unit-tests"

func TestNewServiceWiresConfiguredM2MVerifier(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Issuer = "https://auth.nerve.email"
	cfg.Auth.Audience = "https://api.nerve.email/mcp"
	cfg.Auth.JWKSURL = "https://auth.nerve.email/.well-known/jwks.json"
	service := NewService(cfg, nil)
	if service.M2M == nil || service.M2M.Remote == nil {
		t.Fatal("configured M2M verifier was not wired")
	}
}

func TestAuthenticateRequestJWT(t *testing.T) {
	cfg := config.Default()
	cfg.Security.TokenSigningKey = testSigningKey

	svc := &Service{
		Config: cfg,
		Now:    func() time.Time { return time.Unix(1000, 0) },
	}

	token := signedJWT(t, jwt.MapClaims{
		"iss":    "https://legacy.nerve.email",
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
	if principal.Kind != PrincipalLegacyJWT {
		t.Fatalf("expected legacy principal kind, got %q", principal.Kind)
	}
	if len(principal.Scopes) != 2 {
		t.Fatalf("expected 2 scopes, got %d", len(principal.Scopes))
	}
}

func TestM2MConfigDoesNotConstrainLegacyJWT(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Issuer = "https://auth.nerve.email"
	cfg.Auth.Audience = "https://nerve-runtime.fly.dev/mcp"
	cfg.Auth.JWKSURL = "https://auth.nerve.email/.well-known/jwks.json"
	cfg.Security.TokenSigningKey = testSigningKey
	service := NewService(cfg, nil)
	service.Now = func() time.Time { return time.Unix(1000, 0) }

	token := signedJWT(t, jwt.MapClaims{
		"exp": 2000, "nbf": 500, "org_id": "org-1", "sub": "legacy-client",
	})
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	principal, err := service.AuthenticateRequest(request)
	if err != nil {
		t.Fatalf("M2M config rejected legacy JWT: %v", err)
	}
	if principal.Kind != PrincipalLegacyJWT || principal.OrgID != "org-1" {
		t.Fatalf("unexpected legacy principal: %+v", principal)
	}
}

func TestAuthenticateRequestM2MPrincipalKinds(t *testing.T) {
	privateKey := mustRSAKey(t)
	keys, err := ParseRSAPublicJWKS(mustJWKS(t, "access-key-1", &privateKey.PublicKey, "PS256"))
	if err != nil {
		t.Fatalf("parse access-token JWKS: %v", err)
	}
	svc := &Service{
		M2M: &M2MTokenVerifier{
			Issuer: "https://auth.nerve.email", Audience: "https://api.nerve.email/mcp",
			Keys: keys, Now: func() time.Time { return time.Unix(1000, 0) },
		},
		LookupServiceToken: func(_ context.Context, tokenID string) (store.ServiceToken, error) {
			return store.ServiceToken{
				ID: tokenID, OrgID: "org-1", Actor: "oauth_client:client-1:g:2",
				Scopes: []string{"nerve:email.read"}, ExpiresAt: time.Unix(1900, 0),
			}, nil
		},
	}

	tests := []struct {
		name   string
		kind   PrincipalKind
		orgID  string
		scopes string
	}{
		{name: "onboarding", kind: PrincipalM2MOnboarding, scopes: "nerve:onboarding"},
		{name: "organization", kind: PrincipalM2MOrg, orgID: "org-1", scopes: "nerve:email.read"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			expires := int64(1300)
			if test.kind == PrincipalM2MOrg {
				expires = 1900
			}
			token := signedM2MJWT(t, privateKey, "access-key-1", jwt.SigningMethodPS256, jwt.MapClaims{
				"iss": "https://auth.nerve.email", "aud": "https://api.nerve.email/mcp",
				"exp": expires, "nbf": 1000, "iat": 1000, "jti": "token-1",
				"sub": "client-1", "client_id": "client-1", "generation": 2,
				"token_use": string(test.kind), "org_id": test.orgID, "scope": test.scopes,
			})
			req, reqErr := http.NewRequest(http.MethodPost, "/mcp", nil)
			if reqErr != nil {
				t.Fatalf("create request: %v", reqErr)
			}
			req.Header.Set("Authorization", "Bearer "+token)
			principal, authErr := svc.AuthenticateRequest(req)
			if authErr != nil {
				t.Fatalf("authenticate M2M request: %v", authErr)
			}
			if principal.Kind != test.kind || principal.ClientID != "client-1" || principal.Generation != 2 || principal.OrgID != test.orgID {
				t.Fatalf("unexpected M2M principal: %+v", principal)
			}
		})
	}
}

func TestAuthenticateRequestM2MOrgRequiresExactDurableTokenBinding(t *testing.T) {
	privateKey := mustRSAKey(t)
	keys, err := ParseRSAPublicJWKS(mustJWKS(t, "access-key-1", &privateKey.PublicKey, "PS256"))
	if err != nil {
		t.Fatal(err)
	}
	token := signedM2MJWT(t, privateKey, "access-key-1", jwt.SigningMethodPS256, jwt.MapClaims{
		"iss": "https://auth.nerve.email", "aud": "https://api.nerve.email/mcp",
		"exp": 1900, "nbf": 1000, "iat": 1000, "jti": "org-token-1",
		"sub": "client-1", "client_id": "client-1", "generation": 2,
		"token_use": "m2m_org", "org_id": "org-1", "scope": "nerve:email.read nerve:email.reply",
	})
	validRow := store.ServiceToken{
		ID: "org-token-1", OrgID: "org-1", Actor: "oauth_client:client-1:g:2",
		Scopes: []string{"nerve:email.read", "nerve:email.reply"}, ExpiresAt: time.Unix(1900, 0),
	}
	tests := []struct {
		name   string
		lookup func(context.Context, string) (store.ServiceToken, error)
	}{
		{name: "missing lookup"},
		{name: "missing row", lookup: func(context.Context, string) (store.ServiceToken, error) { return store.ServiceToken{}, sql.ErrNoRows }},
		{name: "revoked", lookup: func(context.Context, string) (store.ServiceToken, error) {
			row := validRow
			row.RevokedAt = sql.NullTime{Time: time.Unix(1001, 0), Valid: true}
			return row, nil
		}},
		{name: "organization mismatch", lookup: func(context.Context, string) (store.ServiceToken, error) {
			row := validRow
			row.OrgID = "org-2"
			return row, nil
		}},
		{name: "actor generation mismatch", lookup: func(context.Context, string) (store.ServiceToken, error) {
			row := validRow
			row.Actor = "oauth_client:client-1:g:1"
			return row, nil
		}},
		{name: "scope mismatch", lookup: func(context.Context, string) (store.ServiceToken, error) {
			row := validRow
			row.Scopes = []string{"nerve:email.read"}
			return row, nil
		}},
		{name: "expiry mismatch", lookup: func(context.Context, string) (store.ServiceToken, error) {
			row := validRow
			row.ExpiresAt = time.Unix(1899, 0)
			return row, nil
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			svc := &Service{
				M2M: &M2MTokenVerifier{
					Issuer: "https://auth.nerve.email", Audience: "https://api.nerve.email/mcp",
					Keys: keys, Now: func() time.Time { return time.Unix(1000, 0) },
				},
				LookupServiceToken: test.lookup,
			}
			req, err := http.NewRequest(http.MethodPost, "/mcp", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+token)
			if _, err := svc.AuthenticateRequest(req); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("binding error=%v, want ErrUnauthenticated", err)
			}
		})
	}

	svc := &Service{
		M2M: &M2MTokenVerifier{
			Issuer: "https://auth.nerve.email", Audience: "https://api.nerve.email/mcp",
			Keys: keys, Now: func() time.Time { return time.Unix(1000, 0) },
		},
		LookupServiceToken: func(context.Context, string) (store.ServiceToken, error) { return validRow, nil },
	}
	req, err := http.NewRequest(http.MethodPost, "/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	principal, err := svc.AuthenticateRequest(req)
	if err != nil {
		t.Fatalf("valid durable binding rejected: %v", err)
	}
	if principal.ActorID != validRow.Actor || principal.OrgID != validRow.OrgID || !equalScopes(principal.Scopes, validRow.Scopes) {
		t.Fatalf("principal not sourced from durable row: %+v", principal)
	}
}

func TestAuthenticateRequestM2MRejectsAlgorithmAndClaimConfusion(t *testing.T) {
	privateKey := mustRSAKey(t)
	keys, err := ParseRSAPublicJWKS(mustJWKS(t, "access-key-1", &privateKey.PublicKey, "PS256"))
	if err != nil {
		t.Fatalf("parse access-token JWKS: %v", err)
	}
	svc := &Service{M2M: &M2MTokenVerifier{
		Issuer: "https://auth.nerve.email", Audience: "https://api.nerve.email/mcp",
		Keys: keys, Now: func() time.Time { return time.Unix(1000, 0) },
	}}
	base := jwt.MapClaims{
		"iss": "https://auth.nerve.email", "aud": "https://api.nerve.email/mcp",
		"exp": 1300, "nbf": 1000, "iat": 1000, "jti": "token-1", "sub": "client-1",
		"client_id": "client-1", "generation": 1, "token_use": "m2m_onboarding",
		"scope": "nerve:onboarding",
	}
	tests := map[string]struct {
		method jwt.SigningMethod
		kid    string
		mutate func(jwt.MapClaims)
	}{
		"RS256 access token": {method: jwt.SigningMethodRS256, kid: "access-key-1"},
		"unknown kid":        {method: jwt.SigningMethodPS256, kid: "unknown"},
		"onboarding org": {method: jwt.SigningMethodPS256, kid: "access-key-1", mutate: func(claims jwt.MapClaims) {
			claims["org_id"] = "org-1"
		}},
		"org without org": {method: jwt.SigningMethodPS256, kid: "access-key-1", mutate: func(claims jwt.MapClaims) {
			claims["token_use"] = "m2m_org"
		}},
		"legacy token_kind claim": {method: jwt.SigningMethodPS256, kid: "access-key-1", mutate: func(claims jwt.MapClaims) {
			delete(claims, "token_use")
			claims["token_kind"] = "m2m_onboarding"
		}},
		"onboarding scope expansion": {method: jwt.SigningMethodPS256, kid: "access-key-1", mutate: func(claims jwt.MapClaims) {
			claims["scope"] = "nerve:onboarding nerve:email.read"
		}},
		"onboarding lifetime expansion": {method: jwt.SigningMethodPS256, kid: "access-key-1", mutate: func(claims jwt.MapClaims) {
			claims["exp"] = 1301
		}},
		"org onboarding scope": {method: jwt.SigningMethodPS256, kid: "access-key-1", mutate: func(claims jwt.MapClaims) {
			claims["token_use"] = "m2m_org"
			claims["org_id"] = "org-1"
			claims["scope"] = "nerve:onboarding"
			claims["exp"] = 1900
		}},
		"scope array type confusion": {method: jwt.SigningMethodPS256, kid: "access-key-1", mutate: func(claims jwt.MapClaims) {
			claims["scope"] = []string{"nerve:onboarding"}
		}},
		"duplicate scope": {method: jwt.SigningMethodPS256, kid: "access-key-1", mutate: func(claims jwt.MapClaims) {
			claims["scope"] = "nerve:onboarding nerve:onboarding"
		}},
		"unknown scope": {method: jwt.SigningMethodPS256, kid: "access-key-1", mutate: func(claims jwt.MapClaims) {
			claims["scope"] = "nerve:unknown"
		}},
		"noncanonical scope order": {method: jwt.SigningMethodPS256, kid: "access-key-1", mutate: func(claims jwt.MapClaims) {
			claims["token_use"] = "m2m_org"
			claims["org_id"] = "org-1"
			claims["scope"] = "nerve:email.reply nerve:email.read"
			claims["exp"] = 1900
		}},
		"subject mismatch": {method: jwt.SigningMethodPS256, kid: "access-key-1", mutate: func(claims jwt.MapClaims) {
			claims["sub"] = "another-client"
		}},
		"missing expiration": {method: jwt.SigningMethodPS256, kid: "access-key-1", mutate: func(claims jwt.MapClaims) {
			delete(claims, "exp")
		}},
		"missing issued at": {method: jwt.SigningMethodPS256, kid: "access-key-1", mutate: func(claims jwt.MapClaims) {
			delete(claims, "iat")
		}},
		"missing not before": {method: jwt.SigningMethodPS256, kid: "access-key-1", mutate: func(claims jwt.MapClaims) {
			delete(claims, "nbf")
		}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			claims := jwt.MapClaims{}
			for key, value := range base {
				claims[key] = value
			}
			if test.mutate != nil {
				test.mutate(claims)
			}
			token := signedM2MJWT(t, privateKey, test.kid, test.method, claims)
			req, reqErr := http.NewRequest(http.MethodPost, "/mcp", nil)
			if reqErr != nil {
				t.Fatalf("create request: %v", reqErr)
			}
			req.Header.Set("Authorization", "Bearer "+token)
			if _, authErr := svc.AuthenticateRequest(req); !errors.Is(authErr, ErrUnauthenticated) {
				t.Fatalf("expected fail-closed rejection, got %v", authErr)
			}
		})
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

	if _, err := svc.AuthenticateRequest(req); !errors.Is(err, ErrUnauthenticated) || !errors.Is(err, ErrUnauthorized) {
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

func TestAuthenticateRequestLegacyBootstrap(t *testing.T) {
	cfg := config.Default()
	cfg.Security.APIKey = "bootstrap-secret"
	service := NewService(cfg, nil)

	t.Run("configured key", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set("X-API-Key", "bootstrap-secret")
		principal, err := service.AuthenticateRequest(req)
		if err != nil {
			t.Fatalf("authenticate bootstrap: %v", err)
		}
		if principal.Kind != PrincipalBootstrap || principal.AuthMethod != "bootstrap_key" || !reflect.DeepEqual(principal.Scopes, []string{"*"}) {
			t.Fatalf("unexpected bootstrap principal: %#v", principal)
		}
	})

	t.Run("wrong key", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set("X-API-Key", "wrong")
		if _, err := service.AuthenticateRequest(req); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("expected unauthenticated, got %v", err)
		}
	})
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

func signedM2MJWT(t *testing.T, key *rsa.PrivateKey, kid string, method jwt.SigningMethod, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(method, claims)
	token.Header["kid"] = kid
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("sign M2M JWT: %v", err)
	}
	return signed
}
