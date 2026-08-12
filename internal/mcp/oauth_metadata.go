package mcp

import (
	"crypto/sha256"
	"fmt"
	"net/http"
)

const (
	ProtectedResourceMetadataPath    = "/.well-known/oauth-protected-resource"
	ProtectedResourceMetadataMCPPath = "/.well-known/oauth-protected-resource/mcp"
	protectedResourceCacheControl    = "public, max-age=300, must-revalidate"
)

var (
	protectedResourceMetadataBody = []byte(`{"resource":"https://nerve-runtime.fly.dev/mcp","authorization_servers":["https://auth.nerve.email"],"scopes_supported":["nerve:onboarding","nerve:email.read","nerve:email.search","nerve:email.draft","nerve:email.reply","nerve:email.compose","nerve:billing.subscribe"],"bearer_methods_supported":["header"]}` + "\n")
	protectedResourceMetadataETag = func() string {
		digest := sha256.Sum256(protectedResourceMetadataBody)
		return fmt.Sprintf(`"%x"`, digest)
	}()
)

func ProtectedResourceMetadataHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", protectedResourceCacheControl)
		w.Header().Set("ETag", protectedResourceMetadataETag)
		if r.Header.Get("If-None-Match") == protectedResourceMetadataETag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write(protectedResourceMetadataBody)
	})
}
