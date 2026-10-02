package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"neuralmail/internal/auth"
	"neuralmail/internal/config"
	"neuralmail/internal/localauth"
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

type inferredProtocolVersionKey struct{}

func withRoutedProtocolVersion(ctx context.Context, version string) context.Context {
	return context.WithValue(ctx, routedProtocolVersionKey{}, version)
}

// withInferredProtocolVersion marks a version the router assumed because the
// client sent no MCP-Protocol-Version header. It pins the response version but,
// unlike an explicit header, must not constrain initialize negotiation.
func withInferredProtocolVersion(ctx context.Context, version string) context.Context {
	return context.WithValue(withRoutedProtocolVersion(ctx, version), inferredProtocolVersionKey{}, true)
}

func protocolVersionInferred(ctx context.Context) bool {
	inferred, _ := ctx.Value(inferredProtocolVersionKey{}).(bool)
	return inferred
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
			writeInvalidToken(w, router.config)
			return
		}
		authenticated = true
		ctx = auth.WithPrincipal(ctx, principal)
	}
	if !router.config.Cloud.Mode {
		ctx, err = localauth.Authenticate(router.config, r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	if err := authorizeOrigin(router.config, origin, principal, authenticated); err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	request := r.WithContext(ctx)
	versions := r.Header.Values("MCP-Protocol-Version")
	if len(versions) > 1 {
		writeHeaderMismatch(w, nil, "MCP-Protocol-Version header must not be repeated")
		return
	}
	// Clients send MCP-Protocol-Version only after version negotiation, so the
	// initialize request (and pre-2025-06-18 clients) arrive without it. The
	// spec says to treat those as legacy, which the sessionful adapter handles.
	// Mcp-Method/Mcp-Name only exist in the modern protocol, so a headerless
	// request carrying them is a malformed modern request, not a legacy one.
	if len(versions) == 0 {
		if len(r.Header.Values("Mcp-Method")) > 0 || len(r.Header.Values("Mcp-Name")) > 0 {
			writeHeaderMismatch(w, nil, "MCP-Protocol-Version header is required")
			return
		}
		router.serveAdapter(w, request.WithContext(withInferredProtocolVersion(request.Context(), LegacyProtocolVersion)), router.legacy)
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

func writeInvalidToken(w http.ResponseWriter, configs ...config.Config) {
	metadataURL := protectedResourceMetadataURL
	if len(configs) > 0 && configs[0].Cloud.PublicBaseURL != "" {
		metadataURL = strings.TrimRight(configs[0].Cloud.PublicBaseURL, "/") + ProtectedResourceMetadataMCPPath
	}
	w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+metadataURL+`", error="invalid_token"`)
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
