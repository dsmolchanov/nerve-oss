package auth

import (
	"context"
	"time"
)

type PrincipalKind string

const (
	PrincipalLegacyJWT     PrincipalKind = "legacy_jwt"
	PrincipalBootstrap     PrincipalKind = "legacy_bootstrap"
	PrincipalCloudAPIKey   PrincipalKind = "cloud_api_key"
	PrincipalM2MOnboarding PrincipalKind = "m2m_onboarding"
	PrincipalM2MOrg        PrincipalKind = "m2m_org"
)

type Principal struct {
	OrgID      string
	ActorID    string
	TokenID    string
	ClientID   string
	Generation int64
	Scopes     []string
	ExpiresAt  time.Time
	Kind       PrincipalKind
	AuthMethod string // jwt or cloud_api_key
}

func (p Principal) IsM2M() bool {
	return p.Kind == PrincipalM2MOnboarding || p.Kind == PrincipalM2MOrg
}

type principalContextKey struct{}

func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, principal)
}

func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(Principal)
	return principal, ok
}
