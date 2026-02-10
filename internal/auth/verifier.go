package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"neuralmail/internal/config"
	"neuralmail/internal/store"
)

var (
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")
)

type CloudKeyLookupFunc func(ctx context.Context, keyHash string) (store.CloudAPIKey, error)

type Service struct {
	Config         config.Config
	Store          *store.Store
	Now            func() time.Time
	LookupCloudKey CloudKeyLookupFunc
}

func NewService(cfg config.Config, st *store.Store) *Service {
	svc := &Service{
		Config: cfg,
		Store:  st,
		Now:    func() time.Time { return time.Now().UTC() },
	}
	if st != nil {
		svc.LookupCloudKey = st.LookupCloudAPIKey
	}
	return svc
}

func (s *Service) AuthenticateRequest(r *http.Request) (Principal, error) {
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
		return s.VerifyJWT(authHeader)
	}
	if key := strings.TrimSpace(r.Header.Get("X-Nerve-Cloud-Key")); key != "" {
		return s.VerifyCloudAPIKey(r.Context(), key)
	}
	return Principal{}, ErrUnauthorized
}

func (s *Service) VerifyJWT(authHeader string) (Principal, error) {
	headerParts := strings.Fields(authHeader)
	if len(headerParts) != 2 || !strings.EqualFold(headerParts[0], "Bearer") {
		return Principal{}, ErrUnauthorized
	}
	token := strings.TrimSpace(headerParts[1])

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Principal{}, ErrUnauthorized
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Principal{}, ErrUnauthorized
	}

	var claims map[string]any
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return Principal{}, ErrUnauthorized
	}
	if err := s.validateStandardClaims(claims); err != nil {
		return Principal{}, err
	}

	orgID := claimString(claims["org_id"])
	if orgID == "" {
		return Principal{}, ErrUnauthorized
	}

	return Principal{
		OrgID:      orgID,
		ActorID:    claimString(claims["sub"]),
		TokenID:    claimString(claims["jti"]),
		Scopes:     extractScopes(claims["scope"]),
		AuthMethod: "jwt",
	}, nil
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

func (s *Service) validateStandardClaims(claims map[string]any) error {
	now := s.Now().Unix()

	if exp := claimInt64(claims["exp"]); exp > 0 && now >= exp {
		return ErrUnauthorized
	}
	if nbf := claimInt64(claims["nbf"]); nbf > 0 && now < nbf {
		return ErrUnauthorized
	}

	if expectedIssuer := strings.TrimSpace(s.Config.Auth.Issuer); expectedIssuer != "" {
		if claimString(claims["iss"]) != expectedIssuer {
			return ErrUnauthorized
		}
	}
	if expectedAudience := strings.TrimSpace(s.Config.Auth.Audience); expectedAudience != "" && !hasAudience(claims["aud"], expectedAudience) {
		return ErrUnauthorized
	}
	return nil
}

func hasAudience(claim any, expected string) bool {
	switch v := claim.(type) {
	case string:
		return v == expected
	case []any:
		for _, value := range v {
			if claimString(value) == expected {
				return true
			}
		}
	case []string:
		for _, value := range v {
			if value == expected {
				return true
			}
		}
	}
	return false
}

func claimString(v any) string {
	switch value := v.(type) {
	case string:
		return strings.TrimSpace(value)
	default:
		return ""
	}
}

func claimInt64(v any) int64 {
	switch value := v.(type) {
	case float64:
		return int64(value)
	case int64:
		return value
	case json.Number:
		i, _ := value.Int64()
		return i
	default:
		return 0
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
