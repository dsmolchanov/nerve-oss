package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"neuralmail/internal/config"
	"neuralmail/internal/store"
)

var (
	ErrUnauthenticated = errors.New("unauthorized")
	// ErrUnauthorized is retained as an API-compatible alias. New HTTP
	// boundaries should classify this condition as unauthenticated (401).
	ErrUnauthorized = ErrUnauthenticated
	ErrForbidden    = errors.New("forbidden")
)

type CloudKeyLookupFunc func(ctx context.Context, keyHash string) (store.CloudAPIKey, error)
type ServiceTokenLookupFunc func(ctx context.Context, tokenID string) (store.ServiceToken, error)

type Service struct {
	Config             config.Config
	Store              *store.Store
	Now                func() time.Time
	LookupCloudKey     CloudKeyLookupFunc
	LookupServiceToken ServiceTokenLookupFunc
	M2M                *M2MTokenVerifier
}

type M2MTokenVerifier struct {
	Issuer   string
	Audience string
	Keys     *RSAPublicKeySet
	Remote   *RemoteJWKS
	Now      func() time.Time
	Skew     time.Duration
}

func NewService(cfg config.Config, st *store.Store) *Service {
	svc := &Service{
		Config: cfg,
		Store:  st,
		Now:    func() time.Time { return time.Now().UTC() },
	}
	if st != nil {
		svc.LookupCloudKey = st.LookupCloudAPIKey
		svc.LookupServiceToken = st.GetServiceToken
	}
	if issuer, audience, jwksURL := strings.TrimSpace(cfg.Auth.Issuer), strings.TrimSpace(cfg.Auth.Audience), strings.TrimSpace(cfg.Auth.JWKSURL); issuer != "" && audience != "" && jwksURL != "" {
		remote, err := NewRemoteJWKS(jwksURL, nil)
		if err == nil {
			svc.M2M = &M2MTokenVerifier{Issuer: issuer, Audience: audience, Remote: remote}
		}
	}
	return svc
}

func (s *Service) AuthenticateRequest(r *http.Request) (Principal, error) {
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
		return s.verifyBearer(r.Context(), authHeader)
	}
	if key := strings.TrimSpace(r.Header.Get("X-Nerve-Cloud-Key")); key != "" {
		return s.VerifyCloudAPIKey(r.Context(), key)
	}
	if bootstrap := strings.TrimSpace(r.Header.Get("X-API-Key")); bootstrap != "" &&
		bootstrap == strings.TrimSpace(s.Config.Security.APIKey) {
		return Principal{
			ActorID:    "bootstrap_admin",
			Scopes:     []string{"*"},
			Kind:       PrincipalBootstrap,
			AuthMethod: "bootstrap_key",
		}, nil
	}
	return Principal{}, ErrUnauthorized
}

func (s *Service) verifyBearer(ctx context.Context, authHeader string) (Principal, error) {
	headerParts := strings.Fields(authHeader)
	if len(headerParts) != 2 || !strings.EqualFold(headerParts[0], "Bearer") {
		return Principal{}, ErrUnauthenticated
	}
	unverified, _, err := jwt.NewParser().ParseUnverified(headerParts[1], jwt.MapClaims{})
	if err != nil {
		return Principal{}, ErrUnauthenticated
	}
	switch unverified.Method.Alg() {
	case jwt.SigningMethodHS256.Alg():
		return s.VerifyJWT(ctx, authHeader)
	case jwt.SigningMethodPS256.Alg():
		if s.M2M == nil {
			return Principal{}, ErrUnauthenticated
		}
		principal, err := s.M2M.Verify(ctx, headerParts[1])
		if err != nil {
			return Principal{}, err
		}
		if principal.Kind == PrincipalM2MOrg {
			return s.bindM2MOrgServiceToken(ctx, principal)
		}
		return principal, nil
	default:
		return Principal{}, ErrUnauthenticated
	}
}

func (s *Service) bindM2MOrgServiceToken(ctx context.Context, principal Principal) (Principal, error) {
	if s.LookupServiceToken == nil || principal.TokenID == "" || principal.OrgID == "" ||
		principal.ClientID == "" || principal.Generation <= 0 || principal.ExpiresAt.IsZero() {
		return Principal{}, ErrUnauthenticated
	}
	token, err := s.LookupServiceToken(ctx, principal.TokenID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Principal{}, ErrUnauthenticated
		}
		return Principal{}, err
	}
	expectedActor := fmt.Sprintf("oauth_client:%s:g:%d", principal.ClientID, principal.Generation)
	if token.ID != principal.TokenID || token.OrgID != principal.OrgID || token.Actor != expectedActor ||
		token.RevokedAt.Valid || !token.ExpiresAt.Equal(principal.ExpiresAt) || !equalScopes(token.Scopes, principal.Scopes) {
		return Principal{}, ErrUnauthenticated
	}
	// Source resource authority from the durable row after proving exact claim
	// equality. A targeted revoke therefore takes effect immediately rather
	// than waiting for the signed JWT to expire.
	principal.OrgID = token.OrgID
	principal.ActorID = token.Actor
	principal.Scopes = append([]string(nil), token.Scopes...)
	return principal, nil
}

