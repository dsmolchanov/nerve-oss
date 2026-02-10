package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jwtlib "github.com/golang-jwt/jwt/v5"

	"neuralmail/internal/auth"
	"neuralmail/internal/config"
)

const testSigningKey = "mcp-test-signing-key"

func TestHandleHTTPCloudModeRequiresCredentials(t *testing.T) {
	cfg := config.Default()
	cfg.Dev.Mode = true
	cfg.Cloud.Mode = true
	cfg.Security.TokenSigningKey = testSigningKey

	server := NewServer(cfg, nil, &auth.Service{
		Config: cfg,
		Now:    time.Now,
	}, nil)

	req := newInitializeRequest(t, "")
	recorder := httptest.NewRecorder()
	server.HandleHTTP(recorder, req)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing credentials, got %d", recorder.Code)
	}
}

func TestHandleHTTPCloudModeRejectsInsufficientScope(t *testing.T) {
	cfg := config.Default()
	cfg.Dev.Mode = true
	cfg.Cloud.Mode = true
	cfg.Security.TokenSigningKey = testSigningKey

	server := NewServer(cfg, nil, &auth.Service{
		Config: cfg,
		Now:    time.Now,
	}, nil)

	token := signedJWT(t, jwtlib.MapClaims{
		"org_id": "org-1",
		"sub":    "user-1",
		"jti":    "token-1",
		"exp":    time.Now().Add(5 * time.Minute).Unix(),
		"scope":  "nerve:email.search",
	})
	req := newInitializeRequest(t, "Bearer "+token)

	recorder := httptest.NewRecorder()
	server.HandleHTTP(recorder, req)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for insufficient scopes, got %d", recorder.Code)
	}
}

func TestHandleHTTPOssModeAllowsInitializeWithoutCloudAuth(t *testing.T) {
	cfg := config.Default()
	cfg.Dev.Mode = true
	cfg.Cloud.Mode = false

	server := NewServer(cfg, nil, nil, nil)
	req := newInitializeRequest(t, "")
	recorder := httptest.NewRecorder()
	server.HandleHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200 in OSS mode, got %d", recorder.Code)
	}
	if recorder.Header().Get("MCP-Session-Id") == "" {
		t.Fatalf("expected initialize response to include MCP-Session-Id")
	}
}

func newInitializeRequest(t *testing.T, authorization string) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{},
	})
	if err != nil {
		t.Fatalf("marshal initialize request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create initialize request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	return req
}

func signedJWT(t *testing.T, claims jwtlib.MapClaims) string {
	t.Helper()
	tok := jwtlib.NewWithClaims(jwtlib.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte(testSigningKey))
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	return signed
}
