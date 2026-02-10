package auth

import "context"

type Principal struct {
	OrgID      string
	ActorID    string
	TokenID    string
	Scopes     []string
	AuthMethod string // jwt or cloud_api_key
}

type principalContextKey struct{}

func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, principal)
}

func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(Principal)
	return principal, ok
}
