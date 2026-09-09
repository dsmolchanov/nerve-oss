// Package localauth holds OSS-only bearer identities. These are independent
// of cloud organizations and never grant cloud/delegation scopes.
package localauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"neuralmail/internal/config"
	"strings"
)

var ErrForbidden = errors.New("local inbox access denied")

type Identity struct{ InboxIDs []string }
type identityKey struct{}

func FromContext(ctx context.Context) (Identity, bool) {
	p, ok := ctx.Value(identityKey{}).(Identity)
	return p, ok
}
func WithIdentity(ctx context.Context, p Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, p)
}

func Authenticate(cfg config.Config, r *http.Request) (context.Context, error) {
	if cfg.Security.APIKey == "" && len(cfg.Security.LocalAPIKeys) == 0 {
		return r.Context(), nil
	}
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return nil, ErrForbidden
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return nil, ErrForbidden
	}
	digest := sha256.Sum256([]byte(parts[1]))
	matches := func(key string) bool {
		expected := sha256.Sum256([]byte(key))
		return subtle.ConstantTimeCompare(digest[:], expected[:]) == 1
	}
	if cfg.Security.APIKey != "" && matches(cfg.Security.APIKey) {
		return r.Context(), nil
	}
	for _, key := range cfg.Security.LocalAPIKeys {
		if matches(key.Token) {
			return WithIdentity(r.Context(), Identity{InboxIDs: append([]string(nil), key.InboxIDs...)}), nil
		}
	}
	return nil, ErrForbidden
}
