package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"neuralmail/internal/config"
	"neuralmail/internal/store"
)

var (
	ErrUnauthorized = errors.New("unauthorized")
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
	return svc
}

func (s *Service) AuthenticateRequest(r *http.Request) (Principal, error) {
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
		return s.VerifyJWT(r.Context(), authHeader)
	}
	if key := strings.TrimSpace(r.Header.Get("X-Nerve-Cloud-Key")); key != "" {
		return s.VerifyCloudAPIKey(r.Context(), key)
	}
	return Principal{}, ErrUnauthorized
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
	if iss := strings.TrimSpace(s.Config.Auth.Issuer); iss != "" {
		parserOpts = append(parserOpts, jwt.WithIssuer(iss))
	}
	if aud := strings.TrimSpace(s.Config.Auth.Audience); aud != "" {
		parserOpts = append(parserOpts, jwt.WithAudience(aud))
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
		AuthMethod: "cloud_api_key",
	}, nil
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
