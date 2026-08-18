package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"net/mail"
	"strings"
	"time"
)

const (
	meterOutboundReplyDay          = "autonomous_outbound_reply_day"
	meterOutboundSendDay           = "autonomous_outbound_send_day"
	meterOutboundFirstRecipientDay = "autonomous_outbound_first_recipient_day"
	meterOutboundReplyRecipient    = "autonomous_outbound_reply_recipient"
	meterOutboundRecipientSeen     = "autonomous_outbound_recipient_seen"

	limitReplyPerDay           = int64(20)
	limitReplyPerRecipientDay  = int64(5)
	limitSendPerDay            = int64(100)
	limitFirstRecipientsPerDay = int64(25)
)

type OutboundLimitInput struct {
	ToolName       string
	IdempotencyKey string
	Recipient      string
	ComposeEnabled bool
	AcceptedAt     time.Time
}

type OutboundLimitError struct {
	MeterName         string
	RetryAfterSeconds int
}

func (err *OutboundLimitError) Error() string { return "autonomous outbound rate limited" }

// ReserveOutboundLimits runs only for a newly inserted autonomous outbox row.
// The caller's transaction therefore contains the idempotency decision, every
// counter/event reservation, and the enqueue itself; rollback removes all of
// them, while a replay returns before this method and consumes nothing.
func (s *Store) ReserveOutboundLimits(
	ctx context.Context, orgID, outboxID string, input OutboundLimitInput,
) error {
	if err := s.requireTx(); err != nil {
		return err
	}
	if err := s.LockOrgPolicy(ctx, orgID); err != nil {
		return err
	}
	acceptedAt := input.AcceptedAt.UTC()
	if acceptedAt.IsZero() {
		acceptedAt = time.Now().UTC()
	}
	input.AcceptedAt = acceptedAt
	dayStart := acceptedAt.Truncate(24 * time.Hour)
	dayEnd := dayStart.Add(24 * time.Hour)
	canonicalRecipient := canonicalOutboundRecipient(input.Recipient)
	recipientHash := outboundRecipientHash(canonicalRecipient)

	if input.ComposeEnabled {
		if err := s.reserveOutboundBucket(ctx, orgID, input, meterOutboundSendDay, "", dayStart, dayEnd, limitSendPerDay); err != nil {
			return err
		}
		first, err := s.firstOutboundRecipient(ctx, orgID, outboxID, canonicalRecipient, recipientHash)
		if err != nil {
			return err
		}
		if first {
			if err := s.reserveOutboundBucket(ctx, orgID, input, meterOutboundFirstRecipientDay, "", dayStart, dayEnd, limitFirstRecipientsPerDay); err != nil {
				return err
			}
			seenMeter := meterOutboundRecipientSeen + ":" + recipientHash
			if err := s.RecordUsageEventAt(ctx, orgID, seenMeter, 1, input.ToolName,
				UsageReplayID(orgID, input.ToolName, input.IdempotencyKey, meterOutboundRecipientSeen, recipientHash),
				"", "success", acceptedAt); err != nil {
				return err
			}
		}
		return nil
	}

	if err := s.reserveOutboundBucket(ctx, orgID, input, meterOutboundReplyDay, "", dayStart, dayEnd, limitReplyPerDay); err != nil {
		return err
	}
	return s.reserveOutboundBucket(
		ctx, orgID, input, meterOutboundReplyRecipient, recipientHash,
		dayStart, dayEnd, limitReplyPerRecipientDay,
	)
}

func (s *Store) reserveOutboundBucket(
	ctx context.Context, orgID string, input OutboundLimitInput,
	meter, dimension string, periodStart, periodEnd time.Time, limit int64,
) error {
	physicalMeter := meter
	if dimension != "" {
		physicalMeter += ":" + dimension
	}
	if err := s.EnsureOrgUsageCounter(ctx, orgID, physicalMeter, periodStart, periodEnd); err != nil {
		return err
	}
	reserved, _, err := s.ReserveOrgUsageUnits(ctx, orgID, physicalMeter, periodStart, 1, limit)
	if err != nil {
		return err
	}
	if !reserved {
		remaining := periodEnd.Sub(input.AcceptedAt.UTC())
		retryAfter := int((remaining + time.Second - 1) / time.Second)
		if retryAfter < 1 {
			retryAfter = 1
		}
		return &OutboundLimitError{MeterName: meter, RetryAfterSeconds: retryAfter}
	}
	return s.RecordUsageEventAt(
		ctx, orgID, physicalMeter, 1, input.ToolName,
		UsageReplayID(orgID, input.ToolName, input.IdempotencyKey, meter, dimension),
		"", "success", input.AcceptedAt.UTC(),
	)
}

func (s *Store) firstOutboundRecipient(ctx context.Context, orgID, outboxID, canonicalRecipient, recipientHash string) (bool, error) {
	seenMeter := meterOutboundRecipientSeen + ":" + recipientHash
	var seen bool
	if err := s.q.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM usage_events
			WHERE org_id = $1 AND meter_name = $2 AND status = 'success'
		) OR EXISTS (
			SELECT 1 FROM outbox_messages
			WHERE org_id = $1 AND id <> $3::uuid
			  AND lower(btrim("to")) = $4
		)
	`, orgID, seenMeter, outboxID, canonicalRecipient).Scan(&seen); err != nil {
		return false, err
	}
	return !seen, nil
}

func outboundRecipientHash(recipient string) string {
	sum := sha256.Sum256([]byte(recipient))
	return hex.EncodeToString(sum[:])
}

func canonicalOutboundRecipient(recipient string) string {
	recipient = strings.TrimSpace(recipient)
	if parsed, err := mail.ParseAddress(recipient); err == nil {
		recipient = parsed.Address
	}
	return strings.ToLower(strings.TrimSpace(recipient))
}

// UsageReplayID is globally unambiguous across orgs, tools, keys, meters and
// dimensions, matching the V1 durable-usage contract.
func UsageReplayID(orgID, toolName, idempotencyKey, meter, dimension string) string {
	hash := sha256.New()
	for _, part := range []string{"mcp-usage-v1", orgID, toolName, idempotencyKey, meter, dimension} {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(part)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(part))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// DeleteExpiredOutboundUsageCounters bounds the derived bucket table. Durable
// usage_events and outbox history remain untouched and can reconstruct any
// deleted bucket for audit.
func (s *Store) DeleteExpiredOutboundUsageCounters(ctx context.Context, before time.Time) (int, error) {
	result, err := s.q.ExecContext(ctx, `
		DELETE FROM org_usage_counters
		WHERE period_end < $1
		  AND meter_name LIKE 'autonomous_outbound_%'
	`, before)
	if err != nil {
		return 0, err
	}
	deleted, err := result.RowsAffected()
	return int(deleted), err
}
