package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
)

func (s *Store) LookupCloudAPIKey(ctx context.Context, keyHash string) (CloudAPIKey, error) {
	var key CloudAPIKey
	if keyHash == "" {
		return key, sql.ErrNoRows
	}
	var scopesText string
	row := s.q.QueryRowContext(ctx, `SELECT id, org_id, scopes::text, revoked_at FROM cloud_api_keys WHERE key_hash = $1`, keyHash)
	if err := row.Scan(&key.ID, &key.OrgID, &scopesText, &key.RevokedAt); err != nil {
		return key, err
	}
	key.Scopes = parseScopes(scopesText)
	return key, nil
}

func (s *Store) CreateServiceToken(ctx context.Context, tokenID string, orgID string, actor string, scopes []string, expiresAt time.Time) error {
	if tokenID == "" {
		tokenID = uuid.NewString()
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO service_tokens (id, org_id, actor, scopes, expires_at)
		VALUES ($1, $2, $3, $4, $5)
	`, tokenID, orgID, actor, scopes, expiresAt)
	return err
}

func (s *Store) RevokeActiveServiceTokens(ctx context.Context, orgID string) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE service_tokens
		SET revoked_at = now()
		WHERE org_id = $1
		  AND revoked_at IS NULL
		  AND expires_at > now()
	`, orgID)
	return err
}

func (s *Store) GetServiceToken(ctx context.Context, tokenID string) (ServiceToken, error) {
	var token ServiceToken
	if tokenID == "" {
		return token, sql.ErrNoRows
	}
	var scopesText string
	row := s.q.QueryRowContext(ctx, `
		SELECT id, org_id, actor, scopes::text, expires_at, revoked_at
		FROM service_tokens
		WHERE id = $1
	`, tokenID)
	if err := row.Scan(&token.ID, &token.OrgID, &token.Actor, &scopesText, &token.ExpiresAt, &token.RevokedAt); err != nil {
		return token, err
	}
	token.Scopes = parseScopes(scopesText)
	return token, nil
}

func (s *Store) CreateCloudAPIKey(ctx context.Context, orgID string, keyPrefix string, keyHash string, label string, scopes []string) (CloudAPIKey, error) {
	var key CloudAPIKey
	var scopesText string
	row := s.q.QueryRowContext(ctx, `
		INSERT INTO cloud_api_keys (org_id, key_prefix, key_hash, label, scopes)
		VALUES ($1, $2, $3, nullif($4, ''), $5)
		RETURNING id, org_id, key_prefix, coalesce(label, ''), scopes::text, created_at, revoked_at
	`, orgID, keyPrefix, keyHash, label, scopes)
	if err := row.Scan(&key.ID, &key.OrgID, &key.KeyPrefix, &key.Label, &scopesText, &key.CreatedAt, &key.RevokedAt); err != nil {
		return key, err
	}
	key.Scopes = parseScopes(scopesText)
	return key, nil
}

func (s *Store) ListCloudAPIKeys(ctx context.Context, orgID string) ([]CloudAPIKey, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, org_id, key_prefix, coalesce(label, ''), scopes::text, created_at, revoked_at
		FROM cloud_api_keys
		WHERE org_id = $1
		ORDER BY created_at DESC
	`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	keys := make([]CloudAPIKey, 0)
	for rows.Next() {
		var key CloudAPIKey
		var scopesText string
		if err := rows.Scan(&key.ID, &key.OrgID, &key.KeyPrefix, &key.Label, &scopesText, &key.CreatedAt, &key.RevokedAt); err != nil {
			return nil, err
		}
		key.Scopes = parseScopes(scopesText)
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (s *Store) RevokeCloudAPIKey(ctx context.Context, orgID string, keyID string) (bool, error) {
	result, err := s.q.ExecContext(ctx, `
		UPDATE cloud_api_keys
		SET revoked_at = now()
		WHERE id = $1
		  AND org_id = $2
		  AND revoked_at IS NULL
	`, keyID, orgID)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

func parseScopes(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" || raw == "null" {
		return []string{}
	}

	if strings.HasPrefix(raw, "[") {
		var parsed []string
		if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
			return normalizeScopes(parsed)
		}
	}

	if strings.HasPrefix(raw, "{") && strings.HasSuffix(raw, "}") {
		raw = strings.TrimSpace(raw[1 : len(raw)-1])
		if raw == "" {
			return []string{}
		}
	}

	if !strings.Contains(raw, ",") && strings.Contains(raw, " ") {
		return normalizeScopes(strings.Fields(raw))
	}

	return normalizeScopes(strings.Split(raw, ","))
}

func normalizeScopes(values []string) []string {
	scopes := make([]string, 0, len(values))
	for _, value := range values {
		item := strings.TrimSpace(strings.Trim(value, `"`))
		if item != "" {
			scopes = append(scopes, item)
		}
	}
	return scopes
}
