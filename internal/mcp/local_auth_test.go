package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"neuralmail/internal/config"
	"neuralmail/internal/localauth"
	"strings"
	"testing"
)

func TestBothAdaptersReceiveOnlyAuthenticatedLocalIdentity(t *testing.T) {
	cfg := config.Default()
	cfg.Security.APIKey = "admin-test"
	cfg.Security.LocalAPIKeys = []config.LocalAPIKey{{Token: "mailbox-test", InboxIDs: []string{"inbox-a"}}}
	for _, version := range []string{LegacyProtocolVersion, ModernProtocolVersion} {
		for _, token := range []string{"", "wrong", "admin-test", "mailbox-test"} {
			called := false
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				_, restricted := localauth.FromContext(r.Context())
				if restricted != (token == "mailbox-test") {
					t.Error("lost or leaked local identity")
				}
				w.WriteHeader(204)
			})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{}`))
			req.Header.Set("MCP-Protocol-Version", version)
			req.Header.Set("Authorization", "Bearer "+token)
			NewRouter(cfg, nil, handler, handler).ServeHTTP(rec, req)
			want := token == "admin-test" || token == "mailbox-test"
			if called != want || (!want && rec.Code != 401) {
				t.Fatalf("%s auth=%v status=%d", version, want, rec.Code)
			}
			if !want && strings.Contains(rec.Header().Get("WWW-Authenticate"), "nerve-runtime") {
				t.Fatal("OSS advertises cloud OAuth")
			}
		}
	}
}

func TestConfiguredOAuthMetadata(t *testing.T) {
	cfg := config.Default()
	cfg.Cloud.PublicBaseURL = "https://runtime.example"
	cfg.Auth.Issuer = "https://auth.example"
	rec := httptest.NewRecorder()
	ProtectedResourceMetadataHandler(cfg).ServeHTTP(rec, httptest.NewRequest("GET", ProtectedResourceMetadataMCPPath, nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["resource"] != "https://runtime.example/mcp" || !bytes.Contains(rec.Body.Bytes(), []byte("https://auth.example")) {
		t.Fatal(rec.Body.String())
	}
	rec = httptest.NewRecorder()
	writeInvalidToken(rec, cfg)
	if !strings.Contains(rec.Header().Get("WWW-Authenticate"), "https://runtime.example/.well-known/") {
		t.Fatal(rec.Header())
	}
}
