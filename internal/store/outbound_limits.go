package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/mail"
	"strconv"
	"strings"
	"time"
)

const (
	MeterMCPRequestsPerMinute      = "mcp_requests_per_minute"
	meterOutboundReplyDay          = "autonomous_outbound_reply_day"
	meterOutboundSendDay           = "autonomous_outbound_send_day"
	meterOutboundFirstRecipientDay = "autonomous_outbound_first_recipient_day"
	meterOutboundReplyRecipient    = "autonomous_outbound_reply_recipient"
	meterOutboundRecipientSeen     = "autonomous_outbound_recipient_seen"
	meterOutboundHardBounceDay     = "autonomous_outbound_hard_bounce_day"

	limitReplyPerDay           = int64(20)
	limitReplyPerRecipientDay  = int64(5)
	limitSendPerDay            = int64(100)
	limitFirstRecipientsPerDay = int64(25)
	limitHardBounceAttempts    = int64(20)
	limitHardBounceBasisPoints = int64(500)
	outboundPolicyV2           = "autonomous-outbound-v2"
)

var ErrOutboundPolicyVersionUnavailable = errors.New("outbound policy version unavailable")

type outboundDailyCaps struct {
	sends           int64
	firstRecipients int64
}

type OutboundLimitInput struct {
	ToolName       string
	IdempotencyKey string
	Recipient      string
	ComposeEnabled bool
}

type outboundLimitClock struct {
	acceptedAt        time.Time
	dayStart          time.Time
	dayEnd            time.Time
	retryAfterSeconds int
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
	// Enrolled sources closed for handover cannot mutate abuse counters
	// through a direct Store caller after the monthly outbox fence closes.
	ledgerAvailable, err := s.recipientLedgerAvailable(ctx)
	if err != nil {
		return err
	}
	if ledgerAvailable {
		if _, _, err := s.RecipientAdmissionPeriod(ctx, orgID); err != nil {
			return err
		}
	}
	reservationClock, err := s.readOutboundLimitClock(ctx)
	if err != nil {
		return err
	}
	canonicalRecipient := canonicalOutboundRecipient(input.Recipient)
	recipientHash := outboundRecipientHash(canonicalRecipient)

	if input.ComposeEnabled {
		caps, warmupOrigin, err := s.outboundComposeCaps(ctx, orgID, reservationClock)
		if err != nil {
			return err
		}
		if err := s.reserveOutboundBucket(ctx, orgID, input, meterOutboundSendDay, "", reservationClock, caps.sends); err != nil {
			return err
		}
		first, err := s.firstOutboundRecipient(ctx, orgID, outboxID, canonicalRecipient, recipientHash)
		if err != nil {
			return err
		}
		if first {
			if err := s.reserveOutboundBucket(ctx, orgID, input, meterOutboundFirstRecipientDay, "", reservationClock, caps.firstRecipients); err != nil {
				return err
			}
			seenMeter := meterOutboundRecipientSeen + ":" + recipientHash
			if err := s.RecordUsageEventAt(ctx, orgID, seenMeter, 1, input.ToolName,
				UsageReplayID(orgID, input.ToolName, input.IdempotencyKey, meterOutboundRecipientSeen, recipientHash),
				"", "success", reservationClock.acceptedAt); err != nil {
				return err
			}
		}
		if !warmupOrigin.IsZero() {
			if err := s.persistOutboundWarmupOrigin(ctx, orgID, warmupOrigin); err != nil {
				return err
			}
		}
		return nil
	}

	if err := s.reserveOutboundBucket(ctx, orgID, input, meterOutboundReplyDay, "", reservationClock, limitReplyPerDay); err != nil {
		return err
	}
	return s.reserveOutboundBucket(
		ctx, orgID, input, meterOutboundReplyRecipient, recipientHash,
		reservationClock, limitReplyPerRecipientDay,
	)
}

