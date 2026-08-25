package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
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

var ErrOutboundPolicyStateMissing = errors.New("outbound policy state missing")

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
// OutboundPolicyFlags are the org flags that decide whether an autonomous
// sender may enqueue. Readers and writers take the same per-org lock so a
// suspension cannot commit between an enqueue's policy read and its insert.
var OutboundPolicyFlags = map[string]bool{
	"autonomous_outbound_policy": true,
	"email_outbound_suspended":   true,
	"email_compose_org_enabled":  true,
}

// outboundDeliveryFenceFlags revoke or restore all autonomous delivery. A
// compose-only transition still takes LockOrgPolicy (it participates in the
// enqueue snapshot) but must not advance the shared epoch: outbox rows do not
// retain enough policy context to distinguish compose from reply.
var outboundDeliveryFenceFlags = map[string]bool{
	"autonomous_outbound_policy": true,
	"email_outbound_suspended":   true,
}

// FenceOrgPolicy runs fn in a transaction holding the org policy lock, so a
// write to evidence the outbound policy reads cannot land between an enqueue's
// check and its insert. Safe to call from inside an existing transaction.
func (s *Store) FenceOrgPolicy(ctx context.Context, orgID string, fn func(*Store) error) error {
	return s.withTx(ctx, func(scoped *Store) error {
		if err := scoped.LockOrgPolicy(ctx, orgID); err != nil {
			return err
		}
		return fn(scoped)
	})
}

// LockOrgPolicy serializes this transaction against concurrent policy writes
// for one org. It is transaction-scoped, so it must be called inside a
// transaction and is released on commit or rollback.
func (s *Store) LockOrgPolicy(ctx context.Context, orgID string) error {
	orgID = strings.TrimSpace(orgID)
	if orgID == "" {
		return errors.New("missing org id")
	}
	if err := s.requireTx(); err != nil {
		return fmt.Errorf("org policy lock: %w", err)
	}
	_, err := s.q.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "org-policy:"+orgID)
	return err
}

// EnsureOutboundPolicyState creates the first autonomous policy epoch. It is
// intentionally transaction-only so onboarding can seed it atomically with
// the org graph and explicit policy flags.
func (s *Store) EnsureOutboundPolicyState(ctx context.Context, orgID string) (int64, error) {
	if err := s.requireOutboundFence("EnsureOutboundPolicyState"); err != nil {
		return 0, err
	}
	orgID = strings.TrimSpace(orgID)
	if orgID == "" {
		return 0, errors.New("missing org id")
	}
	if err := s.requireTx(); err != nil {
		return 0, fmt.Errorf("ensure outbound policy state: %w", err)
	}
	if err := s.LockOrgPolicy(ctx, orgID); err != nil {
		return 0, err
	}
	var epoch int64
	err := s.q.QueryRowContext(ctx, `
		INSERT INTO org_outbound_policy_state (org_id, policy_epoch)
		VALUES ($1::uuid, 1)
		ON CONFLICT (org_id) DO UPDATE
		SET org_id = EXCLUDED.org_id
		RETURNING policy_epoch
	`, orgID).Scan(&epoch)
	return epoch, err
}

// CurrentOutboundPolicyEpoch reads the row while the caller holds the
// transaction-scoped org policy lock. Absence fails closed for autonomous
// senders rather than silently treating the org as legacy.
func (s *Store) CurrentOutboundPolicyEpoch(ctx context.Context, orgID string) (int64, error) {
	if err := s.requireOutboundFence("CurrentOutboundPolicyEpoch"); err != nil {
		return 0, err
	}
	orgID = strings.TrimSpace(orgID)
	if orgID == "" {
		return 0, errors.New("missing org id")
	}
	if err := s.requireTx(); err != nil {
		return 0, fmt.Errorf("read outbound policy epoch: %w", err)
	}
	var epoch int64
	err := s.q.QueryRowContext(ctx, `
		SELECT policy_epoch
		FROM org_outbound_policy_state
		WHERE org_id = $1::uuid
		FOR UPDATE
	`, orgID).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrOutboundPolicyStateMissing
	}
	return epoch, err
}

