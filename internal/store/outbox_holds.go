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

const (
	MinOutboxDeliveryHoldTTL = time.Minute
	MaxOutboxDeliveryHoldTTL = 30 * time.Minute
)

type OutboxDeliveryHold struct {
	ID              string
	OrgID           string
	IdempotencyKey  string
	Reason          string
	HeldBy          string
	HoldReplayID    string
	CreatedAt       time.Time
	ExpiresAt       time.Time
	ReleasedAt      *time.Time
	ReleasedBy      *string
	ReleaseReplayID *string
}

func validateOutboxDeliveryHoldInput(orgID, idempotencyKey, reason, actor string) error {
	if _, err := uuid.Parse(strings.TrimSpace(orgID)); err != nil {
		return fmt.Errorf("invalid hold org id: %w", err)
	}
	if strings.TrimSpace(idempotencyKey) == "" || len(idempotencyKey) > 255 {
		return errors.New("outbox hold idempotency key must contain 1-255 bytes")
	}
	if strings.TrimSpace(reason) == "" || len(reason) > 255 {
		return errors.New("outbox hold reason must contain 1-255 bytes")
	}
	if strings.TrimSpace(actor) == "" || len(actor) > 255 {
		return errors.New("outbox hold actor must contain 1-255 bytes")
	}
	return nil
}

