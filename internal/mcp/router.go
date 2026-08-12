package mcp

import (
	"context"
	"errors"
	"net/http"

	"neuralmail/internal/auth"
	"neuralmail/internal/config"
)

const (
	LegacyProtocolVersion = "2025-11-25"
	ModernProtocolVersion = "2026-07-28"
)

type RequestAuthenticator interface {
	AuthenticateRequest(*http.Request) (auth.Principal, error)
}

// Router is the single hosted boundary shared by the frozen legacy adapter
// and the stateless modern adapter.
type Router struct {
	config config.Config
	auth   RequestAuthenticator
	legacy http.Handler
	modern http.Handler
}

type routedProtocolVersionKey struct{}

func withRoutedProtocolVersion(ctx context.Context, version string) context.Context {
	return context.WithValue(ctx, routedProtocolVersionKey{}, version)
}

func routedProtocolVersion(ctx context.Context) (string, bool) {
	version, ok := ctx.Value(routedProtocolVersionKey{}).(string)
	return version, ok
}

func NewRouter(cfg config.Config, authenticator RequestAuthenticator, legacy, modern http.Handler) *Router {
	return &Router{config: cfg, auth: authenticator, legacy: legacy, modern: modern}
}

func (router *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	origin, err := precheckOrigin(router.config, r.Header.Values("Origin"))
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	ctx := r.Context()
	var principal auth.Principal
	authenticated := false
	if router.config.Cloud.Mode {
		if router.auth == nil {
			http.Error(w, "cloud auth not configured", http.StatusInternalServerError)
			return
		}
		principal, err = router.auth.AuthenticateRequest(r)
		if err != nil {
			if errors.Is(err, auth.ErrForbidden) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		authenticated = true
		ctx = auth.WithPrincipal(ctx, principal)
	}
	if err := authorizeOrigin(router.config, origin, principal, authenticated); err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	request := r.WithContext(ctx)
	switch r.Header.Get("MCP-Protocol-Version") {
	case LegacyProtocolVersion:
		router.serveAdapter(w, request.WithContext(withRoutedProtocolVersion(request.Context(), LegacyProtocolVersion)), router.legacy)
	case ModernProtocolVersion:
		router.serveAdapter(w, request.WithContext(withRoutedProtocolVersion(request.Context(), ModernProtocolVersion)), router.modern)
	default:
		w.Header().Set("MCP-Supported-Protocol-Versions", LegacyProtocolVersion+", "+ModernProtocolVersion)
		http.Error(w, "missing or unsupported MCP-Protocol-Version", http.StatusBadRequest)
	}
}

func (router *Router) serveAdapter(w http.ResponseWriter, r *http.Request, handler http.Handler) {
	if handler == nil {
		http.Error(w, "protocol adapter unavailable", http.StatusServiceUnavailable)
		return
	}
	handler.ServeHTTP(w, r)
}
