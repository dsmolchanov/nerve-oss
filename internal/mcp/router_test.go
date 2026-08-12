package mcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"neuralmail/internal/auth"
	"neuralmail/internal/config"
)

type authenticatorFunc func(*http.Request) (auth.Principal, error)

func (fn authenticatorFunc) AuthenticateRequest(r *http.Request) (auth.Principal, error) {
	return fn(r)
}

func TestRouterRejectsHostileOriginBeforeAuthentication(t *testing.T) {
	cfg := hostedRouterConfig()
	authCalls, handlerCalls := 0, 0
	router := NewRouter(cfg, authenticatorFunc(func(*http.Request) (auth.Principal, error) {
		authCalls++
		return auth.Principal{Kind: auth.PrincipalLegacyJWT}, nil
	}), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { handlerCalls++ }), nil)

	for _, origin := range []string{"null", "http://agent.example", "https://agent.example/", "https://agent.example.evil", "https://evilagent.example", " https://agent.example", "https://agent.example?x=1"} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, routedRequest(LegacyProtocolVersion, origin))
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("origin %q: expected 403, got %d", origin, recorder.Code)
		}
	}
	if authCalls != 0 || handlerCalls != 0 {
		t.Fatalf("hostile origin reached auth=%d handler=%d", authCalls, handlerCalls)
	}
}

func TestRouterRejectsMultipleOriginHeadersBeforeAuthentication(t *testing.T) {
	cfg := hostedRouterConfig()
	authCalls := 0
	router := NewRouter(cfg, authenticatorFunc(func(*http.Request) (auth.Principal, error) {
		authCalls++
		return auth.Principal{Kind: auth.PrincipalLegacyJWT}, nil
	}), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler reached") }), nil)
	req := routedRequest(LegacyProtocolVersion, "")
	req.Header.Add("Origin", "https://agent.example")
	req.Header.Add("Origin", "https://agent.example")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusForbidden || authCalls != 0 {
		t.Fatalf("expected pre-auth 403, got status=%d auth=%d", recorder.Code, authCalls)
	}
}

func TestRouterAllowedOriginStillRequiresAuthentication(t *testing.T) {
	cfg := hostedRouterConfig()
	handlerCalls := 0
	router := NewRouter(cfg, authenticatorFunc(func(*http.Request) (auth.Principal, error) {
		return auth.Principal{}, auth.ErrUnauthenticated
	}), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { handlerCalls++ }), nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, routedRequest(LegacyProtocolVersion, "https://agent.example"))
	if recorder.Code != http.StatusUnauthorized || handlerCalls != 0 {
		t.Fatalf("expected 401 before handler, got status=%d calls=%d", recorder.Code, handlerCalls)
	}
}

func TestRouterAbsentOriginAcceptsNativePrincipalKinds(t *testing.T) {
	for _, kind := range []auth.PrincipalKind{auth.PrincipalBootstrap, auth.PrincipalCloudAPIKey, auth.PrincipalLegacyJWT, auth.PrincipalM2MOnboarding, auth.PrincipalM2MOrg} {
		t.Run(string(kind), func(t *testing.T) {
			cfg := hostedRouterConfig()
			router := NewRouter(cfg, authenticatorFunc(func(*http.Request) (auth.Principal, error) {
				return auth.Principal{Kind: kind}, nil
			}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				principal, ok := auth.PrincipalFromContext(r.Context())
				if !ok || principal.Kind != kind {
					t.Fatalf("wrong principal: %#v, %v", principal, ok)
				}
				w.WriteHeader(http.StatusNoContent)
			}), nil)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, routedRequest(LegacyProtocolVersion, ""))
			if recorder.Code != http.StatusNoContent {
				t.Fatalf("expected native request to pass, got %d", recorder.Code)
			}
		})
	}
}

func TestRouterRejectsAbsentOriginForUnknownPrincipal(t *testing.T) {
	cfg := hostedRouterConfig()
	handlerCalls := 0
	router := NewRouter(cfg, authenticatorFunc(func(*http.Request) (auth.Principal, error) {
		return auth.Principal{Kind: auth.PrincipalKind("future")}, nil
	}), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { handlerCalls++ }), nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, routedRequest(LegacyProtocolVersion, ""))
	if recorder.Code != http.StatusForbidden || handlerCalls != 0 {
		t.Fatalf("expected rejection, got status=%d calls=%d", recorder.Code, handlerCalls)
	}
}

