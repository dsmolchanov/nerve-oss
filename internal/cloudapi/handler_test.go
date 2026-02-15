package cloudapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	jwtlib "github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"neuralmail/internal/auth"
	"neuralmail/internal/config"
	"neuralmail/internal/domains"
	"neuralmail/internal/store"
)

const testSigningKey = "test-signing-key-for-handler-tests"

type stubBilling struct{}

func (s *stubBilling) ProcessWebhook(_ context.Context, _ []byte, _ string) error {
	return nil
}

type stubTokenIssuer struct {
	lastScopes []string
	lastTTL    time.Duration
}

func (s *stubTokenIssuer) IssueServiceToken(_ context.Context, _ string, _ string, scopes []string, ttl time.Duration, _ bool) (IssuedToken, error) {
	s.lastScopes = scopes
	s.lastTTL = ttl
	return IssuedToken{
		Token:     "token.mock",
		TokenID:   "tok-1",
		ExpiresAt: time.Now().Add(ttl),
		Scopes:    scopes,
	}, nil
}

func TestControlPlaneAuthPermissionModel(t *testing.T) {
	withTempStore(t, func(ctx context.Context, st *store.Store) {
		cfg := config.Default()
		cfg.Cloud.Mode = true
		cfg.Security.APIKey = "bootstrap-admin"
		cfg.Security.TokenSigningKey = testSigningKey
		authSvc := &auth.Service{Config: cfg, Now: time.Now}
		tokenStub := &stubTokenIssuer{}
		handler := NewHandler(cfg, st, authSvc, &stubBilling{}, tokenStub)

		mux := http.NewServeMux()
		handler.RegisterRoutes(mux)

		readonlyToken := signedJWTForTest(t, jwtlib.MapClaims{
			"org_id": "org-x",
			"sub":    "user-x",
			"jti":    "tok-x",
			"scope":  "nerve:email.read",
			"exp":    time.Now().Add(5 * time.Minute).Unix(),
		})

		body := map[string]any{
			"org_id":      "org-x",
			"scopes":      []string{"nerve:email.read"},
			"ttl_seconds": 300,
		}
		req := jsonRequest(t, http.MethodPost, "/v1/tokens/service", body)
		req.Header.Set("Authorization", "Bearer "+readonlyToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected non-admin principal to be forbidden, got %d", rec.Code)
		}

		req = jsonRequest(t, http.MethodPost, "/v1/orgs", map[string]any{"name": "Acme"})
		req.Header.Set("X-API-Key", "bootstrap-admin")
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected bootstrap admin to create org, got %d body=%s", rec.Code, rec.Body.String())
		}
	})
}

func TestCheckoutClientReferenceIDMapping(t *testing.T) {
	withTempStore(t, func(ctx context.Context, st *store.Store) {
		_ = ctx
		cfg := config.Default()
		cfg.Security.APIKey = "bootstrap-admin"
		handler := NewHandler(cfg, st, &auth.Service{Config: cfg, Now: time.Now}, &stubBilling{}, &stubTokenIssuer{})
		mux := http.NewServeMux()
		handler.RegisterRoutes(mux)

		orgID := uuid.NewString()
		req := jsonRequest(t, http.MethodPost, "/v1/subscriptions/checkout", map[string]any{"org_id": orgID})
		req.Header.Set("X-API-Key", "bootstrap-admin")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected checkout request success, got %d body=%s", rec.Code, rec.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode checkout payload: %v", err)
		}
		if payload["client_reference_id"] != orgID {
			t.Fatalf("expected client_reference_id=%s got %#v", orgID, payload["client_reference_id"])
		}
		if !strings.Contains(payload["checkout_url"].(string), orgID) {
			t.Fatalf("expected checkout_url to include org id mapping")
		}
	})
}

