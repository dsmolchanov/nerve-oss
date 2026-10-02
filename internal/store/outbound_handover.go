package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

var ErrOutboundHandoverConflict = errors.New("outbound policy history handover unavailable or conflicting")

const (
	outboundHandoverPageSize      = 256
	outboundHandoverMaxBuckets    = 1024
	outboundHandoverFreezeMeter   = "autonomous_outbound_handover_freeze"
	outboundHandoverSnapshotMeter = "autonomous_outbound_handover_snapshot"
	outboundHandoverCompleteMeter = "autonomous_outbound_handover_complete"
)

type OutboundPolicyHandoverResult struct {
	SourcePolicyEpoch     int64
	FrozenAt              time.Time
	DayStart              time.Time
	SnapshotSHA256        string
	RecipientHashesCopied int64
}

type outboundHandoverBucket struct {
	Meter string
	Used  int64
}
type outboundHandoverSnapshot struct {
	SourceOrg    string
	TransferID   string
	Epoch        int64
	FrozenAt     time.Time
	DayStart     time.Time
	WarmupOrigin sql.NullTime
	Buckets      []outboundHandoverBucket
}

// TransferOutboundPolicyHistory is a maintenance primitive, never grant
// authority. Its caller must already authorize the exact old/new generations
// and hold their lifecycle/apex/allocation locks before entering Core. Core
// knows no Cloud pool or callback. The target remains enrolled without a live
// recipient period; a new grant must be committed in this same transaction.
// Both policy locks, the freeze, bounded history pages and enforced counters
// share that transaction. Any error aborts it, even if a caller handles it.
func (s *Store) TransferOutboundPolicyHistory(ctx context.Context, sourceOrg, targetOrg, transferID string) (result OutboundPolicyHandoverResult, err error) {
	if err = s.requireTx(); err != nil {
		return result, err
	}
	rollback, ok := s.q.(interface{ Rollback() error })
	if !ok {
		return result, ErrOutboundHandoverConflict
	}
	defer func() {
		if err != nil {
			_ = rollback.Rollback()
			result = OutboundPolicyHandoverResult{}
		}
	}()
	if err = recipientIDs(sourceOrg, targetOrg); err != nil {
		return result, err
	}
	if err = recipientIDs(sourceOrg, transferID); err != nil {
		return result, err
	}
	if sourceOrg == targetOrg {
		return result, ErrOutboundHandoverConflict
	}
	orgs := []string{sourceOrg, targetOrg}
	sort.Strings(orgs)
	for _, org := range orgs {
		if err = s.LockOrgPolicy(ctx, org); err != nil {
			return result, err
		}
	}
	var snapshot outboundHandoverSnapshot
	err = s.RunAsOrg(ctx, sourceOrg, func(tx *Store) error {
		var inner error
		snapshot, inner = tx.freezeOutboundHandover(ctx, sourceOrg, transferID)
		return inner
	})
	if err != nil {
		return result, err
	}
	encoded, marshalErr := json.Marshal(snapshot)
	if marshalErr != nil {
		return result, marshalErr
	}
	digest := sha256.Sum256(encoded)
	result = OutboundPolicyHandoverResult{SourcePolicyEpoch: snapshot.Epoch, FrozenAt: snapshot.FrozenAt, DayStart: snapshot.DayStart, SnapshotSHA256: hex.EncodeToString(digest[:])}
	tag := "policy_handover:" + sourceOrg + ":" + strconv.FormatInt(snapshot.Epoch, 10) + ":" + result.SnapshotSHA256
	var complete bool
	err = s.RunAsOrg(ctx, targetOrg, func(tx *Store) error {
		var count int64
		var inner error
		complete, count, inner = tx.prepareOutboundHandoverTarget(ctx, targetOrg, transferID, tag, snapshot)
		result.RecipientHashesCopied = count
		return inner
	})
	if err != nil || complete {
		return result, err
	}
	cursor := ""
	for {
		var hashes []string
		err = s.RunAsOrg(ctx, sourceOrg, func(tx *Store) error {
			var inner error
			hashes, inner = tx.exportOutboundHandoverHashes(ctx, sourceOrg, cursor)
			return inner
		})
		if err != nil {
			return result, err
		}
		if len(hashes) == 0 {
			break
		}
		err = s.RunAsOrg(ctx, targetOrg, func(tx *Store) error {
			return tx.installOutboundHandoverHashes(ctx, targetOrg, transferID, tag, snapshot.FrozenAt, hashes)
		})
		if err != nil {
			return result, err
		}
		result.RecipientHashesCopied += int64(len(hashes))
		if result.RecipientHashesCopied >= math.MaxInt32 {
			return result, ErrOutboundHandoverConflict
		}
		cursor = hashes[len(hashes)-1]
	}
	err = s.RunAsOrg(ctx, targetOrg, func(tx *Store) error {
		return tx.ensureOutboundHandoverEvent(ctx, targetOrg, transferID, tag, outboundHandoverCompleteMeter, result.RecipientHashesCopied+1, snapshot.FrozenAt)
	})
	return result, err
}