func TestRouterRoutesExactProtocolVersions(t *testing.T) {
	cfg := config.Default()
	cfg.Cloud.Mode = false
	legacyCalls, modernCalls := 0, 0
	router := NewRouter(cfg, nil,
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { legacyCalls++; w.WriteHeader(http.StatusNoContent) }),
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { modernCalls++; w.WriteHeader(http.StatusAccepted) }))

	for _, test := range []struct {
		version string
		status  int
	}{{LegacyProtocolVersion, http.StatusNoContent}, {ModernProtocolVersion, http.StatusAccepted}, {"", http.StatusBadRequest}, {"2027-01-01", http.StatusBadRequest}, {" " + LegacyProtocolVersion, http.StatusBadRequest}} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, routedRequest(test.version, ""))
		if recorder.Code != test.status {
			t.Fatalf("version %q: expected %d, got %d", test.version, test.status, recorder.Code)
		}
		if test.status == http.StatusBadRequest && recorder.Header().Get("MCP-Supported-Protocol-Versions") != LegacyProtocolVersion+", "+ModernProtocolVersion {
			t.Fatal("unsupported response omitted supported versions")
		}
	}
	if legacyCalls != 1 || modernCalls != 1 {
		t.Fatalf("unexpected routes: legacy=%d modern=%d", legacyCalls, modernCalls)
	}
}

func TestRouterFailsClosedWhenModernAdapterIsUnregistered(t *testing.T) {
	cfg := config.Default()
	router := NewRouter(cfg, nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, routedRequest(ModernProtocolVersion, ""))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", recorder.Code)
	}
}

func TestRouterDevelopmentOriginBypassRequiresLoopbackBinding(t *testing.T) {
	for _, test := range []struct {
		address string
		status  int
	}{{"127.0.0.1:8088", 204}, {"[::1]:8088", 204}, {"localhost:8088", 204}, {":8088", 403}, {"0.0.0.0:8088", 403}} {
		cfg := config.Default()
		cfg.HTTP.Addr = test.address
		router := NewRouter(cfg, nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }), nil)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, routedRequest(LegacyProtocolVersion, "https://unlisted.example"))
		if recorder.Code != test.status {
			t.Fatalf("address %q: expected %d, got %d", test.address, test.status, recorder.Code)
		}
	}
}

func TestRouterAuthenticatesExactlyOnce(t *testing.T) {
	cfg := hostedRouterConfig()
	calls := 0
	router := NewRouter(cfg, authenticatorFunc(func(*http.Request) (auth.Principal, error) {
		calls++
		return auth.Principal{Kind: auth.PrincipalLegacyJWT}, nil
	}), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }), nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, routedRequest(LegacyProtocolVersion, ""))
	if recorder.Code != 204 || calls != 1 {
		t.Fatalf("expected one authentication, got status=%d calls=%d", recorder.Code, calls)
	}
}

func TestLegacyAdapterRejectsHeaderBodyProtocolMismatchBeforeDispatch(t *testing.T) {
	cfg := config.Default()
	cfg.Cloud.Mode = false
	server := NewServer(cfg, nil, nil, nil)
	router := NewRouter(cfg, nil, http.HandlerFunc(server.HandleRoutedHTTP), nil)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2026-07-28"}}`))
	req.Header.Set("MCP-Protocol-Version", LegacyProtocolVersion)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("MCP-Session-Id") != "" {
		t.Fatal("mismatched initialize created a session")
	}
}

func hostedRouterConfig() config.Config {
	cfg := config.Default()
	cfg.Cloud.Mode, cfg.Dev.Mode = true, false
	cfg.MCP.AllowOrigins = []string{"https://agent.example"}
	return cfg
}

func routedRequest(version, origin string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	if version != "" {
		req.Header.Set("MCP-Protocol-Version", version)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	return req
}
