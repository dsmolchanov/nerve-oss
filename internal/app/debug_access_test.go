package app

import (
	"net/http"
	"net/http/httptest"
	"neuralmail/internal/auth"
	"neuralmail/internal/config"
	"testing"
)

func TestDebugAccessMatrix(t *testing.T) {
	for _, tt := range []struct {
		name  string
		cloud bool
		token string
		want  int
	}{
		{"anonymous cloud", true, "", 404}, {"authenticated cloud", true, "owner-test", 404},
		{"anonymous local", false, "", 401}, {"local owner", false, "owner-test", 204},
		{"restricted local key", false, "mailbox-test", 403}, {"invalid local key", false, "wrong", 401},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Cloud.Mode = tt.cloud
			cfg.Security.APIKey = "owner-test"
			cfg.Security.LocalAPIKeys = []config.LocalAPIKey{{Token: "mailbox-test", InboxIDs: []string{"inbox-a"}}}
			a := &App{Config: cfg}
			req := httptest.NewRequest("GET", "/debug", nil)
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}
			if tt.cloud && tt.token != "" {
				req = req.WithContext(auth.WithPrincipal(req.Context(), auth.Principal{Kind: auth.PrincipalLegacyJWT, OrgID: "org-a"}))
			}
			called := false
			rec := httptest.NewRecorder()
			a.debugAccess(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(204) })).ServeHTTP(rec, req)
			if rec.Code != tt.want || called != (tt.want == 204) {
				t.Fatalf("status=%d renderer=%v", rec.Code, called)
			}
			if tt.want != 204 {
				rec = httptest.NewRecorder()
				a.handleDebug(rec, req)
				if rec.Code != tt.want {
					t.Fatalf("direct handler=%d", rec.Code)
				}
			}
		})
	}
}