// The policy marker is projected from an immutable purchased offer. Existing
// subscriptions without it keep v1, even if their legacy tier label matches a
// new tier. The org lock held by ReserveOutboundLimits serializes this read
// with entitlement projection and with the first accepted compose event.
func (s *Store) outboundComposeCaps(ctx context.Context, orgID string, clock outboundLimitClock) (outboundDailyCaps, time.Time, error) {
	v1 := outboundDailyCaps{sends: limitSendPerDay, firstRecipients: limitFirstRecipientsPerDay}
	ent, err := s.GetOrgEntitlement(ctx, orgID)
	if errors.Is(err, sql.ErrNoRows) {
		return v1, time.Time{}, nil
	}
	if err != nil {
		return outboundDailyCaps{}, time.Time{}, err
	}
	var features struct {
		OutboundPolicyVersion string `json:"outbound_policy_version"`
	}
	if err := json.Unmarshal(ent.Features, &features); err != nil {
		return outboundDailyCaps{}, time.Time{}, ErrOutboundPolicyVersionUnavailable
	}
	if features.OutboundPolicyVersion == "" {
		return v1, time.Time{}, nil
	}
	if features.OutboundPolicyVersion != outboundPolicyV2 {
		return outboundDailyCaps{}, time.Time{}, ErrOutboundPolicyVersionUnavailable
	}
	enrolled, err := s.RecipientMeterEnrolled(ctx, orgID)
	if err != nil {
		return outboundDailyCaps{}, time.Time{}, err
	}
	if !enrolled {
		// A projected tier marker without its monthly recipient ledger is a
		// partial billing transition, not permission to use higher daily caps.
		return outboundDailyCaps{}, time.Time{}, ErrOutboundPolicyVersionUnavailable
	}
	origin, err := s.outboundWarmupOrigin(ctx, orgID, clock.acceptedAt)
	if err != nil {
		return outboundDailyCaps{}, time.Time{}, err
	}
	caps, err := effectiveOutboundV2Caps(ent.PlanCode, origin, clock.acceptedAt)
	if err != nil {
		return outboundDailyCaps{}, time.Time{}, err
	}
	return caps, origin, nil
}

// The org policy lock and transaction cover both this read and the eventual
// enqueue. The historical lookup runs only until a successful v2 admission
// persists the origin; a denied or rolled-back send cannot start warmup.
func (s *Store) outboundWarmupOrigin(ctx context.Context, orgID string, acceptedAt time.Time) (time.Time, error) {
	var origin sql.NullTime
	err := s.q.QueryRowContext(ctx, `SELECT first_compose_accepted_at
  FROM org_outbound_policy_state WHERE org_id=$1::uuid FOR UPDATE`, orgID).Scan(&origin)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, ErrOutboundPolicyStateMissing
	}
	if err != nil {
		return time.Time{}, err
	}
	if origin.Valid {
		return origin.Time, nil
	}
	err = s.q.QueryRowContext(ctx, `SELECT created_at FROM usage_events
  WHERE org_id=$1::uuid AND meter_name='autonomous_outbound_send_day' AND status='success'
  ORDER BY created_at LIMIT 1`, orgID).Scan(&origin)
	if errors.Is(err, sql.ErrNoRows) {
		return acceptedAt, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return origin.Time, nil
}

func (s *Store) persistOutboundWarmupOrigin(ctx context.Context, orgID string, origin time.Time) error {
	result, err := s.q.ExecContext(ctx, `UPDATE org_outbound_policy_state
  SET first_compose_accepted_at=$2 WHERE org_id=$1::uuid AND first_compose_accepted_at IS NULL`, orgID, origin)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		var stored time.Time
		if err := s.q.QueryRowContext(ctx, `SELECT first_compose_accepted_at
  FROM org_outbound_policy_state WHERE org_id=$1::uuid`, orgID).Scan(&stored); err != nil {
			return err
		}
		if !stored.Equal(origin) {
			return ErrOutboundPolicyVersionUnavailable
		}
	}
	return nil
}