func (s *Store) freezeOutboundHandover(ctx context.Context, org, transfer string) (snapshot outboundHandoverSnapshot, err error) {
	snapshot.SourceOrg, snapshot.TransferID = org, transfer
	enrolled, err := s.RecipientMeterEnrolled(ctx, org)
	if err != nil {
		return snapshot, err
	}
	if !enrolled {
		return snapshot, ErrOutboundHandoverConflict
	}
	key := UsageReplayID(org, "policy_handover", transfer, outboundHandoverFreezeMeter, "")
	var meter string
	err = s.q.QueryRowContext(ctx, `SELECT meter_name,created_at FROM usage_events WHERE org_id=$1::uuid AND replay_id=$2 AND tool_name='policy_handover_freeze' AND status='success' AND quantity=1`, org, key).Scan(&meter, &snapshot.FrozenAt)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err = s.CurrentOutboundPolicyEpoch(ctx, org); err != nil {
			return snapshot, err
		}
		if _, err = s.q.ExecContext(ctx, `UPDATE org_recipient_periods SET admission_closed=true WHERE org_id=$1::uuid AND NOT admission_closed`, org); err != nil {
			return snapshot, err
		}
		changed, flagErr := s.SetFeatureFlag(ctx, &org, "email_outbound_suspended", true, "policy-handover")
		if flagErr != nil {
			return snapshot, flagErr
		}
		if !changed {
			if _, _, err = s.AdvanceOutboundPolicyEpoch(ctx, org); err != nil {
				return snapshot, err
			}
		}
		if snapshot.Epoch, err = s.CurrentOutboundPolicyEpoch(ctx, org); err != nil {
			return snapshot, err
		}
		clock, clockErr := s.readOutboundLimitClock(ctx)
		if clockErr != nil {
			return snapshot, clockErr
		}
		snapshot.FrozenAt = clock.acceptedAt
		meter = outboundHandoverFreezeMeter + ":" + strconv.FormatInt(snapshot.Epoch, 10)
		if err = s.RecordUsageEventAt(ctx, org, meter, 1, "policy_handover_freeze", key, "", "success", snapshot.FrozenAt); err != nil {
			return snapshot, err
		}
	} else if err != nil {
		return snapshot, err
	} else {
		if !strings.HasPrefix(meter, outboundHandoverFreezeMeter+":") {
			return snapshot, ErrOutboundHandoverConflict
		}
		snapshot.Epoch, err = strconv.ParseInt(strings.TrimPrefix(meter, outboundHandoverFreezeMeter+":"), 10, 64)
		if err != nil || snapshot.Epoch <= 0 {
			return snapshot, ErrOutboundHandoverConflict
		}
	}
	epoch, err := s.CurrentOutboundPolicyEpoch(ctx, org)
	if err != nil {
		return snapshot, err
	}
	flags, err := s.LookupFeatureFlag(ctx, org, "email_outbound_suspended")
	if err != nil {
		return snapshot, err
	}
	var live bool
	if err = s.q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM org_recipient_periods WHERE org_id=$1::uuid AND NOT admission_closed)`, org).Scan(&live); err != nil {
		return snapshot, err
	}
	if epoch != snapshot.Epoch || flags.Org == nil || !*flags.Org || live {
		return snapshot, ErrOutboundHandoverConflict
	}
	snapshot.FrozenAt = snapshot.FrozenAt.UTC()
	snapshot.DayStart = snapshot.FrozenAt.Truncate(24 * time.Hour)
	// The timestamp is actual accepted history, including v1, never the time
	// a new grant was issued. An absent compose history starts no warmup.
	if err = s.q.QueryRowContext(ctx, `SELECT least(first_compose_accepted_at,(SELECT min(created_at) FROM usage_events WHERE org_id=$1::uuid AND meter_name=$2 AND status='success')) FROM org_outbound_policy_state WHERE org_id=$1::uuid`, org, meterOutboundSendDay).Scan(&snapshot.WarmupOrigin); err != nil {
		return snapshot, err
	}
	if snapshot.WarmupOrigin.Valid {
		snapshot.WarmupOrigin.Time = snapshot.WarmupOrigin.Time.UTC()
	}
	if snapshot.WarmupOrigin.Valid && snapshot.WarmupOrigin.Time.After(snapshot.FrozenAt) {
		return snapshot, ErrOutboundHandoverConflict
	}
	rows, err := s.q.QueryContext(ctx, `WITH counters AS (
 SELECT meter_name,used FROM org_usage_counters WHERE org_id=$1::uuid AND period_start=$2 AND period_end=$3
 AND (meter_name IN ($4,$5,$6,$7) OR meter_name LIKE $8||':%')),
 events AS (SELECT meter_name,sum(quantity)::bigint AS used FROM usage_events WHERE org_id=$1::uuid AND created_at>=$2 AND created_at<$3 AND status='success'
 AND (meter_name IN ($4,$5,$6,$7) OR meter_name LIKE $8||':%') GROUP BY meter_name)
 SELECT coalesce(c.meter_name,e.meter_name),greatest(coalesce(c.used,0),coalesce(e.used,0)) FROM counters c FULL JOIN events e ON e.meter_name=c.meter_name ORDER BY 1 LIMIT $9`, org, snapshot.DayStart, snapshot.DayStart.Add(24*time.Hour), meterOutboundSendDay, meterOutboundReplyDay, meterOutboundFirstRecipientDay, meterOutboundHardBounceDay, meterOutboundReplyRecipient, outboundHandoverMaxBuckets+1)
	if err != nil {
		return snapshot, err
	}
	defer rows.Close()
	for rows.Next() {
		var bucket outboundHandoverBucket
		if err = rows.Scan(&bucket.Meter, &bucket.Used); err != nil {
			return snapshot, err
		}
		if !validOutboundHandoverMeter(bucket.Meter) || bucket.Used < 0 || bucket.Used > math.MaxInt32 {
			return snapshot, ErrOutboundHandoverConflict
		}
		if bucket.Meter == meterOutboundSendDay && bucket.Used > 0 && !snapshot.WarmupOrigin.Valid {
			return snapshot, ErrOutboundHandoverConflict
		}
		snapshot.Buckets = append(snapshot.Buckets, bucket)
	}
	if err = rows.Err(); err != nil {
		return snapshot, err
	}
	if len(snapshot.Buckets) > outboundHandoverMaxBuckets {
		return snapshot, ErrOutboundHandoverConflict
	}
	return snapshot, nil
}

func validOutboundHandoverMeter(meter string) bool {
	switch meter {
	case meterOutboundSendDay, meterOutboundReplyDay, meterOutboundFirstRecipientDay, meterOutboundHardBounceDay:
		return true
	}
	return strings.HasPrefix(meter, meterOutboundReplyRecipient+":") && validOutboundHandoverHash(strings.TrimPrefix(meter, meterOutboundReplyRecipient+":"))
}
func validOutboundHandoverHash(hash string) bool {
	if len(hash) != 64 {
		return false
	}
	for _, c := range hash {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (s *Store) prepareOutboundHandoverTarget(ctx context.Context, org, transfer, tag string, snapshot outboundHandoverSnapshot) (bool, int64, error) {
	var count int64
	var matches bool
	key := UsageReplayID(org, "policy_handover", transfer, outboundHandoverCompleteMeter, "")
	err := s.q.QueryRowContext(ctx, `SELECT quantity-1,tool_name=$3 AND meter_name=$4 AND status='success' AND created_at=$5 FROM usage_events WHERE org_id=$1::uuid AND replay_id=$2`, org, key, tag, outboundHandoverCompleteMeter, snapshot.FrozenAt).Scan(&count, &matches)
	if err == nil {
		if !matches || count < 0 {
			return false, 0, ErrOutboundHandoverConflict
		}
		return true, count, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, 0, err
	}
	enrolled, err := s.RecipientMeterEnrolled(ctx, org)
	if err != nil {
		return false, 0, err
	}
	if !enrolled {
		return false, 0, ErrOutboundHandoverConflict
	}
	var blocked bool
	if err = s.q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM org_recipient_periods WHERE org_id=$1::uuid AND (NOT admission_closed OR reserved<>0 OR committed<>0))
 OR EXISTS(SELECT 1 FROM outbox_messages WHERE org_id=$1::uuid)
 OR EXISTS(SELECT 1 FROM usage_events WHERE org_id=$1::uuid AND meter_name LIKE 'autonomous_outbound_%' AND tool_name<>$2)
 OR EXISTS(SELECT 1 FROM org_usage_counters WHERE org_id=$1::uuid AND meter_name LIKE 'autonomous_outbound_%' AND used<>0 AND NOT EXISTS(SELECT 1 FROM usage_events e WHERE e.org_id=$1::uuid AND e.meter_name=org_usage_counters.meter_name AND e.tool_name=$2 AND e.status='success'))`, org, tag).Scan(&blocked); err != nil {
		return false, 0, err
	}
	if blocked {
		return false, 0, ErrOutboundHandoverConflict
	}
	if _, err = s.CurrentOutboundPolicyEpoch(ctx, org); err != nil {
		return false, 0, err
	}
	if err = s.ensureOutboundHandoverEvent(ctx, org, transfer, tag, outboundHandoverSnapshotMeter, 1, snapshot.FrozenAt); err != nil {
		return false, 0, err
	}
	for _, bucket := range snapshot.Buckets {
		if bucket.Used == 0 {
			continue
		}
		if err = s.ensureOutboundHandoverEvent(ctx, org, transfer, tag, bucket.Meter, bucket.Used, snapshot.DayStart); err != nil {
			return false, 0, err
		}
		if err = s.EnsureOrgUsageCounter(ctx, org, bucket.Meter, snapshot.DayStart, snapshot.DayStart.Add(24*time.Hour)); err != nil {
			return false, 0, err
		}
		if _, err = s.q.ExecContext(ctx, `UPDATE org_usage_counters SET used=greatest(used,$4),updated_at=now() WHERE org_id=$1::uuid AND meter_name=$2 AND period_start=$3`, org, bucket.Meter, snapshot.DayStart, bucket.Used); err != nil {
			return false, 0, err
		}
	}
	if snapshot.WarmupOrigin.Valid {
		var origin sql.NullTime
		if err = s.q.QueryRowContext(ctx, `SELECT first_compose_accepted_at FROM org_outbound_policy_state WHERE org_id=$1::uuid FOR UPDATE`, org).Scan(&origin); err != nil {
			return false, 0, err
		}
		if origin.Valid && !origin.Time.Equal(snapshot.WarmupOrigin.Time) {
			return false, 0, ErrOutboundHandoverConflict
		}
		if err = s.persistOutboundWarmupOrigin(ctx, org, snapshot.WarmupOrigin.Time); err != nil {
			return false, 0, err
		}
	}
	return false, 0, nil
}

