package cloudapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	"neuralmail/internal/store"
)

type IssuedToken struct {
	Token     string    `json:"token"`
	TokenID   string    `json:"token_id"`
	ExpiresAt time.Time `json:"expires_at"`
	Scopes    []string  `json:"scopes"`
}

type ServiceTokenIssuer interface {
	IssueServiceToken(ctx context.Context, orgID string, actor string, scopes []string, ttl time.Duration, rotate bool) (IssuedToken, error)
}

type TokenService struct {
	Store *store.Store
	Now   func() time.Time
}

func NewTokenService(st *store.Store) *TokenService {
	return &TokenService{
		Store: st,
		Now:   func() time.Time { return time.Now().UTC() },
	}
}

func (s *TokenService) IssueServiceToken(ctx context.Context, orgID string, actor string, scopes []string, ttl time.Duration, rotate bool) (IssuedToken, error) {
	var issued IssuedToken
	if s == nil || s.Store == nil {
		return issued, errors.New("token service not configured")
	}
	if orgID == "" {
		return issued, errors.New("missing org id")
	}
	if len(scopes) == 0 {
		return issued, errors.New("missing scopes")
	}
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	if ttl > time.Hour {
		return issued, errors.New("ttl exceeds maximum")
	}
	if actor == "" {
		actor = "system"
	}

	now := s.Now()
	expiresAt := now.Add(ttl)
	tokenID := uuid.NewString()
	claims := map[string]any{
		"org_id":    orgID,
		"sub":       actor,
		"jti":       tokenID,
		"scope":     scopes,
		"iat":       now.Unix(),
		"exp":       expiresAt.Unix(),
		"token_use": "service",
	}
	token, err := unsignedJWT(claims)
	if err != nil {
		return issued, err
	}

	if rotate {
		if err := s.Store.RevokeActiveServiceTokens(ctx, orgID); err != nil {
			return issued, err
		}
	}
	if err := s.Store.CreateServiceToken(ctx, tokenID, orgID, actor, scopes, expiresAt); err != nil {
		return issued, err
	}

	inputHash := hashAny(map[string]any{
		"org_id": orgID,
		"actor":  actor,
		"scopes": scopes,
		"ttl":    ttl.Seconds(),
		"rotate": rotate,
	})
	outputHash := hashAny(map[string]any{
		"token_id":   tokenID,
		"expires_at": expiresAt.Unix(),
		"scopes":     scopes,
	})
	toolCallID, err := s.Store.RecordToolCall(ctx, "issue_service_token", tokenID, "", "control-plane", 0)
	if err == nil {
		_ = s.Store.RecordAudit(ctx, toolCallID, actor, inputHash, outputHash, "")
	}

	issued = IssuedToken{
		Token:     token,
		TokenID:   tokenID,
		ExpiresAt: expiresAt,
		Scopes:    scopes,
	}
	return issued, nil
}

func unsignedJWT(claims map[string]any) (string, error) {
	headerBytes, err := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claimsBytes, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(headerBytes) + "." + base64.RawURLEncoding.EncodeToString(claimsBytes) + ".", nil
}

func hashAny(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
