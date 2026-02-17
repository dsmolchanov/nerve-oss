package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type ToolIdempotencyRecord struct {
	OrgID          string
	ToolName       string
	IdempotencyKey string
	Status         string
	CachedResponse json.RawMessage
	UpdatedAt      time.Time
}

type ToolIdempotencyAcquireState string

const (
	ToolIdempotencyAcquired   ToolIdempotencyAcquireState = "acquired"
	ToolIdempotencyReplay     ToolIdempotencyAcquireState = "replay"
	ToolIdempotencyInProgress ToolIdempotencyAcquireState = "in_progress"
)

type ToolIdempotencyAcquireResult struct {
	State          ToolIdempotencyAcquireState
	CachedResponse json.RawMessage
}

func (s *Store) GetToolIdempotency(ctx context.Context, orgID string, toolName string, idempotencyKey string) (ToolIdempotencyRecord, error) {
	var rec ToolIdempotencyRecord
	row := s.q.QueryRowContext(ctx, `
		SELECT org_id::text, tool_name, idempotency_key, status, cached_response, updated_at
		FROM tool_idempotency
		WHERE org_id = $1 AND tool_name = $2 AND idempotency_key = $3
	`, orgID, toolName, idempotencyKey)
	if err := row.Scan(&rec.OrgID, &rec.ToolName, &rec.IdempotencyKey, &rec.Status, &rec.CachedResponse, &rec.UpdatedAt); err != nil {
		return rec, err
	}
	return rec, nil
}

// AcquireToolIdempotency ensures only one in-flight tool execution exists per (org_id, tool_name, idempotency_key).
// It returns:
// - acquired: caller should proceed and later mark succeeded/failed in FinalizeToolExecution
// - replay: tool already succeeded; cached response should be returned without charging
// - in_progress: another execution is in-flight
func (s *Store) AcquireToolIdempotency(ctx context.Context, orgID string, toolName string, idempotencyKey string, now time.Time, staleAfter time.Duration) (ToolIdempotencyAcquireResult, error) {
	if orgID == "" || toolName == "" || idempotencyKey == "" {
		return ToolIdempotencyAcquireResult{}, errors.New("missing tool idempotency key")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if staleAfter <= 0 {
		staleAfter = 15 * time.Minute
	}

	// Fast-path: try to create a new "started" row.
	inserted, err := execRowsAffected(s.q.ExecContext(ctx, `
		INSERT INTO tool_idempotency (org_id, tool_name, idempotency_key, status, updated_at)
		VALUES ($1, $2, $3, 'started', $4)
		ON CONFLICT DO NOTHING
	`, orgID, toolName, idempotencyKey, now))
	if err != nil {
		return ToolIdempotencyAcquireResult{}, err
	}
	if inserted {
		return ToolIdempotencyAcquireResult{State: ToolIdempotencyAcquired}, nil
	}

	// Conflict: inspect the existing row.
	rec, err := s.GetToolIdempotency(ctx, orgID, toolName, idempotencyKey)
	if err != nil {
		return ToolIdempotencyAcquireResult{}, err
	}
	switch rec.Status {
	case "succeeded":
		return ToolIdempotencyAcquireResult{State: ToolIdempotencyReplay, CachedResponse: rec.CachedResponse}, nil
	case "started":
		if rec.UpdatedAt.After(now.Add(-staleAfter)) {
			return ToolIdempotencyAcquireResult{State: ToolIdempotencyInProgress}, nil
		}
		// Stale started row: attempt to recover by re-acquiring.
		acquired, err := execRowsAffected(s.q.ExecContext(ctx, `
			UPDATE tool_idempotency
			SET status = 'started', cached_response = null, updated_at = $4
			WHERE org_id = $1 AND tool_name = $2 AND idempotency_key = $3
			  AND status = 'started'
			  AND updated_at < $5
		`, orgID, toolName, idempotencyKey, now, now.Add(-staleAfter)))
		if err != nil {
			return ToolIdempotencyAcquireResult{}, err
		}
		if acquired {
			return ToolIdempotencyAcquireResult{State: ToolIdempotencyAcquired}, nil
		}
	case "failed":
		acquired, err := execRowsAffected(s.q.ExecContext(ctx, `
			UPDATE tool_idempotency
			SET status = 'started', cached_response = null, updated_at = $4
			WHERE org_id = $1 AND tool_name = $2 AND idempotency_key = $3
			  AND status = 'failed'
		`, orgID, toolName, idempotencyKey, now))
		if err != nil {
			return ToolIdempotencyAcquireResult{}, err
		}
		if acquired {
			return ToolIdempotencyAcquireResult{State: ToolIdempotencyAcquired}, nil
		}
	}

	// Race fallback: re-read and return the most conservative state.
	rec, err = s.GetToolIdempotency(ctx, orgID, toolName, idempotencyKey)
	if err != nil {
		return ToolIdempotencyAcquireResult{}, err
	}
	if rec.Status == "succeeded" {
		return ToolIdempotencyAcquireResult{State: ToolIdempotencyReplay, CachedResponse: rec.CachedResponse}, nil
	}
	return ToolIdempotencyAcquireResult{State: ToolIdempotencyInProgress}, nil
}

func (s *Store) MarkToolIdempotencySucceeded(ctx context.Context, orgID string, toolName string, idempotencyKey string, cachedResponse json.RawMessage, now time.Time) error {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO tool_idempotency (org_id, tool_name, idempotency_key, status, cached_response, updated_at)
		VALUES ($1, $2, $3, 'succeeded', $4, $5)
		ON CONFLICT (org_id, tool_name, idempotency_key) DO UPDATE SET
			status = 'succeeded',
			cached_response = EXCLUDED.cached_response,
			updated_at = EXCLUDED.updated_at
	`, orgID, toolName, idempotencyKey, cachedResponse, now)
	return err
}

func (s *Store) MarkToolIdempotencyFailed(ctx context.Context, orgID string, toolName string, idempotencyKey string, now time.Time) error {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO tool_idempotency (org_id, tool_name, idempotency_key, status, cached_response, updated_at)
		VALUES ($1, $2, $3, 'failed', null, $4)
		ON CONFLICT (org_id, tool_name, idempotency_key) DO UPDATE SET
			status = 'failed',
			cached_response = null,
			updated_at = EXCLUDED.updated_at
	`, orgID, toolName, idempotencyKey, now)
	return err
}

func execRowsAffected(result sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