func (s *Store) ensureOutboundHandoverEvent(ctx context.Context, org, transfer, tag, meter string, quantity int64, at time.Time) error {
	key := UsageReplayID(org, "policy_handover", transfer, meter, "")
	if _, err := s.q.ExecContext(ctx, `INSERT INTO usage_events(org_id,meter_name,quantity,tool_name,replay_id,status,created_at) VALUES($1::uuid,$2,$3,$4,$5,'success',$6) ON CONFLICT(replay_id) WHERE replay_id IS NOT NULL DO NOTHING`, org, meter, quantity, tag, key, at); err != nil {
		return err
	}
	var exact bool
	if err := s.q.QueryRowContext(ctx, `SELECT org_id=$1::uuid AND meter_name=$2 AND quantity=$3 AND tool_name=$4 AND status='success' AND created_at=$6 FROM usage_events WHERE replay_id=$5`, org, meter, quantity, tag, key, at).Scan(&exact); err != nil {
		return err
	}
	if !exact {
		return ErrOutboundHandoverConflict
	}
	return nil
}

func (s *Store) exportOutboundHandoverHashes(ctx context.Context, org, cursor string) ([]string, error) {
	if cursor != "" && !validOutboundHandoverHash(cursor) {
		return nil, ErrOutboundHandoverConflict
	}
	// The fallback hashes the same lower/btrim bytes firstOutboundRecipient
	// compares; display-address parsing must not silently broaden its history.
	rows, err := s.q.QueryContext(ctx, `WITH hashes AS (
 SELECT substring(meter_name FROM $3) AS hash FROM usage_events WHERE org_id=$1::uuid AND status='success' AND meter_name LIKE $4||':%'
 UNION SELECT encode(digest(lower(btrim("to")),'sha256'),'hex') FROM outbox_messages WHERE org_id=$1::uuid)
 SELECT hash FROM hashes WHERE hash>$2 ORDER BY hash LIMIT $5`, org, cursor, len(meterOutboundRecipientSeen)+2, meterOutboundRecipientSeen, outboundHandoverPageSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hashes []string
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, err
		}
		if !validOutboundHandoverHash(hash) {
			return nil, ErrOutboundHandoverConflict
		}
		hashes = append(hashes, hash)
	}
	return hashes, rows.Err()
}