func TestTokenIssuanceValidatesScopeAndTTL(t *testing.T) {
	withTempStore(t, func(ctx context.Context, st *store.Store) {
		cfg := config.Default()
		cfg.Security.APIKey = "bootstrap-admin"
		handler := NewHandler(cfg, st, &auth.Service{Config: cfg, Now: time.Now}, &stubBilling{}, &stubTokenIssuer{})
		mux := http.NewServeMux()
		handler.RegisterRoutes(mux)

		orgID, err := st.CreateOrg(ctx, "token-org")
		if err != nil {
			t.Fatalf("create org: %v", err)
		}

		req := jsonRequest(t, http.MethodPost, "/v1/tokens/service", map[string]any{
			"org_id":      orgID,
			"scopes":      []string{"invalid.scope"},
			"ttl_seconds": 300,
		})
		req.Header.Set("X-API-Key", "bootstrap-admin")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected invalid scope rejection, got %d", rec.Code)
		}

		req = jsonRequest(t, http.MethodPost, "/v1/tokens/service", map[string]any{
			"org_id":      orgID,
			"scopes":      []string{"nerve:email.read"},
			"ttl_seconds": 7200,
		})
		req.Header.Set("X-API-Key", "bootstrap-admin")
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected ttl validation rejection, got %d", rec.Code)
		}

		req = jsonRequest(t, http.MethodPost, "/v1/tokens/service", map[string]any{
			"org_id":      orgID,
			"scopes":      []string{"nerve:email.read"},
			"ttl_seconds": 300,
			"rotate":      true,
		})
		req.Header.Set("X-API-Key", "bootstrap-admin")
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected valid token issuance, got %d body=%s", rec.Code, rec.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode token response: %v", err)
		}
		if payload["token"] == "" || payload["token_id"] == "" {
			t.Fatalf("expected token issuance payload with token and token_id")
		}
	})
}

func TestCloudAPIKeysCreateListAndRevoke(t *testing.T) {
	withTempStore(t, func(ctx context.Context, st *store.Store) {
		cfg := config.Default()
		cfg.Security.APIKey = "bootstrap-admin"
		handler := NewHandler(cfg, st, &auth.Service{Config: cfg, Now: time.Now}, &stubBilling{}, &stubTokenIssuer{})
		mux := http.NewServeMux()
		handler.RegisterRoutes(mux)

		orgID, err := st.CreateOrg(ctx, "keys-org")
		if err != nil {
			t.Fatalf("create org: %v", err)
		}

		req := jsonRequest(t, http.MethodPost, "/v1/keys", map[string]any{
			"org_id": orgID,
			"scopes": []string{"nerve:admin.billing"},
		})
		req.Header.Set("X-API-Key", "bootstrap-admin")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected invalid cloud key scope rejection, got %d body=%s", rec.Code, rec.Body.String())
		}

		req = jsonRequest(t, http.MethodPost, "/v1/keys", map[string]any{
			"org_id": orgID,
			"label":  "Primary integration",
			"scopes": []string{"nerve:email.read", "nerve:email.search"},
		})
		req.Header.Set("X-API-Key", "bootstrap-admin")
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected cloud key creation success, got %d body=%s", rec.Code, rec.Body.String())
		}

		var created struct {
			ID        string    `json:"id"`
			Key       string    `json:"key"`
			KeyPrefix string    `json:"key_prefix"`
			Label     string    `json:"label"`
			Scopes    []string  `json:"scopes"`
			CreatedAt time.Time `json:"created_at"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode create key response: %v", err)
		}
		if created.ID == "" || created.Key == "" {
			t.Fatalf("expected created key id and raw key, got %+v", created)
		}
		if !strings.HasPrefix(created.Key, "nrv_live_") || !strings.HasPrefix(created.KeyPrefix, "nrv_live_") {
			t.Fatalf("expected key/key_prefix to include nrv_live_ prefix, got key=%q key_prefix=%q", created.Key, created.KeyPrefix)
		}
		if created.Label != "Primary integration" {
			t.Fatalf("expected key label to round-trip, got %q", created.Label)
		}

		keySum := sha256.Sum256([]byte(created.Key))
		stored, err := st.LookupCloudAPIKey(ctx, hex.EncodeToString(keySum[:]))
		if err != nil {
			t.Fatalf("lookup cloud key by hash: %v", err)
		}
		if stored.ID != created.ID || stored.OrgID != orgID {
			t.Fatalf("unexpected stored cloud key record: %+v", stored)
		}
		if stored.RevokedAt.Valid {
			t.Fatalf("expected fresh cloud key to be active")
		}

		listReq, err := http.NewRequest(http.MethodGet, "/v1/keys?org_id="+url.QueryEscape(orgID), nil)
		if err != nil {
			t.Fatalf("build list request: %v", err)
		}
		listReq.Header.Set("X-API-Key", "bootstrap-admin")
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, listReq)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected key list success, got %d body=%s", rec.Code, rec.Body.String())
		}

		var listed struct {
			Keys []struct {
				ID        string     `json:"id"`
				KeyPrefix string     `json:"key_prefix"`
				RevokedAt *time.Time `json:"revoked_at"`
			} `json:"keys"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
			t.Fatalf("decode key list response: %v", err)
		}
		if len(listed.Keys) != 1 || listed.Keys[0].ID != created.ID {
			t.Fatalf("expected one listed key with created id, got %+v", listed.Keys)
		}
		if listed.Keys[0].KeyPrefix == "" || listed.Keys[0].RevokedAt != nil {
			t.Fatalf("expected active key metadata in list, got %+v", listed.Keys[0])
		}

		revokeReq, err := http.NewRequest(http.MethodDelete, "/v1/keys/"+created.ID+"?org_id="+url.QueryEscape(orgID), nil)
		if err != nil {
			t.Fatalf("build revoke request: %v", err)
		}
		revokeReq.Header.Set("X-API-Key", "bootstrap-admin")
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, revokeReq)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected revoke success, got %d body=%s", rec.Code, rec.Body.String())
		}

		stored, err = st.LookupCloudAPIKey(ctx, hex.EncodeToString(keySum[:]))
		if err != nil {
			t.Fatalf("lookup cloud key after revoke: %v", err)
		}
		if !stored.RevokedAt.Valid {
			t.Fatalf("expected cloud key to be revoked after delete")
		}
	})
}

