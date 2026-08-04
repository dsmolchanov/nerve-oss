package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

type FeatureFlagValues struct {
	Org    *bool
	Global *bool
}

type FeatureFlag struct {
	ID        string
	OrgID     sql.NullString
	Flag      string
	Enabled   bool
	UpdatedAt time.Time
	UpdatedBy string
}

func (s *Store) LookupFeatureFlagForOrg(ctx context.Context, orgID string, flag string) (FeatureFlagValues, error) {
	var values FeatureFlagValues
	err := s.RunAsOrg(ctx, orgID, func(scoped *Store) error {
		resolved, err := scoped.LookupFeatureFlag(ctx, orgID, flag)
		values = resolved
		return err
	})
	return values, err
}

// LookupFeatureFlag returns the org-specific and global values separately so
// callers can apply precedence without losing the distinction between an
// explicit false and an absent row.
func (s *Store) LookupFeatureFlag(ctx context.Context, orgID string, flag string) (FeatureFlagValues, error) {
	orgID = strings.TrimSpace(orgID)
	flag = strings.TrimSpace(flag)
	if orgID == "" || flag == "" {
		return FeatureFlagValues{}, errors.New("missing org id or feature flag")
	}

	rows, err := s.q.QueryContext(ctx, `
		SELECT org_id, enabled
		FROM org_feature_flags
		WHERE flag = $1
		  AND (org_id = $2::uuid OR org_id IS NULL)
	`, flag, orgID)
	if err != nil {
		return FeatureFlagValues{}, err
	}
	defer rows.Close()

	var values FeatureFlagValues
	for rows.Next() {
		var rowOrgID sql.NullString
		var enabled bool
		if err := rows.Scan(&rowOrgID, &enabled); err != nil {
			return FeatureFlagValues{}, err
		}
		value := enabled
		if rowOrgID.Valid {
			values.Org = &value
		} else {
			values.Global = &value
		}
	}
	if err := rows.Err(); err != nil {
		return FeatureFlagValues{}, err
	}
	return values, nil
}

// SetFeatureFlag is idempotent: setting an existing scope to its current
// value performs no update and reports changed=false.
func (s *Store) SetFeatureFlag(ctx context.Context, orgID *string, flag string, enabled bool, updatedBy string) (bool, error) {
	flag = strings.TrimSpace(flag)
	updatedBy = strings.TrimSpace(updatedBy)
	if flag == "" || updatedBy == "" {
		return false, errors.New("missing feature flag or updated_by")
	}

	var (
		result sql.Result
		err    error
	)
	if orgID == nil {
		result, err = s.q.ExecContext(ctx, `
			INSERT INTO org_feature_flags (org_id, flag, enabled, updated_by)
			VALUES (NULL, $1, $2, $3)
			ON CONFLICT (flag) WHERE org_id IS NULL DO UPDATE
			SET enabled = EXCLUDED.enabled,
			    updated_at = now(),
			    updated_by = EXCLUDED.updated_by
			WHERE org_feature_flags.enabled IS DISTINCT FROM EXCLUDED.enabled
		`, flag, enabled, updatedBy)
	} else {
		trimmedOrgID := strings.TrimSpace(*orgID)
		if trimmedOrgID == "" {
			return false, errors.New("empty org id")
		}
		result, err = s.q.ExecContext(ctx, `
			INSERT INTO org_feature_flags (org_id, flag, enabled, updated_by)
			VALUES ($1::uuid, $2, $3, $4)
			ON CONFLICT (org_id, flag) WHERE org_id IS NOT NULL DO UPDATE
			SET enabled = EXCLUDED.enabled,
			    updated_at = now(),
			    updated_by = EXCLUDED.updated_by
			WHERE org_feature_flags.enabled IS DISTINCT FROM EXCLUDED.enabled
		`, trimmedOrgID, flag, enabled, updatedBy)
	}
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed > 0, err
}

func (s *Store) ListFeatureFlags(ctx context.Context, orgID *string) ([]FeatureFlag, error) {
	query := `
		SELECT id, org_id, flag, enabled, updated_at, updated_by
		FROM org_feature_flags
	`
	var args []any
	if orgID == nil {
		query += ` WHERE org_id IS NULL`
	} else {
		trimmedOrgID := strings.TrimSpace(*orgID)
		if trimmedOrgID == "" {
			return nil, errors.New("empty org id")
		}
		query += ` WHERE org_id = $1::uuid`
		args = append(args, trimmedOrgID)
	}
	query += ` ORDER BY flag`

	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	flags := make([]FeatureFlag, 0)
	for rows.Next() {
		var flag FeatureFlag
		if err := rows.Scan(&flag.ID, &flag.OrgID, &flag.Flag, &flag.Enabled, &flag.UpdatedAt, &flag.UpdatedBy); err != nil {
			return nil, err
		}
		flags = append(flags, flag)
	}
	return flags, rows.Err()
}