func effectiveOutboundV2Caps(tier string, firstCompose, now time.Time) (outboundDailyCaps, error) {
	var tierCaps outboundDailyCaps
	switch tier {
	case "starter":
		tierCaps = outboundDailyCaps{100, 25}
	case "growth":
		tierCaps = outboundDailyCaps{500, 125}
	case "scale":
		tierCaps = outboundDailyCaps{1500, 375}
	default:
		return outboundDailyCaps{}, ErrOutboundPolicyVersionUnavailable
	}
	firstDay := firstCompose.UTC().Truncate(24 * time.Hour)
	currentDay := now.UTC().Truncate(24 * time.Hour)
	if currentDay.Before(firstDay) {
		return outboundDailyCaps{}, ErrOutboundPolicyVersionUnavailable
	}
	days := int(currentDay.Sub(firstDay) / (24 * time.Hour))
	warmup := outboundDailyCaps{100, 25}
	if days >= 3 && days < 7 {
		warmup = outboundDailyCaps{500, 125}
	} else if days >= 7 {
		warmup = tierCaps
	}
	return outboundDailyCaps{
		sends:           min(tierCaps.sends, warmup.sends),
		firstRecipients: min(tierCaps.firstRecipients, warmup.firstRecipients),
	}, nil
}

// OutboundPolicyUsage describes the current UTC compose buckets, not an
// authorization to compose. Dispatch still applies owner, generation, abuse
// and suspension fences. Reading this snapshot never starts warmup.
type OutboundPolicyUsage struct {
	Version                 string     `json:"version"`
	DayStart                time.Time  `json:"day_start"`
	DayEnd                  time.Time  `json:"day_end"`
	ComposeSendLimit        int64      `json:"compose_send_limit"`
	ComposeSendUsed         int64      `json:"compose_send_used"`
	ComposeSendRemaining    int64      `json:"compose_send_remaining"`
	FirstRecipientLimit     int64      `json:"first_recipient_limit"`
	FirstRecipientUsed      int64      `json:"first_recipient_used"`
	FirstRecipientRemaining int64      `json:"first_recipient_remaining"`
	WarmupStartedAt         *time.Time `json:"warmup_started_at"`
	NextCapChangeAt         *time.Time `json:"next_cap_change_at"`
}

func (s *Store) ReadOutboundPolicyUsage(ctx context.Context, orgID string) (OutboundPolicyUsage, error) {
	var result OutboundPolicyUsage
	if err := s.requireTx(); err != nil {
		return result, err
	}
	if err := s.LockOrgPolicy(ctx, orgID); err != nil {
		return result, err
	}
	clock, err := s.readOutboundLimitClock(ctx)
	if err != nil {
		return result, err
	}
	caps, origin, err := s.outboundComposeCaps(ctx, orgID, clock)
	if err != nil {
		return result, err
	}
	result.Version = "autonomous-outbound-v1"
	result.DayStart, result.DayEnd = clock.dayStart, clock.dayEnd
	result.ComposeSendLimit, result.FirstRecipientLimit = caps.sends, caps.firstRecipients
	for _, bucket := range []struct {
		meter string
		used  *int64
	}{
		{meterOutboundSendDay, &result.ComposeSendUsed},
		{meterOutboundFirstRecipientDay, &result.FirstRecipientUsed},
	} {
		used, err := s.GetOrgUsageCounterUsed(ctx, orgID, bucket.meter, clock.dayStart)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return OutboundPolicyUsage{}, err
		}
		*bucket.used = used
	}
	result.ComposeSendRemaining = max(int64(0), caps.sends-result.ComposeSendUsed)
	result.FirstRecipientRemaining = max(int64(0), caps.firstRecipients-result.FirstRecipientUsed)
	if origin.IsZero() {
		return result, nil
	}
	result.Version = outboundPolicyV2
	// outboundComposeCaps uses today's clock for a first prospective send.
	// Only durable state or an actual successful historical compose may be
	// shown as a started warmup; no read writes that first-send evidence.
	var started sql.NullTime
	err = s.q.QueryRowContext(ctx, `SELECT coalesce(first_compose_accepted_at,
  (SELECT created_at FROM usage_events WHERE org_id=$1::uuid
    AND meter_name='autonomous_outbound_send_day' AND status='success'
    ORDER BY created_at LIMIT 1))
  FROM org_outbound_policy_state WHERE org_id=$1::uuid`, orgID).Scan(&started)
	if err != nil {
		return OutboundPolicyUsage{}, err
	}
	if started.Valid {
		at := started.Time.UTC()
		result.WarmupStartedAt = &at
		ent, err := s.GetOrgEntitlement(ctx, orgID)
		if err != nil {
			return OutboundPolicyUsage{}, err
		}
		result.NextCapChangeAt = nextOutboundCapChange(ent.PlanCode, at, clock.acceptedAt)
	}
	return result, nil
}

