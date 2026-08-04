package featureflags

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"neuralmail/internal/auth"
)

const EffectiveStatePathPrefix = "/internal/feature-flags/"

type Gate interface {
	Enabled(ctx context.Context, flag string, orgID string) (bool, error)
}

// EffectiveStateHandler is the authenticated convergence probe shared by the
// runtime and control-plane apps. It always resolves for the caller's org; an
// org_id query parameter cannot override tenant identity.
func EffectiveStateHandler(authService *auth.Service, gate Gate) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		flag := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, EffectiveStatePathPrefix))
		if !validFlagName(flag) {
			http.Error(w, "invalid feature flag", http.StatusBadRequest)
			return
		}
		if authService == nil || gate == nil {
			http.Error(w, "feature flag probe unavailable", http.StatusServiceUnavailable)
			return
		}
		principal, err := authService.AuthenticateRequest(r)
		if err != nil {
			if errors.Is(err, auth.ErrUnauthenticated) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			http.Error(w, "authentication failed", http.StatusInternalServerError)
			return
		}
		if err := authService.ValidateScopes(principal, "nerve:email.read"); err != nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		enabled, err := gate.Enabled(r.Context(), flag, principal.OrgID)
		if err != nil {
			http.Error(w, "feature flag resolution failed", http.StatusServiceUnavailable)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"flag":              flag,
			"org_id":            principal.OrgID,
			"enabled":           enabled,
			"cache_ttl_seconds": int(DefaultTTL.Seconds()),
		})
	})
}

func validFlagName(flag string) bool {
	if flag == "" {
		return false
	}
	for _, character := range flag {
		if (character < 'a' || character > 'z') &&
			(character < '0' || character > '9') && character != '_' {
			return false
		}
	}
	return true
}
