package cloudapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
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
	Store      *store.Store
	SigningKey []byte
	Now        func() time.Time
}

func NewTokenService(st *store.Store, signingKey string) *TokenService {
	return &TokenService{
		Store:      st,
		SigningKey: []byte(signingKey),
		Now:        func() time.Time { return time.Now().UTC() },
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

	if len(s.SigningKey) == 0 {
		return issued, errors.New("token signing key not configured")
	}

	now := s.Now()
	expiresAt := now.Add(ttl)
	tokenID := uuid.NewString()
	jwtClaims := jwt.MapClaims{
		"org_id":    orgID,
		"sub":       actor,
		"jti":       tokenID,
		"scope":     scopes,
		"iat":       now.Unix(),
		"exp":       expiresAt.Unix(),
		"token_use": "service",
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwtClaims)
	token, err := tok.SignedString(s.SigningKey)
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

func hashAny(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