func nextOutboundCapChange(tier string, firstCompose, now time.Time) *time.Time {
	day := firstCompose.UTC().Truncate(24 * time.Hour)
	var next time.Time
	if (tier == "growth" || tier == "scale") && now.Before(day.Add(3*24*time.Hour)) {
		next = day.Add(3 * 24 * time.Hour)
	} else if tier == "scale" && now.Before(day.Add(7*24*time.Hour)) {
		next = day.Add(7 * 24 * time.Hour)
	}
	if next.IsZero() {
		return nil
	}
	return &next
}

func (s *Store) readOutboundLimitClock(ctx context.Context) (outboundLimitClock, error) {
	var result outboundLimitClock
	err := s.q.QueryRowContext(ctx, `
		WITH db_clock AS (
			SELECT clock_timestamp() AS accepted_at
		), bucket AS (
			SELECT
				accepted_at,
				date_trunc('day', accepted_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AS day_start
			FROM db_clock
		)
		SELECT
			accepted_at,
			day_start,
			day_start + interval '1 day',
			greatest(1, ceil(extract(epoch FROM (day_start + interval '1 day' - accepted_at))))::integer
		FROM bucket
	`).Scan(&result.acceptedAt, &result.dayStart, &result.dayEnd, &result.retryAfterSeconds)
	return result, err
}

func (s *Store) reserveOutboundBucket(
	ctx context.Context, orgID string, input OutboundLimitInput,
	meter, dimension string, reservationClock outboundLimitClock, limit int64,
) error {
	physicalMeter := meter
	if dimension != "" {
		physicalMeter += ":" + dimension
	}
	if err := s.EnsureOrgUsageCounter(ctx, orgID, physicalMeter, reservationClock.dayStart, reservationClock.dayEnd); err != nil {
		return err
	}
	reserved, _, err := s.ReserveOrgUsageUnits(ctx, orgID, physicalMeter, reservationClock.dayStart, 1, limit)
	if err != nil {
		return err
	}
	if !reserved {
		return &OutboundLimitError{MeterName: meter, RetryAfterSeconds: reservationClock.retryAfterSeconds}
	}
	return s.RecordUsageEventAt(
		ctx, orgID, physicalMeter, 1, input.ToolName,
		UsageReplayID(orgID, input.ToolName, input.IdempotencyKey, meter, dimension),
		"", "success", reservationClock.acceptedAt,
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

// OutboundIdempotencyKey scopes the caller's tool-idempotency key before it
// reaches the outbox and transport layers. Tool idempotency is defined over
// (org, tool, key), so the outbox must not collapse equal caller keys from two
// different tools into one parent row before their usage reservations run.
func OutboundIdempotencyKey(toolName, idempotencyKey string) string {
	return "mcp-outbox-v1:" + strconv.Itoa(len(toolName)) + ":" + toolName + ":" + idempotencyKey
}

// DeleteExpiredOutboundUsageCounters bounds both autonomous send buckets and
// the durable MCP RPM buckets. Durable usage_events and outbox history remain
// untouched and can reconstruct any deleted bucket for audit.
func (s *Store) DeleteExpiredOutboundUsageCounters(ctx context.Context, before time.Time) (int, error) {
	result, err := s.q.ExecContext(ctx, `
		DELETE FROM org_usage_counters
		WHERE period_end < $1
		  AND (
			meter_name LIKE 'autonomous_outbound_%'
			OR meter_name = $2
		  )
	`, before, MeterMCPRequestsPerMinute)
	if err != nil {
		return 0, err
	}
	deleted, err := result.RowsAffected()
	return int(deleted), err
}
