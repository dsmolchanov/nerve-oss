package featureflags

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"neuralmail/internal/auth"
	"neuralmail/internal/store"
)

type probeGate struct {
	enabled bool
	err     error
	flag    string
	orgID   string
}

func (g *probeGate) Enabled(_ context.Context, flag string, orgID string) (bool, error) {
	g.flag = flag
	g.orgID = orgID
	return g.enabled, g.err
}

func TestEffectiveStateHandlerResolvesAuthenticatedOrg(t *testing.T) {
	gate := &probeGate{enabled: true}
	handler := EffectiveStateHandler(probeAuthService([]string{"nerve:email.read"}), gate)
	request := httptest.NewRequest(http.MethodGet, EffectiveStatePathPrefix+"attachments?org_id=other-org", nil)
	request.Header.Set("X-Nerve-Cloud-Key", "nrv_live_probe")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Flag            string `json:"flag"`
		OrgID           string `json:"org_id"`
		Enabled         bool   `json:"enabled"`
		CacheTTLSeconds int    `json:"cache_ttl_seconds"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Flag != "attachments" || response.OrgID != "org-probe" ||
		!response.Enabled || response.CacheTTLSeconds != 30 {
		t.Fatalf("response=%+v", response)
	}
	if gate.flag != "attachments" || gate.orgID != "org-probe" {
		t.Fatalf("gate flag=%q org=%q", gate.flag, gate.orgID)
	}
}

func TestEffectiveStateHandlerRejectsUnsafeOrUnavailableRequests(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		scopes     []string
		key        string
		gate       Gate
		wantStatus int
	}{
		{name: "method", method: http.MethodPost, path: "attachments", scopes: []string{"nerve:email.read"}, key: "key", gate: &probeGate{}, wantStatus: http.StatusMethodNotAllowed},
		{name: "invalid flag", method: http.MethodGet, path: "Attachments", scopes: []string{"nerve:email.read"}, key: "key", gate: &probeGate{}, wantStatus: http.StatusBadRequest},
		{name: "unknown flag", method: http.MethodGet, path: "unknown_flag", scopes: []string{"nerve:email.read"}, key: "key", gate: &probeGate{}, wantStatus: http.StatusBadRequest},
		{name: "flag too long", method: http.MethodGet, path: "a" + strings.Repeat("b", maxFlagNameLength), scopes: []string{"nerve:email.read"}, key: "key", gate: &probeGate{}, wantStatus: http.StatusBadRequest},
		{name: "unauthenticated", method: http.MethodGet, path: "attachments", scopes: []string{"nerve:email.read"}, gate: &probeGate{}, wantStatus: http.StatusUnauthorized},
		{name: "missing scope", method: http.MethodGet, path: "attachments", scopes: []string{"nerve:email.send"}, key: "key", gate: &probeGate{}, wantStatus: http.StatusForbidden},
		{name: "resolver failure", method: http.MethodGet, path: "attachments", scopes: []string{"nerve:email.read"}, key: "key", gate: &probeGate{err: errors.New("lookup failed")}, wantStatus: http.StatusServiceUnavailable},
		{name: "missing gate", method: http.MethodGet, path: "attachments", scopes: []string{"nerve:email.read"}, key: "key", wantStatus: http.StatusServiceUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := EffectiveStateHandler(probeAuthService(test.scopes), test.gate)
			request := httptest.NewRequest(test.method, EffectiveStatePathPrefix+test.path, nil)
			if test.key != "" {
				request.Header.Set("X-Nerve-Cloud-Key", test.key)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status=%d body=%s, want %d", recorder.Code, recorder.Body.String(), test.wantStatus)
			}
		})
	}
}

func TestEffectiveStateHandlerRejectsUnknownFlagBeforeResolver(t *testing.T) {
	gate := &probeGate{}
	handler := EffectiveStateHandler(probeAuthService([]string{"nerve:email.read"}), gate)
	request := httptest.NewRequest(http.MethodGet, EffectiveStatePathPrefix+"unique_cache_key", nil)
	request.Header.Set("X-Nerve-Cloud-Key", "key")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if gate.flag != "" || gate.orgID != "" {
		t.Fatalf("resolver called for unknown flag: flag=%q org=%q", gate.flag, gate.orgID)
	}
}

func probeAuthService(scopes []string) *auth.Service {
	return &auth.Service{
		LookupCloudKey: func(context.Context, string) (store.CloudAPIKey, error) {
			if scopes == nil {
				return store.CloudAPIKey{}, sql.ErrNoRows
			}
			return store.CloudAPIKey{
				ID: "key-probe", OrgID: "org-probe", Scopes: scopes,
			}, nil
		},
	}
}
