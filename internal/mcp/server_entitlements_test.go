package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jwtlib "github.com/golang-jwt/jwt/v5"

	"neuralmail/internal/auth"
	"neuralmail/internal/config"
	"neuralmail/internal/entitlements"
)

type fakeEntitlementGate struct {
	preAuthErr error
}

func (f *fakeEntitlementGate) PreAuthorizeTool(_ context.Context, _ auth.Principal, _ string, _ string) (*entitlements.Reservation, error) {
	return nil, f.preAuthErr
}

func (f *fakeEntitlementGate) FinalizeToolExecution(_ context.Context, _ entitlements.Reservation, _ string, _ string, _ string, _ string) error {
	return nil
}

func TestQuotaErrorContract(t *testing.T) {
	resp := callToolWithEntitlementError(t, entitlements.ErrQuotaExceeded)
	if resp.Error == nil {
		t.Fatalf("expected quota error response")
	}
	if resp.Error.Code != -32040 || resp.Error.Message != "quota_exceeded" {
		t.Fatalf("unexpected quota error: %#v", resp.Error)
	}
}

func TestSubscriptionErrorContract(t *testing.T) {
	resp := callToolWithEntitlementError(t, entitlements.ErrSubscriptionInactive)
	if resp.Error == nil {
		t.Fatalf("expected subscription error response")
	}
	if resp.Error.Code != -32041 || resp.Error.Message != "subscription_inactive" {
		t.Fatalf("unexpected subscription error: %#v", resp.Error)
	}
}

func TestRateLimitErrorContract(t *testing.T) {
	resp := callToolWithEntitlementError(t, &entitlements.RateLimitError{RetryAfterSeconds: 12})
	if resp.Error == nil {
		t.Fatalf("expected rate-limit error response")
	}
	if resp.Error.Code != -32042 || resp.Error.Message != "rate_limited" {
		t.Fatalf("unexpected rate-limit error: %#v", resp.Error)
	}
	data, ok := resp.Error.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected structured error data")
	}
	if int(data["retry_after_seconds"].(float64)) != 12 {
		t.Fatalf("expected retry_after_seconds=12, got %#v", data["retry_after_seconds"])
	}
}

func callToolWithEntitlementError(t *testing.T, entitlementErr error) Response {
	t.Helper()
	cfg := config.Default()
	cfg.Dev.Mode = true
	cfg.Cloud.Mode = true
	cfg.Security.TokenSigningKey = testSigningKey

	server := NewServer(cfg, nil, &auth.Service{
		Config: cfg,
		Now:    time.Now,
	}, &fakeEntitlementGate{preAuthErr: entitlementErr})

	token := signedJWT(t, jwtlib.MapClaims{
		"org_id": "org-1",
		"sub":    "user-1",
		"jti":    "tok-1",
		"scope":  "nerve:email.read",
		"exp":    time.Now().Add(5 * time.Minute).Unix(),
	})

	initReq := rpcRequest(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{},
	}, "", token)
	initRec := httptest.NewRecorder()
	server.HandleHTTP(initRec, initReq)
	if initRec.Code != http.StatusOK {
		t.Fatalf("initialize failed status=%d body=%s", initRec.Code, initRec.Body.String())
	}
	sessionID := initRec.Header().Get("MCP-Session-Id")
	if sessionID == "" {
		t.Fatalf("missing MCP-Session-Id in initialize response")
	}

	toolReq := rpcRequest(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "list_threads",
			"arguments": map[string]any{
				"inbox_id": "inbox-1",
				"limit":    1,
			},
		},
	}, sessionID, token)
	toolRec := httptest.NewRecorder()
	server.HandleHTTP(toolRec, toolReq)
	if toolRec.Code != http.StatusOK {
		t.Fatalf("tool call failed status=%d body=%s", toolRec.Code, toolRec.Body.String())
	}

	var resp Response
	if err := json.Unmarshal(toolRec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode rpc response: %v", err)
	}
	return resp
}

func rpcRequest(t *testing.T, payload map[string]any, sessionID, token string) *http.Request {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if sessionID != "" {
		req.Header.Set("MCP-Session-Id", sessionID)
	}
	return req
}