func equalScopes(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (s *Service) VerifyJWT(ctx context.Context, authHeader string) (Principal, error) {
	headerParts := strings.Fields(authHeader)
	if len(headerParts) != 2 || !strings.EqualFold(headerParts[0], "Bearer") {
		return Principal{}, ErrUnauthorized
	}
	rawToken := strings.TrimSpace(headerParts[1])

	signingKey := []byte(s.Config.Security.TokenSigningKey)
	if len(signingKey) == 0 {
		return Principal{}, fmt.Errorf("%w: token signing key not configured", ErrUnauthorized)
	}

	parserOpts := []jwt.ParserOption{
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithTimeFunc(s.Now),
	}

	parsed, err := jwt.Parse(rawToken, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return signingKey, nil
	}, parserOpts...)
	if err != nil || !parsed.Valid {
		return Principal{}, ErrUnauthorized
	}

	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		return Principal{}, ErrUnauthorized
	}

	orgID := claimString(claims["org_id"])
	if orgID == "" {
		return Principal{}, ErrUnauthorized
	}
	tokenID := claimString(claims["jti"])
	if servicePrincipal, ok, err := s.resolveServiceTokenPrincipal(ctx, tokenID); err != nil {
		return Principal{}, err
	} else if ok {
		return servicePrincipal, nil
	}

	return Principal{
		OrgID:      orgID,
		ActorID:    claimString(claims["sub"]),
		TokenID:    tokenID,
		Scopes:     extractScopes(claims["scope"]),
		Kind:       PrincipalLegacyJWT,
		AuthMethod: "jwt",
	}, nil
}

func (s *Service) resolveServiceTokenPrincipal(ctx context.Context, tokenID string) (Principal, bool, error) {
	if tokenID == "" || s.LookupServiceToken == nil {
		return Principal{}, false, nil
	}
	token, err := s.LookupServiceToken(ctx, tokenID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Principal{}, false, nil
		}
		return Principal{}, false, err
	}
	now := s.Now()
	if token.RevokedAt.Valid || !token.ExpiresAt.After(now) {
		return Principal{}, true, ErrUnauthorized
	}
	return Principal{
		OrgID:      token.OrgID,
		ActorID:    token.Actor,
		TokenID:    token.ID,
		Scopes:     token.Scopes,
		Kind:       PrincipalLegacyJWT,
		AuthMethod: "jwt",
	}, true, nil
}

func (s *Service) VerifyCloudAPIKey(ctx context.Context, key string) (Principal, error) {
	if s.LookupCloudKey == nil {
		return Principal{}, ErrUnauthorized
	}
	keyHash := hashCloudKey(key)
	record, err := s.LookupCloudKey(ctx, keyHash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Principal{}, ErrUnauthorized
		}
		return Principal{}, err
	}
	if record.RevokedAt.Valid {
		return Principal{}, ErrUnauthorized
	}
	return Principal{
		OrgID:      record.OrgID,
		ActorID:    "cloud_api_key:" + record.ID,
		TokenID:    record.ID,
		Scopes:     record.Scopes,
		Kind:       PrincipalCloudAPIKey,
		AuthMethod: "cloud_api_key",
	}, nil
}