// AdvanceOutboundPolicyEpoch fences every queued autonomous row from the old
// epoch in the same transaction as the caller's policy transition. The
// existing failed status is retained; policy_revoked is the bounded reason.
func (s *Store) AdvanceOutboundPolicyEpoch(ctx context.Context, orgID string) (epoch int64, terminalized int64, err error) {
	if ferr := s.requireOutboundFence("AdvanceOutboundPolicyEpoch"); ferr != nil {
		return 0, 0, ferr
	}
	orgID = strings.TrimSpace(orgID)
	if orgID == "" {
		return 0, 0, errors.New("missing org id")
	}
	if err := s.requireTx(); err != nil {
		return 0, 0, fmt.Errorf("advance outbound policy epoch: %w", err)
	}
	if err := s.LockOrgPolicy(ctx, orgID); err != nil {
		return 0, 0, err
	}
	if err := s.q.QueryRowContext(ctx, `
		UPDATE org_outbound_policy_state
		SET policy_epoch = policy_epoch + 1,
		    updated_at = now()
		WHERE org_id = $1::uuid
		RETURNING policy_epoch
	`, orgID).Scan(&epoch); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, ErrOutboundPolicyStateMissing
		}
		return 0, 0, err
	}
	result, err := s.q.ExecContext(ctx, `
		UPDATE outbox_messages
		SET status = 'failed',
		    last_error = 'policy_revoked',
		    locked_at = NULL,
		    locked_by = NULL,
		    terminal_at = now()
		WHERE org_id = $1::uuid
		  AND autonomous_policy_epoch IS NOT NULL
		  AND autonomous_policy_epoch < $2
		  AND status IN ('queued', 'sending')
		  AND NOT (provider_started_at IS NOT NULL AND provider_resolved_at IS NULL)
	`, orgID, epoch)
	if err != nil {
		return 0, 0, err
	}
	terminalized, err = result.RowsAffected()
	return epoch, terminalized, err
}

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
	if orgID != nil && OutboundPolicyFlags[flag] {
		// Writers take the reader's lock so an enqueue in flight either sees
		// this change or blocks until it commits, never lands between the two.
		if err := s.LockOrgPolicy(ctx, *orgID); err != nil {
			return false, err
		}
	}
	if orgID == nil && flag == "domain_writes" {
		if err := s.requireTx(); err != nil {
			return false, fmt.Errorf("domain_writes fence mutation: %w", err)
		}
		if _, err := s.q.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('domain-writes-fence', 0))`); err != nil {
			return false, err
		}
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
	if err != nil || changed == 0 || orgID == nil || !outboundDeliveryFenceFlags[flag] {
		return changed > 0, err
	}
	// Legacy organizations have no epoch row and retain their existing flag
	// behavior. Every real autonomous policy change advances the fence in the
	// same transaction, so suspended work can never revive after a later clear.
	// Before Core 0029 the policy-state table does not exist and no org can
	// carry an epoch, so the probe itself would be an unsupported-schema query.
	if !s.OutboundFenceEnabled() {
		return true, nil
	}
	var hasPolicyState bool
	if err := s.q.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM org_outbound_policy_state WHERE org_id = $1::uuid
		)
	`, strings.TrimSpace(*orgID)).Scan(&hasPolicyState); err != nil {
		return false, err
	}
	if hasPolicyState {
		if _, _, err := s.AdvanceOutboundPolicyEpoch(ctx, strings.TrimSpace(*orgID)); err != nil {
			return false, err
		}
	}
	return true, nil
}

// SetFeatureFlagAudited atomically applies an idempotent flag write and
// records the operator action. A repeated value leaves the flag row unchanged
// but still records that the command was issued.
func (s *Store) SetFeatureFlagAudited(ctx context.Context, orgID *string, flag string, enabled bool, actor string) (bool, string, error) {
	replayID := uuid.NewString()
	var changed bool
	err := s.withTx(ctx, func(scoped *Store) error {
		var err error
		changed, err = scoped.SetFeatureFlag(ctx, orgID, flag, enabled, actor)
		if err != nil {
			return err
		}
		inputsHash, err := featureFlagAuditHash(map[string]any{
			"org_id":  orgID,
			"flag":    strings.TrimSpace(flag),
			"enabled": enabled,
		})
		if err != nil {
			return err
		}
		outputsHash, err := featureFlagAuditHash(map[string]any{"changed": changed})
		if err != nil {
			return err
		}
		_, err = scoped.q.ExecContext(ctx, `
			INSERT INTO audit_log (tool_call_id, actor, inputs_hash, outputs_hash, replay_id)
			VALUES (NULL, $1, $2, $3, $4)
		`, strings.TrimSpace(actor), inputsHash, outputsHash, replayID)
		return err
	})
	if err != nil {
		return false, "", err
	}
	return changed, replayID, nil
}

func featureFlagAuditHash(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
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