func (s *Store) CreateOutboxDeliveryHoldAudited(
	ctx context.Context,
	orgID, idempotencyKey, reason, actor string,
	ttl time.Duration,
) (OutboxDeliveryHold, bool, string, error) {
	orgID = strings.TrimSpace(orgID)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	reason = strings.TrimSpace(reason)
	actor = strings.TrimSpace(actor)
	if err := validateOutboxDeliveryHoldInput(orgID, idempotencyKey, reason, actor); err != nil {
		return OutboxDeliveryHold{}, false, "", err
	}
	if ttl < MinOutboxDeliveryHoldTTL || ttl > MaxOutboxDeliveryHoldTTL {
		return OutboxDeliveryHold{}, false, "", fmt.Errorf(
			"outbox hold ttl must be between %s and %s", MinOutboxDeliveryHoldTTL, MaxOutboxDeliveryHoldTTL,
		)
	}

	now := time.Now().UTC()
	holdReplayID := uuid.NewString()
	var hold OutboxDeliveryHold
	var changed bool
	err := s.withTx(ctx, func(scoped *Store) error {
		if _, err := scoped.q.ExecContext(ctx, `
			SELECT pg_advisory_xact_lock(hashtextextended($1, 0))
		`, orgID+":"+idempotencyKey); err != nil {
			return err
		}
		if _, err := scoped.q.ExecContext(ctx, `
			UPDATE outbox_delivery_holds
			SET released_at = $3, released_by = 'system:expired', release_replay_id = $4::uuid
			WHERE org_id = $1::uuid AND idempotency_key = $2
			  AND released_at IS NULL AND expires_at <= $3
		`, orgID, idempotencyKey, now, uuid.NewString()); err != nil {
			return err
		}

		row := scoped.q.QueryRowContext(ctx, `
			SELECT id::text, org_id::text, idempotency_key, reason, held_by,
			       hold_replay_id::text, created_at, expires_at
			FROM outbox_delivery_holds
			WHERE org_id = $1::uuid AND idempotency_key = $2 AND released_at IS NULL
			FOR UPDATE
		`, orgID, idempotencyKey)
		if err := row.Scan(
			&hold.ID, &hold.OrgID, &hold.IdempotencyKey, &hold.Reason, &hold.HeldBy,
			&hold.HoldReplayID, &hold.CreatedAt, &hold.ExpiresAt,
		); err == nil {
			return scoped.auditOutboxDeliveryHold(ctx, actor, "hold", orgID, idempotencyKey, reason, false, holdReplayID)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		hold = OutboxDeliveryHold{
			ID: uuid.NewString(), OrgID: orgID, IdempotencyKey: idempotencyKey,
			Reason: reason, HeldBy: actor, HoldReplayID: holdReplayID,
			CreatedAt: now, ExpiresAt: now.Add(ttl),
		}
		if _, err := scoped.q.ExecContext(ctx, `
			INSERT INTO outbox_delivery_holds (
			  id, org_id, idempotency_key, reason, held_by, hold_replay_id,
			  created_at, expires_at
			) VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6::uuid, $7, $8)
		`, hold.ID, hold.OrgID, hold.IdempotencyKey, hold.Reason, hold.HeldBy,
			hold.HoldReplayID, hold.CreatedAt, hold.ExpiresAt); err != nil {
			return err
		}
		changed = true
		return scoped.auditOutboxDeliveryHold(ctx, actor, "hold", orgID, idempotencyKey, reason, true, holdReplayID)
	})
	if err != nil {
		return OutboxDeliveryHold{}, false, "", err
	}
	return hold, changed, holdReplayID, nil
}

func (s *Store) ReleaseOutboxDeliveryHoldAudited(
	ctx context.Context,
	orgID, idempotencyKey, actor string,
) (OutboxDeliveryHold, bool, string, error) {
	orgID = strings.TrimSpace(orgID)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	actor = strings.TrimSpace(actor)
	if err := validateOutboxDeliveryHoldInput(orgID, idempotencyKey, "release", actor); err != nil {
		return OutboxDeliveryHold{}, false, "", err
	}

	now := time.Now().UTC()
	replayID := uuid.NewString()
	var hold OutboxDeliveryHold
	var changed bool
	err := s.withTx(ctx, func(scoped *Store) error {
		if _, err := scoped.q.ExecContext(ctx, `
			SELECT pg_advisory_xact_lock(hashtextextended($1, 0))
		`, orgID+":"+idempotencyKey); err != nil {
			return err
		}
		row := scoped.q.QueryRowContext(ctx, `
			UPDATE outbox_delivery_holds
			SET released_at = $3, released_by = $4, release_replay_id = $5::uuid
			WHERE org_id = $1::uuid AND idempotency_key = $2 AND released_at IS NULL
			RETURNING id::text, org_id::text, idempotency_key, reason, held_by,
			          hold_replay_id::text, created_at, expires_at,
			          released_at, released_by, release_replay_id::text
		`, orgID, idempotencyKey, now, actor, replayID)
		if err := scanOutboxDeliveryHold(row, &hold); err == nil {
			changed = true
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return scoped.auditOutboxDeliveryHold(ctx, actor, "release", orgID, idempotencyKey, "", changed, replayID)
	})
	if err != nil {
		return OutboxDeliveryHold{}, false, "", err
	}
	return hold, changed, replayID, nil
}

func (s *Store) LatestOutboxDeliveryHold(ctx context.Context, orgID, idempotencyKey string) (OutboxDeliveryHold, error) {
	orgID = strings.TrimSpace(orgID)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if _, err := uuid.Parse(orgID); err != nil || idempotencyKey == "" {
		return OutboxDeliveryHold{}, errors.New("invalid outbox hold lookup")
	}
	var hold OutboxDeliveryHold
	row := s.q.QueryRowContext(ctx, `
		SELECT id::text, org_id::text, idempotency_key, reason, held_by,
		       hold_replay_id::text, created_at, expires_at,
		       released_at, released_by, release_replay_id::text
		FROM outbox_delivery_holds
		WHERE org_id = $1::uuid AND idempotency_key = $2
		ORDER BY created_at DESC, id DESC
		LIMIT 1
	`, orgID, idempotencyKey)
	if err := scanOutboxDeliveryHold(row, &hold); err != nil {
		return OutboxDeliveryHold{}, err
	}
	return hold, nil
}

type rowScanner interface {
	Scan(...any) error
}

func scanOutboxDeliveryHold(row rowScanner, hold *OutboxDeliveryHold) error {
	return row.Scan(
		&hold.ID, &hold.OrgID, &hold.IdempotencyKey, &hold.Reason, &hold.HeldBy,
		&hold.HoldReplayID, &hold.CreatedAt, &hold.ExpiresAt,
		&hold.ReleasedAt, &hold.ReleasedBy, &hold.ReleaseReplayID,
	)
}

func (s *Store) auditOutboxDeliveryHold(
	ctx context.Context,
	actor, action, orgID, idempotencyKey, reason string,
	changed bool,
	replayID string,
) error {
	inputsHash, err := outboxDeliveryHoldAuditHash(map[string]any{
		"action": action, "org_id": orgID, "idempotency_key": idempotencyKey, "reason": reason,
	})
	if err != nil {
		return err
	}
	outputsHash, err := outboxDeliveryHoldAuditHash(map[string]any{"changed": changed})
	if err != nil {
		return err
	}
	_, err = s.q.ExecContext(ctx, `
		INSERT INTO audit_log (tool_call_id, actor, inputs_hash, outputs_hash, replay_id)
		VALUES (NULL, $1, $2, $3, $4)
	`, actor, inputsHash, outputsHash, replayID)
	return err
}

func outboxDeliveryHoldAuditHash(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