func TestOrgRuntimeConfigGetAndPut(t *testing.T) {
	withTempStore(t, func(ctx context.Context, st *store.Store) {
		cfg := config.Default()
		cfg.Security.APIKey = "bootstrap-admin"
		handler := NewHandler(cfg, st, &auth.Service{Config: cfg, Now: time.Now}, &stubBilling{}, &stubTokenIssuer{})
		mux := http.NewServeMux()
		handler.RegisterRoutes(mux)

		orgID, err := st.CreateOrg(ctx, "runtime-org")
		if err != nil {
			t.Fatalf("create org: %v", err)
		}

		getReq, err := http.NewRequest(http.MethodGet, "/v1/orgs/runtime?org_id="+url.QueryEscape(orgID), nil)
		if err != nil {
			t.Fatalf("build get runtime request: %v", err)
		}
		getReq.Header.Set("X-API-Key", "bootstrap-admin")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, getReq)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected runtime config read success, got %d body=%s", rec.Code, rec.Body.String())
		}
		var initial struct {
			OrgID       string `json:"org_id"`
			MCPEndpoint string `json:"mcp_endpoint"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &initial); err != nil {
			t.Fatalf("decode initial runtime payload: %v", err)
		}
		if initial.OrgID != orgID || initial.MCPEndpoint != "" {
			t.Fatalf("unexpected initial runtime payload: %+v", initial)
		}

		putReq := jsonRequest(t, http.MethodPut, "/v1/orgs/runtime", map[string]any{
			"org_id":       orgID,
			"mcp_endpoint": "https://nerve-runtime.fly.dev/mcp/",
		})
		putReq.Header.Set("X-API-Key", "bootstrap-admin")
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, putReq)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected runtime config update success, got %d body=%s", rec.Code, rec.Body.String())
		}
		var updated struct {
			OrgID       string `json:"org_id"`
			MCPEndpoint string `json:"mcp_endpoint"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
			t.Fatalf("decode updated runtime payload: %v", err)
		}
		if updated.MCPEndpoint != "https://nerve-runtime.fly.dev/mcp" {
			t.Fatalf("expected normalized mcp endpoint, got %q", updated.MCPEndpoint)
		}

		getReq, err = http.NewRequest(http.MethodGet, "/v1/orgs/runtime?org_id="+url.QueryEscape(orgID), nil)
		if err != nil {
			t.Fatalf("build second get runtime request: %v", err)
		}
		getReq.Header.Set("X-API-Key", "bootstrap-admin")
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, getReq)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected runtime config read success, got %d body=%s", rec.Code, rec.Body.String())
		}
		var fetched struct {
			OrgID       string `json:"org_id"`
			MCPEndpoint string `json:"mcp_endpoint"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &fetched); err != nil {
			t.Fatalf("decode fetched runtime payload: %v", err)
		}
		if fetched.MCPEndpoint != "https://nerve-runtime.fly.dev/mcp" {
			t.Fatalf("expected persisted mcp endpoint, got %q", fetched.MCPEndpoint)
		}

		clearReq := jsonRequest(t, http.MethodPut, "/v1/orgs/runtime", map[string]any{
			"org_id":       orgID,
			"mcp_endpoint": "",
		})
		clearReq.Header.Set("X-API-Key", "bootstrap-admin")
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, clearReq)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected runtime config clear success, got %d body=%s", rec.Code, rec.Body.String())
		}

		invalidReq := jsonRequest(t, http.MethodPut, "/v1/orgs/runtime", map[string]any{
			"org_id":       orgID,
			"mcp_endpoint": "ftp://bad-endpoint",
		})
		invalidReq.Header.Set("X-API-Key", "bootstrap-admin")
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, invalidReq)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected invalid endpoint rejection, got %d body=%s", rec.Code, rec.Body.String())
		}
	})
}

type stubTXTResolver struct {
	records map[string][]string
}

func (s stubTXTResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	if s.records == nil {
		s.records = map[string][]string{}
	}
	if recs, ok := s.records[name]; ok {
		return recs, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func TestOrgDomainsCreateListDNSVerifyAndDelete(t *testing.T) {
	withTempStore(t, func(ctx context.Context, st *store.Store) {
		cfg := config.Default()
		cfg.Security.APIKey = "bootstrap-admin"
		cfg.Cloud.Mode = true
		handler := NewHandler(cfg, st, &auth.Service{Config: cfg, Now: time.Now}, &stubBilling{}, &stubTokenIssuer{})
		mux := http.NewServeMux()
		handler.RegisterRoutes(mux)

		orgID, err := st.CreateOrg(ctx, "domains-org")
		if err != nil {
			t.Fatalf("create org: %v", err)
		}

		createReq := jsonRequest(t, http.MethodPost, "/v1/domains", map[string]any{
			"org_id": orgID,
			"domain": "acme.com",
		})
		createReq.Header.Set("X-API-Key", "bootstrap-admin")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, createReq)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected domain create success, got %d body=%s", rec.Code, rec.Body.String())
		}
		var created struct {
			Domain struct {
				ID                string `json:"id"`
				Domain            string `json:"domain"`
				Status            string `json:"status"`
				VerificationToken string `json:"verification_token"`
				DNSRecords        []struct {
					Type  string `json:"type"`
					Host  string `json:"host"`
					Value string `json:"value"`
				} `json:"dns_records"`
			} `json:"domain"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode domain create response: %v", err)
		}
		if created.Domain.ID == "" || created.Domain.Domain != "acme.com" {
			t.Fatalf("unexpected created domain payload: %+v", created.Domain)
		}
		if created.Domain.Status != "pending" {
			t.Fatalf("expected status 'pending', got %q", created.Domain.Status)
		}
		if created.Domain.VerificationToken == "" {
			t.Fatalf("expected non-empty verification_token")
		}
		if len(created.Domain.DNSRecords) != 1 || created.Domain.DNSRecords[0].Host != domains.OwnershipTXTLabel {
			t.Fatalf("expected one DNS record for ownership, got %+v", created.Domain.DNSRecords)
		}
		if created.Domain.DNSRecords[0].Value != created.Domain.VerificationToken {
			t.Fatalf("expected dns record value to match verification token")
		}

		listReq, err := http.NewRequest(http.MethodGet, "/v1/domains?org_id="+url.QueryEscape(orgID), nil)
		if err != nil {
			t.Fatalf("build list request: %v", err)
		}
		listReq.Header.Set("X-API-Key", "bootstrap-admin")
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, listReq)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected domain list success, got %d body=%s", rec.Code, rec.Body.String())
		}

		dnsReq, err := http.NewRequest(http.MethodGet, "/v1/domains/dns?org_id="+url.QueryEscape(orgID)+"&domain_id="+url.QueryEscape(created.Domain.ID), nil)
		if err != nil {
			t.Fatalf("build dns request: %v", err)
		}
		dnsReq.Header.Set("X-API-Key", "bootstrap-admin")
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, dnsReq)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected dns instructions success, got %d body=%s", rec.Code, rec.Body.String())
		}

		// Verify: stub DNS resolver to return the token at _nerve-verify.acme.com.
		txtName := domains.OwnershipTXTLabel + ".acme.com"
		handler.Domains = domains.NewVerifier(stubTXTResolver{
			records: map[string][]string{
				txtName: {created.Domain.VerificationToken},
			},
		})
		verifyReq := jsonRequest(t, http.MethodPost, "/v1/domains/verify", map[string]any{
			"org_id":    orgID,
			"domain_id": created.Domain.ID,
		})
		verifyReq.Header.Set("X-API-Key", "bootstrap-admin")
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, verifyReq)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected verify success, got %d body=%s", rec.Code, rec.Body.String())
		}
		var verified struct {
			Domain struct {
				Status string `json:"status"`
			} `json:"domain"`
			Checks map[string]any `json:"checks"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &verified); err != nil {
			t.Fatalf("decode verify response: %v", err)
		}
		if verified.Domain.Status != "active" {
			t.Fatalf("expected status 'active', got %q", verified.Domain.Status)
		}
		if ok, _ := verified.Checks["ownership_verified"].(bool); !ok {
			t.Fatalf("expected ownership_verified=true, got %+v", verified.Checks)
		}

		deleteReq, err := http.NewRequest(http.MethodDelete, "/v1/domains/"+created.Domain.ID+"?org_id="+url.QueryEscape(orgID), nil)
		if err != nil {
			t.Fatalf("build delete request: %v", err)
		}
		deleteReq.Header.Set("X-API-Key", "bootstrap-admin")
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, deleteReq)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected delete success, got %d body=%s", rec.Code, rec.Body.String())
		}
	})
}

func TestInboxesCreateAndList(t *testing.T) {
	withTempStore(t, func(ctx context.Context, st *store.Store) {
		cfg := config.Default()
		cfg.Security.APIKey = "bootstrap-admin"
		cfg.Cloud.Mode = true
		handler := NewHandler(cfg, st, &auth.Service{Config: cfg, Now: time.Now}, &stubBilling{}, &stubTokenIssuer{})
		mux := http.NewServeMux()
		handler.RegisterRoutes(mux)

		orgID, err := st.CreateOrg(ctx, "inboxes-org")
		if err != nil {
			t.Fatalf("create org: %v", err)
		}

		// Create domain (pending)
		createDomainReq := jsonRequest(t, http.MethodPost, "/v1/domains", map[string]any{
			"org_id": orgID,
			"domain": "acme.com",
		})
		createDomainReq.Header.Set("X-API-Key", "bootstrap-admin")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, createDomainReq)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected domain create success, got %d body=%s", rec.Code, rec.Body.String())
		}
		var createdDomain struct {
			Domain struct {
				ID                string `json:"id"`
				VerificationToken string `json:"verification_token"`
			} `json:"domain"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &createdDomain); err != nil {
			t.Fatalf("decode domain create response: %v", err)
		}

		// Creating an inbox before verification should fail.
		createInboxReq := jsonRequest(t, http.MethodPost, "/v1/inboxes", map[string]any{
			"org_id":  orgID,
			"address": "support@acme.com",
		})
		createInboxReq.Header.Set("X-API-Key", "bootstrap-admin")
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, createInboxReq)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected domain verification requirement, got %d body=%s", rec.Code, rec.Body.String())
		}

		// Verify domain.
		txtName := domains.OwnershipTXTLabel + ".acme.com"
		handler.Domains = domains.NewVerifier(stubTXTResolver{
			records: map[string][]string{
				txtName: {createdDomain.Domain.VerificationToken},
			},
		})
		verifyReq := jsonRequest(t, http.MethodPost, "/v1/domains/verify", map[string]any{
			"org_id":    orgID,
			"domain_id": createdDomain.Domain.ID,
		})
		verifyReq.Header.Set("X-API-Key", "bootstrap-admin")
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, verifyReq)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected verify success, got %d body=%s", rec.Code, rec.Body.String())
		}

		// Create inbox on the verified domain.
		createInboxReq = jsonRequest(t, http.MethodPost, "/v1/inboxes", map[string]any{
			"org_id":  orgID,
			"address": "support@acme.com",
		})
		createInboxReq.Header.Set("X-API-Key", "bootstrap-admin")
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, createInboxReq)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected inbox create success, got %d body=%s", rec.Code, rec.Body.String())
		}
		var createdInbox struct {
			Inbox struct {
				ID          string  `json:"id"`
				Address     string  `json:"address"`
				OrgDomainID *string `json:"org_domain_id"`
			} `json:"inbox"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &createdInbox); err != nil {
			t.Fatalf("decode inbox create response: %v", err)
		}
		if createdInbox.Inbox.ID == "" || createdInbox.Inbox.Address != "support@acme.com" {
			t.Fatalf("unexpected inbox payload: %+v", createdInbox.Inbox)
		}
		if createdInbox.Inbox.OrgDomainID == nil || *createdInbox.Inbox.OrgDomainID != createdDomain.Domain.ID {
			t.Fatalf("expected org_domain_id=%q got %+v", createdDomain.Domain.ID, createdInbox.Inbox.OrgDomainID)
		}

		// List inboxes
		listReq, err := http.NewRequest(http.MethodGet, "/v1/inboxes?org_id="+url.QueryEscape(orgID), nil)
		if err != nil {
			t.Fatalf("build list request: %v", err)
		}
		listReq.Header.Set("X-API-Key", "bootstrap-admin")
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, listReq)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected inbox list success, got %d body=%s", rec.Code, rec.Body.String())
		}
		var listed struct {
			Inboxes []struct {
				ID      string `json:"id"`
				Address string `json:"address"`
			} `json:"inboxes"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
			t.Fatalf("decode inbox list response: %v", err)
		}
		if len(listed.Inboxes) != 1 || listed.Inboxes[0].ID != createdInbox.Inbox.ID {
			t.Fatalf("expected one listed inbox, got %+v", listed.Inboxes)
		}
	})
}

