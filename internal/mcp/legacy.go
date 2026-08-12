package mcp

import "net/http"

// LegacyAdapter is the frozen, sessionful 2025-11-25 wire adapter. It owns
// only legacy envelope/session/error translation; tool execution is delegated
// to the shared Invoker held by Server.
type LegacyAdapter struct {
	server *Server
}

func NewLegacyHandler(server *Server) http.Handler {
	return &LegacyAdapter{server: server}
}

func (adapter *LegacyAdapter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if adapter == nil || adapter.server == nil {
		http.Error(w, "legacy MCP adapter unavailable", http.StatusServiceUnavailable)
		return
	}
	adapter.server.HandleRoutedHTTP(w, r)
}