func (v *M2MTokenVerifier) Verify(ctx context.Context, rawToken string) (Principal, error) {
	if v == nil || (v.Keys == nil && v.Remote == nil) || strings.TrimSpace(v.Issuer) == "" || strings.TrimSpace(v.Audience) == "" {
		return Principal{}, ErrUnauthenticated
	}
	now := v.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	skew := v.Skew
	if skew == 0 {
		skew = 30 * time.Second
	}
	if skew < 0 || skew > time.Minute {
		return Principal{}, ErrUnauthenticated
	}
	parsed, err := jwt.Parse(rawToken, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodPS256 {
			return nil, ErrUnauthenticated
		}
		kid, ok := token.Header["kid"].(string)
		if !ok || kid == "" {
			return nil, ErrUnauthenticated
		}
		if v.Remote != nil {
			key, err := v.Remote.Lookup(ctx, kid)
			if err != nil {
				return nil, ErrUnauthenticated
			}
			return key, nil
		}
		key, found := v.Keys.Lookup(kid)
		if !found {
			return nil, ErrUnauthenticated
		}
		return key, nil
	}, jwt.WithValidMethods([]string{"PS256"}), jwt.WithIssuer(v.Issuer), jwt.WithAudience(v.Audience), jwt.WithTimeFunc(now), jwt.WithLeeway(skew), jwt.WithExpirationRequired(), jwt.WithIssuedAt())
	if err != nil || !parsed.Valid {
		return Principal{}, ErrUnauthenticated
	}
	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		return Principal{}, ErrUnauthenticated
	}
	expiresAt, expErr := claims.GetExpirationTime()
	issuedAt, iatErr := claims.GetIssuedAt()
	notBefore, nbfErr := claims.GetNotBefore()
	if expErr != nil || iatErr != nil || nbfErr != nil || expiresAt == nil || issuedAt == nil || notBefore == nil {
		return Principal{}, ErrUnauthenticated
	}
	if !notBefore.Time.Equal(issuedAt.Time) || !expiresAt.Time.After(issuedAt.Time) {
		return Principal{}, ErrUnauthenticated
	}
	clientID := claimString(claims["client_id"])
	if clientID == "" || clientID != claimString(claims["sub"]) {
		return Principal{}, ErrUnauthenticated
	}
	generation, ok := claimPositiveInt64(claims["generation"])
	if !ok {
		return Principal{}, ErrUnauthenticated
	}
	scopes, ok := canonicalM2MScopes(claims["scope"])
	if !ok {
		return Principal{}, ErrUnauthenticated
	}
	principal := Principal{
		OrgID:      claimString(claims["org_id"]),
		ActorID:    clientID,
		TokenID:    claimString(claims["jti"]),
		ClientID:   clientID,
		Generation: generation,
		Scopes:     scopes,
		ExpiresAt:  expiresAt.Time,
		AuthMethod: "m2m_bearer",
	}
	if principal.TokenID == "" || len(principal.Scopes) == 0 {
		return Principal{}, ErrUnauthenticated
	}
	switch claimString(claims["token_use"]) {
	case string(PrincipalM2MOnboarding):
		if principal.OrgID != "" || expiresAt.Time.Sub(issuedAt.Time) != 5*time.Minute ||
			len(principal.Scopes) != 1 || principal.Scopes[0] != "nerve:onboarding" {
			return Principal{}, ErrUnauthenticated
		}
		principal.Kind = PrincipalM2MOnboarding
	case string(PrincipalM2MOrg):
		if principal.OrgID == "" || expiresAt.Time.Sub(issuedAt.Time) != 15*time.Minute ||
			containsString(principal.Scopes, "nerve:onboarding") {
			return Principal{}, ErrUnauthenticated
		}
		principal.Kind = PrincipalM2MOrg
	default:
		return Principal{}, ErrUnauthenticated
	}
	return principal, nil
}

func canonicalM2MScopes(claim any) ([]string, bool) {
	raw, ok := claim.(string)
	if !ok || raw == "" || len(raw) > 512 {
		return nil, false
	}
	order := map[string]int{
		"nerve:onboarding":        0,
		"nerve:email.read":        1,
		"nerve:email.search":      2,
		"nerve:email.draft":       3,
		"nerve:email.reply":       4,
		"nerve:email.compose":     5,
		"nerve:billing.subscribe": 6,
	}
	scopes := strings.Split(raw, " ")
	if len(scopes) == 0 || len(scopes) > 6 {
		return nil, false
	}
	seen := make(map[string]bool, len(scopes))
	previous := -1
	for _, scope := range scopes {
		position, known := order[scope]
		if !known || seen[scope] || len(scope) > 64 || position <= previous {
			return nil, false
		}
		seen[scope] = true
		previous = position
	}
	return scopes, true
}

func claimPositiveInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		integer := int64(typed)
		return integer, typed == float64(integer) && integer > 0
	case json.Number:
		integer, err := typed.Int64()
		return integer, err == nil && integer > 0
	case string:
		integer, err := strconv.ParseInt(typed, 10, 64)
		return integer, err == nil && integer > 0 && strconv.FormatInt(integer, 10) == typed
	default:
		return 0, false
	}
}

func (s *Service) ValidateScopes(principal Principal, requiredScope string) error {
	if requiredScope == "" {
		return nil
	}
	for _, scope := range principal.Scopes {
		if scope == "*" || scope == requiredScope {
			return nil
		}
		if strings.HasSuffix(scope, ".*") {
			prefix := strings.TrimSuffix(scope, ".*")
			if strings.HasPrefix(requiredScope, prefix+".") {
				return nil
			}
		}
	}
	return ErrForbidden
}

func claimString(v any) string {
	switch value := v.(type) {
	case string:
		return strings.TrimSpace(value)
	default:
		return ""
	}
}

func extractScopes(claim any) []string {
	var scopes []string
	switch value := claim.(type) {
	case string:
		for _, item := range strings.Fields(value) {
			if item != "" {
				scopes = append(scopes, item)
			}
		}
	case []any:
		for _, item := range value {
			if scope := claimString(item); scope != "" {
				scopes = append(scopes, scope)
			}
		}
	case []string:
		for _, item := range value {
			if item != "" {
				scopes = append(scopes, item)
			}
		}
	}
	return scopes
}

func hashCloudKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