func jsonRequest(t *testing.T, method, target string, body any) *http.Request {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequest(method, target, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

func signedJWTForTest(t *testing.T, claims jwtlib.MapClaims) string {
	t.Helper()
	tok := jwtlib.NewWithClaims(jwtlib.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte(testSigningKey))
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	return signed
}

func withTempStore(t *testing.T, run func(ctx context.Context, st *store.Store)) {
	t.Helper()

	baseDSN := os.Getenv("NM_TEST_DB_DSN")
	if baseDSN == "" {
		baseDSN = "postgres://neuralmail:neuralmail@127.0.0.1:54320/neuralmail?sslmode=disable"
	}
	adminDSN, err := dsnWithDatabase(baseDSN, "postgres")
	if err != nil {
		t.Fatalf("build admin dsn: %v", err)
	}
	adminDB, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatalf("open admin db: %v", err)
	}
	defer adminDB.Close()

	pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := adminDB.PingContext(pingCtx); err != nil {
		t.Skipf("postgres unavailable for cloudapi tests: %v", err)
	}

	dbName := "nerve_cloud_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := adminDB.ExecContext(context.Background(), fmt.Sprintf(`CREATE DATABASE %s`, dbName)); err != nil {
		t.Fatalf("create test db: %v", err)
	}
	testDSN, err := dsnWithDatabase(baseDSN, dbName)
	if err != nil {
		t.Fatalf("build test dsn: %v", err)
	}
	st, err := store.Open(testDSN)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	goose.SetDialect("postgres")
	goose.SetTableName("schema_migrations")
	if err := goose.UpContext(context.Background(), st.DB(), migrationDir(t)); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	t.Cleanup(func() {
		_, _ = adminDB.ExecContext(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, dbName)
		_, _ = adminDB.ExecContext(context.Background(), fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, dbName))
	})

	run(context.Background(), st)
}

func dsnWithDatabase(rawDSN, dbName string) (string, error) {
	parsed, err := url.Parse(rawDSN)
	if err != nil {
		return "", err
	}
	parsed.Path = "/" + dbName
	return parsed.String(), nil
}

func migrationDir(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("resolve migration dir: missing caller")
	}
	return filepath.Join(filepath.Dir(currentFile), "..", "store", "migrations")
}
