package cloudapi

import (
	"bytes"
	"context"
	"database/sql"
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
