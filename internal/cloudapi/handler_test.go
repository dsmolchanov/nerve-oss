package cloudapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
