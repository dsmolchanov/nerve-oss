package mcp

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"neuralmail/internal/config"
	"strings"
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

func ProtectedResourceMetadataHandler(configs ...config.Config) http.Handler {
	body := protectedResourceMetadataBody
	if len(configs) > 0 {
		var data map[string]any
		_ = json.Unmarshal(body, &data)
		cfg := configs[0]
		if cfg.Cloud.PublicBaseURL != "" {
			data["resource"] = strings.TrimRight(cfg.Cloud.PublicBaseURL, "/") + "/mcp"
		}
		if cfg.Auth.Issuer != "" {
			data["authorization_servers"] = []string{cfg.Auth.Issuer}
		}
		body, _ = json.Marshal(data)
		body = append(body, '\n')
	}
	digest := sha256.Sum256(body)
	etag := fmt.Sprintf(`"%x"`, digest)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", protectedResourceCacheControl)
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write(body)
	})
}
