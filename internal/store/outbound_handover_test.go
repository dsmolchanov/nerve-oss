package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestOutboundHandoverRequiresTransactionAndCanonicalIdentity(t *testing.T) {
	s := &Store{}
	if _, err := s.TransferOutboundPolicyHistory(context.Background(), uuid.NewString(), uuid.NewString(), uuid.NewString()); err == nil {
		t.Fatal("handover outside transaction accepted")
	}
	for _, value := range []string{"", "A", "zz", "000000000000000000000000000000000000000000000000000000000000000G"} {
		if validOutboundHandoverHash(value) {
			t.Fatalf("invalid hash accepted: %q", value)
		}
	}
	hash := outboundRecipientHash("recipient@example.test")
	if !validOutboundHandoverHash(hash) || !validOutboundHandoverMeter(meterOutboundReplyRecipient+":"+hash) {
		t.Fatal("canonical recipient dimension rejected")
	}
	if validOutboundHandoverMeter(meterOutboundRecipientSeen + ":" + hash) {
		t.Fatal("lifetime marker admitted as daily bucket")
	}
}

func handoverPeriods(t *testing.T, ctx context.Context, s *Store, source, target string) (RecipientPeriod, RecipientPeriod) {
	t.Helper()
	now := time.Now().UTC()
	old := RecipientPeriod{OrgID: source, PeriodID: uuid.NewString(), StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour), Limit: sql.NullInt64{Int64: 300, Valid: true}}
	zero := RecipientPeriod{OrgID: target, PeriodID: uuid.NewString(), StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour), Limit: sql.NullInt64{Int64: 0, Valid: true}}
	if err := s.RunInTx(ctx, func(tx *Store) error {
		if err := tx.InstallRecipientPeriod(ctx, old); err != nil {
			return err
		}
		if err := tx.InstallRecipientPeriod(ctx, zero); err != nil {
			return err
		}
		_, err := tx.q.ExecContext(ctx, `UPDATE org_recipient_periods SET admission_closed=true WHERE org_id=$1 AND period_id=$2`, target, zero.PeriodID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return old, zero
}

func TestOutboundHandoverCopiesEnforcedHistoryAndPreservesReplay(t *testing.T) {
	withOutboundLimitStore(t, func(ctx context.Context, s *Store, source, inbox string) {
		target, targetInbox := insertOutboundLimitTenant(t, ctx, s, "handover-target")
		_, _ = handoverPeriods(t, ctx, s, source, target)
		// Accepted attempts are real enqueue/counter writes, not billing snapshots.
		for n := range 2 {
			if _, err := s.EnqueueOutboxMessage(ctx, outboundLimitMessage(source, inbox, fmt.Sprintf("compose-%d", n), "seen@example.test", true)); err != nil {
				t.Fatal(err)
			}
		}
		for n := range 5 {
			if _, err := s.EnqueueOutboxMessage(ctx, outboundLimitMessage(source, inbox, fmt.Sprintf("reply-%d", n), "reply@example.test", false)); err != nil {
				t.Fatal(err)
			}
		}
		// Older lifetime history crosses multiple bounded pages and remains relevant
		// even though its original UTC day is outside the copied daily window.
		if _, err := s.q.ExecContext(ctx, `INSERT INTO usage_events(org_id,meter_name,quantity,tool_name,status,created_at)
   SELECT $1::uuid,$2||':'||encode(digest('old-'||n::text||'@example.test','sha256'),'hex'),1,'compose_email','success',now()-interval '10 days'
   FROM generate_series(1,600) AS n`, source, meterOutboundRecipientSeen); err != nil {
			t.Fatal(err)
		}
		transfer := uuid.NewString()
		var result OutboundPolicyHandoverResult
		if err := s.RunInTx(ctx, func(tx *Store) error {
			var err error
			result, err = tx.TransferOutboundPolicyHistory(ctx, source, target, transfer)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		// 600 older hashes plus the compose and reply fallback outbox hashes.
		if result.RecipientHashesCopied != 602 || len(result.SnapshotSHA256) != 64 || !result.DayStart.Equal(result.FrozenAt.UTC().Truncate(24*time.Hour)) {
			t.Fatalf("result=%+v", result)
		}
		for meter, want := range map[string]int64{meterOutboundSendDay: 2, meterOutboundFirstRecipientDay: 1, meterOutboundReplyDay: 5, meterOutboundReplyRecipient + ":" + outboundRecipientHash("reply@example.test"): 5} {
			assertUsageCounterMatchesEvents(t, ctx, s, target, meter, want)
		}
		var enrolled bool
		if err := s.RunInTx(ctx, func(tx *Store) error {
			var err error
			_, enrolled, err = tx.RecipientAdmissionPeriod(ctx, target)
			return err
		}); !errors.Is(err, ErrRecipientPeriodUnavailable) || !enrolled {
			t.Fatalf("target unexpectedly active: %v", err)
		}
		// Activate a grant only after history has committed. A Cloud caller will put
		// its authoritative grant in the same transaction as the history transfer.
		grant := RecipientPeriod{OrgID: target, PeriodID: uuid.NewString(), StartsAt: result.FrozenAt, EndsAt: result.FrozenAt.Add(time.Hour), Limit: sql.NullInt64{Int64: 180, Valid: true}}
		if err := s.RunInTx(ctx, func(tx *Store) error { return tx.InstallRecipientPeriod(ctx, grant) }); err != nil {
			t.Fatal(err)
		}
		if _, err := s.EnqueueOutboxMessage(ctx, outboundLimitMessage(target, targetInbox, "reply-blocked", "reply@example.test", false)); err == nil {
			t.Fatal("per-recipient reply cap reset")
		} else {
			var limited *OutboundLimitError
			if !errors.As(err, &limited) || limited.MeterName != meterOutboundReplyRecipient {
				t.Fatalf("wrong reply denial: %v", err)
			}
		}
		if _, err := s.EnqueueOutboxMessage(ctx, outboundLimitMessage(target, targetInbox, "seen-after-transfer", "seen@example.test", true)); err != nil {
			t.Fatal(err)
		}
		assertUsageCounterMatchesEvents(t, ctx, s, target, meterOutboundFirstRecipientDay, 1)
		assertUsageCounterMatchesEvents(t, ctx, s, target, meterOutboundSendDay, 3)
		if _, err := s.EnqueueOutboxMessage(ctx, outboundLimitMessage(target, targetInbox, "old-after-transfer", "old-1@example.test", true)); err != nil {
			t.Fatal(err)
		}
		assertUsageCounterMatchesEvents(t, ctx, s, target, meterOutboundFirstRecipientDay, 1)
		var sourceOrigin, targetOrigin time.Time
		if err := s.q.QueryRowContext(ctx, `SELECT (SELECT min(created_at) FROM usage_events WHERE org_id=$1 AND meter_name=$3 AND status='success'),first_compose_accepted_at FROM org_outbound_policy_state WHERE org_id=$2`, source, target, meterOutboundSendDay).Scan(&sourceOrigin, &targetOrigin); err != nil || !sourceOrigin.Equal(targetOrigin) {
			t.Fatalf("warmup origin reset source=%v target=%v err=%v", sourceOrigin, targetOrigin, err)
		}
		var replay OutboundPolicyHandoverResult
		if err := s.RunInTx(ctx, func(tx *Store) error {
			var err error
			replay, err = tx.TransferOutboundPolicyHistory(ctx, source, target, transfer)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if replay != result {
			t.Fatalf("replay=%+v original=%+v", replay, result)
		}
		assertUsageCounterMatchesEvents(t, ctx, s, target, meterOutboundSendDay, 4)
		assertUsageCounterMatchesEvents(t, ctx, s, target, meterOutboundFirstRecipientDay, 1)
		// Old source is closed even for direct daily reservations and suppressed
		// messages with no autonomous-policy argument.
		if err := s.RunInTx(ctx, func(tx *Store) error {
			return tx.ReserveOutboundLimits(ctx, source, uuid.NewString(), OutboundLimitInput{ToolName: "compose_email", IdempotencyKey: "after-freeze", Recipient: "new@example.test", ComposeEnabled: true})
		}); !errors.Is(err, ErrRecipientPeriodUnavailable) {
			t.Fatalf("source daily write=%v", err)
		}
		if err := s.AddSuppression(ctx, source, "suppressed@example.test", "hard_bounce", "bounce"); err != nil {
			t.Fatal(err)
		}
		msg := outboundLimitMessage(source, inbox, "suppressed-after-freeze", "suppressed@example.test", true)
		msg.AutonomousLimits = nil
		if _, err := s.EnqueueOutboxMessage(ctx, msg); !errors.Is(err, ErrRecipientPeriodUnavailable) {
			t.Fatalf("closed suppressed source=%v", err)
		}

		// A successor may itself become the source of the next handover.
		// Aggregated imported day journals must not replace the original
		// accepted instant with the synthetic UTC bucket-start timestamp.
		next, _ := insertOutboundLimitTenant(t, ctx, s, "handover-next")
		zero := RecipientPeriod{OrgID: next, PeriodID: uuid.NewString(), StartsAt: result.FrozenAt, EndsAt: result.FrozenAt.Add(time.Hour), Limit: sql.NullInt64{Int64: 0, Valid: true}}
		if err := s.RunInTx(ctx, func(tx *Store) error {
			if err := tx.InstallRecipientPeriod(ctx, zero); err != nil {
				return err
			}
			if _, err := tx.q.ExecContext(ctx, `UPDATE org_recipient_periods SET admission_closed=true WHERE org_id=$1 AND period_id=$2`, next, zero.PeriodID); err != nil {
				return err
			}
			_, err := tx.TransferOutboundPolicyHistory(ctx, target, next, uuid.NewString())
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var nextOrigin time.Time
		if err := s.q.QueryRowContext(ctx, `SELECT first_compose_accepted_at FROM org_outbound_policy_state WHERE org_id=$1`, next).Scan(&nextOrigin); err != nil || !nextOrigin.Equal(sourceOrigin) {
			t.Fatalf("successive transfer changed accepted origin original=%v next=%v err=%v", sourceOrigin, nextOrigin, err)
		}
		assertUsageCounterMatchesEvents(t, ctx, s, next, meterOutboundSendDay, 4)
		assertUsageCounterMatchesEvents(t, ctx, s, next, meterOutboundFirstRecipientDay, 1)
	})
}

func TestOutboundHandoverAbortsEnclosingTransactionOnConflict(t *testing.T) {
	for _, conflict := range []string{"live_target", "different_transfer", "invalid_hash", "sql_failure"} {
		t.Run(conflict, func(t *testing.T) {
			withOutboundLimitStore(t, func(ctx context.Context, s *Store, source, _ string) {
				target, _ := insertOutboundLimitTenant(t, ctx, s, "handover-conflict")
				old, zero := handoverPeriods(t, ctx, s, source, target)
				transfer := uuid.NewString()
				if conflict == "live_target" {
					if _, err := s.q.ExecContext(ctx, `UPDATE org_recipient_periods SET admission_closed=false WHERE org_id=$1 AND period_id=$2`, target, zero.PeriodID); err != nil {
						t.Fatal(err)
					}
				}
				if conflict == "invalid_hash" {
					if _, err := s.q.ExecContext(ctx, `INSERT INTO usage_events(org_id,meter_name,quantity,tool_name,status) VALUES($1,$2,1,'compose_email','success')`, source, meterOutboundRecipientSeen+":not-a-hash"); err != nil {
						t.Fatal(err)
					}
				}
				if conflict == "sql_failure" {
					if _, err := s.q.ExecContext(ctx, `CREATE FUNCTION fail_handover_import() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.tool_name LIKE 'policy_handover:%' THEN RAISE EXCEPTION 'injected handover failure'; END IF; RETURN NEW; END $$;
    CREATE TRIGGER fail_handover_import BEFORE INSERT ON usage_events FOR EACH ROW EXECUTE FUNCTION fail_handover_import()`); err != nil {
						t.Fatal(err)
					}
				}
				if conflict == "different_transfer" {
					if err := s.RunInTx(ctx, func(tx *Store) error {
						_, err := tx.TransferOutboundPolicyHistory(ctx, source, target, transfer)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					transfer = uuid.NewString()
				}
				var beforeEpoch int64
				if err := s.RunInTx(ctx, func(tx *Store) error {
					var err error
					beforeEpoch, err = tx.CurrentOutboundPolicyEpoch(ctx, source)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				var handoverErr error
				commitErr := s.RunInTx(ctx, func(tx *Store) error {
					_, handoverErr = tx.TransferOutboundPolicyHistory(ctx, source, target, transfer)
					return nil
				})
				if handoverErr == nil || commitErr == nil {
					t.Fatalf("handled failure committed: handover=%v commit=%v", handoverErr, commitErr)
				}
				var afterEpoch int64
				if err := s.RunInTx(ctx, func(tx *Store) error {
					var err error
					afterEpoch, err = tx.CurrentOutboundPolicyEpoch(ctx, source)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if afterEpoch != beforeEpoch {
					t.Fatalf("source epoch changed %d -> %d", beforeEpoch, afterEpoch)
				}
				if conflict != "different_transfer" {
					var closed bool
					if err := s.q.QueryRowContext(ctx, `SELECT admission_closed FROM org_recipient_periods WHERE org_id=$1 AND period_id=$2`, source, old.PeriodID).Scan(&closed); err != nil || closed {
						t.Fatalf("source freeze survived rollback closed=%v err=%v", closed, err)
					}
					var imports int
					if err := s.q.QueryRowContext(ctx, `SELECT count(*) FROM usage_events WHERE org_id=$1 AND tool_name LIKE 'policy_handover:%'`, target).Scan(&imports); err != nil || imports != 0 {
						t.Fatalf("partial import rows=%d err=%v", imports, err)
					}
				}
			})
		})
	}
}

func TestOutboundHandoverRetainsUnknownProviderReservation(t *testing.T) {
	withOutboundLimitStore(t, func(ctx context.Context, s *Store, source, inbox string) {
		target, _ := insertOutboundLimitTenant(t, ctx, s, "handover-unknown")
		old, _ := handoverPeriods(t, ctx, s, source, target)
		id, err := s.EnqueueOutboxMessage(ctx, outboundLimitMessage(source, inbox, "unknown", "unknown@example.test", true))
		if err != nil {
			t.Fatal(err)
		}
		claims, err := s.ClaimOutboxMessages(ctx, 1, "handover-worker", time.Now().UTC(), time.Minute)
		if err != nil || len(claims) != 1 || claims[0].ID != id {
			t.Fatalf("claims=%+v err=%v", claims, err)
		}
		operation, err := s.BeginOutboxProviderOperationState(ctx, claims[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.EnqueueOutboxMessage(ctx, outboundLimitMessage(source, inbox, "unstarted", "unstarted@example.test", true)); err != nil {
			t.Fatal(err)
		}
		if err := s.RunInTx(ctx, func(tx *Store) error {
			_, err := tx.TransferOutboundPolicyHistory(ctx, source, target, uuid.NewString())
			return err
		}); err != nil {
			t.Fatal(err)
		}
		assertRecipientCounters(t, ctx, s.db, old, 1, 0)
		replay, err := s.BeginOutboxProviderOperationState(ctx, claims[0])
		if err != nil || replay.ID != operation.ID || !replay.StartedAt.Equal(operation.StartedAt) {
			t.Fatalf("unknown operation lost: %+v err=%v", replay, err)
		}
		if err := s.MarkClaimedOutboxMessageSent(ctx, id, claims[0].LockedBy.String, replay.ID, "accepted-after-handover"); err != nil {
			t.Fatal(err)
		}
		assertRecipientCounters(t, ctx, s.db, old, 0, 1)
	})
}

func TestOutboundHandoverDoesNotInvertOutcomeLockOrder(t *testing.T) {
	withOutboundLimitStore(t, func(ctx context.Context, s *Store, source, inbox string) {
		target, _ := insertOutboundLimitTenant(t, ctx, s, "handover-lock-order")
		old, _ := handoverPeriods(t, ctx, s, source, target)
		id, err := s.EnqueueOutboxMessage(ctx, outboundLimitMessage(source, inbox, "locked-outcome", "locked@example.test", true))
		if err != nil {
			t.Fatal(err)
		}
		holder, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Rollback()
		var holderPID int
		if err := holder.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
			t.Fatal(err)
		}
		var heldID string
		if err := holder.QueryRowContext(ctx, `SELECT id::text FROM outbox_messages WHERE org_id=$1 AND id=$2 FOR UPDATE`, source, id).Scan(&heldID); err != nil {
			t.Fatal(err)
		}
		finished := make(chan error, 1)
		workerPID := make(chan int, 1)
		transfer := uuid.NewString()
		go func() {
			finished <- s.RunInTx(ctx, func(tx *Store) error {
				var pid int
				if err := tx.q.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
					return err
				}
				workerPID <- pid
				_, err := tx.TransferOutboundPolicyHistory(ctx, source, target, transfer)
				return err
			})
		}()
		var pid int
		select {
		case pid = <-workerPID:
		case <-time.After(5 * time.Second):
			t.Fatal("handover worker did not begin")
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			var blocked bool
			if err := s.q.QueryRowContext(ctx, `SELECT $2::int=ANY(pg_blocking_pids($1::int))`, pid, holderPID).Scan(&blocked); err != nil {
				t.Fatal(err)
			}
			if blocked {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("handover did not reach the held outbox")
			}
			time.Sleep(10 * time.Millisecond)
		}
		// An in-flight definitive outcome must still be able to take its period
		// lock while handover waits on the outbox. Acquiring period first in the
		// handover would form a cycle here and abort one transaction.
		periodCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		var periodID string
		if err := holder.QueryRowContext(periodCtx, `SELECT period_id::text FROM org_recipient_periods WHERE org_id=$1 AND period_id=$2 FOR UPDATE`, source, old.PeriodID).Scan(&periodID); err != nil {
			t.Fatalf("outcome lock cycle: %v", err)
		}
		if err := holder.Commit(); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-finished:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("handover did not finish after outcome released")
		}
	})
}
