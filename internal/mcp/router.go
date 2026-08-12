package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"neuralmail/internal/auth"
	"neuralmail/internal/config"
)

const (
	LegacyProtocolVersion        = "2025-11-25"
	ModernProtocolVersion        = "2026-07-28"
	protectedResourceMetadataURL = "https://nerve-runtime.fly.dev" + ProtectedResourceMetadataMCPPath
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
			writeInvalidToken(w)
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
	versions := r.Header.Values("MCP-Protocol-Version")
	if len(versions) != 1 {
		writeHeaderMismatch(w, nil, "MCP-Protocol-Version header is required exactly once")
		return
	}
	requestedVersion := versions[0]
	switch requestedVersion {
	case LegacyProtocolVersion:
		router.serveAdapter(w, request.WithContext(withRoutedProtocolVersion(request.Context(), LegacyProtocolVersion)), router.legacy)
	case ModernProtocolVersion:
		router.serveAdapter(w, request.WithContext(withRoutedProtocolVersion(request.Context(), ModernProtocolVersion)), router.modern)
	default:
		w.Header().Set("MCP-Supported-Protocol-Versions", LegacyProtocolVersion+", "+ModernProtocolVersion)
		writeProtocolError(w, nil, sdkmcp.CodeUnsupportedProtocolVersion, "unsupported protocol version", sdkmcp.UnsupportedProtocolVersionData{
			Supported: []string{LegacyProtocolVersion, ModernProtocolVersion},
			Requested: requestedVersion,
		})
	}
}

func writeInvalidToken(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+protectedResourceMetadataURL+`", error="invalid_token"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

func writeInsufficientScope(w http.ResponseWriter, requiredScope string) {
	w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope", scope="`+requiredScope+`"`)
	http.Error(w, "forbidden", http.StatusForbidden)
}

func writeHeaderMismatch(w http.ResponseWriter, id any, message string) {
	writeProtocolError(w, id, sdkmcp.CodeHeaderMismatch, message, nil)
}

func writeProtocolError(w http.ResponseWriter, id any, code int, message string, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(Response{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &ResponseError{Code: code, Message: message, Data: data},
	})
}

func (router *Router) serveAdapter(w http.ResponseWriter, r *http.Request, handler http.Handler) {
	if handler == nil {
		http.Error(w, "protocol adapter unavailable", http.StatusServiceUnavailable)
		return
	}
	handler.ServeHTTP(w, r)
}