func (s *Store) installOutboundHandoverHashes(ctx context.Context, org, transfer, tag string, at time.Time, hashes []string) error {
	if len(hashes) == 0 || len(hashes) > outboundHandoverPageSize {
		return ErrOutboundHandoverConflict
	}
	meters, keys := make([]string, len(hashes)), make([]string, len(hashes))
	for i, hash := range hashes {
		if !validOutboundHandoverHash(hash) {
			return ErrOutboundHandoverConflict
		}
		meters[i] = meterOutboundRecipientSeen + ":" + hash
		keys[i] = UsageReplayID(org, "policy_handover", transfer, meters[i], "")
	}
	if _, err := s.q.ExecContext(ctx, `INSERT INTO usage_events(org_id,meter_name,quantity,tool_name,replay_id,status,created_at)
 SELECT $1::uuid,meter,1,$2,replay,'success',$3 FROM unnest($4::text[],$5::text[]) AS rows(meter,replay)
 ON CONFLICT(replay_id) WHERE replay_id IS NOT NULL DO NOTHING`, org, tag, at, meters, keys); err != nil {
		return err
	}
	var exact int
	if err := s.q.QueryRowContext(ctx, `SELECT count(*) FROM unnest($4::text[],$5::text[]) AS expected(meter,replay)
 JOIN usage_events e ON e.replay_id=expected.replay WHERE e.org_id=$1::uuid AND e.tool_name=$2 AND e.created_at=$3 AND e.meter_name=expected.meter AND e.quantity=1 AND e.status='success'`, org, tag, at, meters, keys).Scan(&exact); err != nil {
		return err
	}
	if exact != len(hashes) {
		return ErrOutboundHandoverConflict
	}
	return nil
}
