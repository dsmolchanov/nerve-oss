package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestEnqueueReservesOneRecipientAcrossAllCallers(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		s, period := recipientFixture(t, ctx, db, sql.NullInt64{Int64: 3, Valid: true})
		inbox := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO inboxes(id,org_id,address,status)
  VALUES($1,$2,'sender@local.neuralmail','active')`, inbox, period.OrgID); err != nil {
			t.Fatal(err)
		}
		message := func(n int) OutboxMessage {
			return OutboxMessage{OrgID: period.OrgID, InboxID: inbox, Provider: "smtp",
				IdempotencyKey: fmt.Sprintf("meter-%d", n), To: fmt.Sprintf("recipient-%d@example.test", n),
				From: "sender@local.neuralmail", Subject: fmt.Sprintf("meter-%d", n), TextBody: "body"}
		}
		var wg sync.WaitGroup
		results := make(chan error, 20)
		for n := range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := s.EnqueueOutboxMessage(ctx, message(n))
				results <- err
			}()
		}
		wg.Wait()
		close(results)
		accepted := 0
		for err := range results {
			if err == nil {
				accepted++
			} else if !errors.Is(err, ErrRecipientLimit) {
				t.Errorf("unexpected enqueue error: %v", err)
			}
		}
		if accepted != 3 {
			t.Fatalf("accepted=%d, want 3", accepted)
		}
		assertRecipientCounters(t, ctx, db, period, 3, 0)
		var outboxRows int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM outbox_messages WHERE org_id=$1`, period.OrgID).Scan(&outboxRows); err != nil {
			t.Fatal(err)
		}
		if outboxRows != accepted {
			t.Fatalf("outbox rows=%d, accepted=%d", outboxRows, accepted)
		}
		if err := MigrateDownCore(ctx, db); err == nil || !strings.Contains(err.Error(), "metered outbox rows exist") {
			t.Fatalf("Core31 down with metered outbox: %v", err)
		}
	})
}

func TestCore30EnrolledOutboxRequiresProviderFence(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if err := MigrateUpToCore(ctx, db, 30); err != nil {
			t.Fatal(err)
		}
		s := &Store{db: db, q: db}
		period := RecipientPeriod{OrgID: uuid.NewString(), PeriodID: uuid.NewString(),
			StartsAt: time.Now().UTC().Add(-time.Hour), EndsAt: time.Now().UTC().Add(time.Hour),
			Limit: sql.NullInt64{Int64: 1, Valid: true}}
		inbox := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs(id,name) VALUES($1,'Core30 enrolled')`, period.OrgID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO inboxes(id,org_id,address,status)
  VALUES($1,$2,'sender@local.neuralmail','active')`, inbox, period.OrgID); err != nil {
			t.Fatal(err)
		}
		if err := s.RunInTx(ctx, func(tx *Store) error { return tx.InstallRecipientPeriod(ctx, period) }); err != nil {
			t.Fatal(err)
		}
		_, err := s.EnqueueOutboxMessage(ctx, OutboxMessage{OrgID: period.OrgID, InboxID: inbox,
			Provider: "smtp", IdempotencyKey: "core30-enrolled", To: "recipient@example.test",
			From: "sender@local.neuralmail", Subject: "core30", TextBody: "body"})
		var unsupported *UnsupportedSchemaError
		if !errors.As(err, &unsupported) || unsupported.RequiresCore != 31 {
			t.Fatalf("Core30 enrolled send err=%v", err)
		}
		assertRecipientCounters(t, ctx, db, period, 0, 0)
		var rows int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM outbox_messages WHERE org_id=$1`, period.OrgID).Scan(&rows); err != nil || rows != 0 {
			t.Fatalf("Core30 outbox rows=%d err=%v", rows, err)
		}
	})
}

func TestCore31ProviderStartRequiresPolicyEpochOrMeterMarker(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if err := MigrateCore(ctx, db); err != nil {
			t.Fatal(err)
		}
		orgID, inboxID := insertPolicyFenceTenant(t, ctx, db, "recipient-provider-shape")
		outboxID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO outbox_messages
  (id,org_id,inbox_id,provider,idempotency_key,"to","from",subject)
  VALUES($1,$2,$3,'smtp','recipient-provider-shape','to@example.test','from@example.test','shape')`,
			outboxID, orgID, inboxID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE outbox_messages
  SET provider_started_at=now(),provider_operation_id='outbox:' || id::text
  WHERE id=$1`, outboxID); err == nil || !strings.Contains(err.Error(), "chk_outbox_provider_fence_shape") {
			t.Fatalf("unfenced provider start: %v", err)
		}
	})
}

func TestPolicyEpochTerminalizationReleasesMeteredRecipient(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		s, period := recipientFixture(t, ctx, db, sql.NullInt64{Int64: 1, Valid: true})
		inbox := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO inboxes(id,org_id,address,status)
  VALUES($1,$2,'sender@local.neuralmail','active')`, inbox, period.OrgID); err != nil {
			t.Fatal(err)
		}
		seedPolicyFenceState(t, ctx, s, period.OrgID)
		id, err := s.EnqueueOutboxMessage(ctx, OutboxMessage{OrgID: period.OrgID, InboxID: inbox,
			Provider: "smtp", IdempotencyKey: "metered-policy", To: "recipient@example.test",
			From: "sender@local.neuralmail", Subject: "metered policy", TextBody: "body",
			AutonomousLimits: &OutboundLimitInput{ToolName: "compose_email", IdempotencyKey: "metered-policy", ComposeEnabled: true}})
		if err != nil {
			t.Fatal(err)
		}
		assertRecipientCounters(t, ctx, db, period, 1, 0)
		if err := s.RunInTx(ctx, func(tx *Store) error {
			_, terminalized, err := tx.AdvanceOutboundPolicyEpoch(ctx, period.OrgID)
			if err == nil && terminalized != 1 {
				return fmt.Errorf("terminalized=%d, want 1", terminalized)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		assertRecipientCounters(t, ctx, db, period, 0, 0)
		var status string
		if err := db.QueryRowContext(ctx, `SELECT status FROM outbox_messages WHERE org_id=$1 AND id=$2`, period.OrgID, id).Scan(&status); err != nil || status != "failed" {
			t.Fatalf("outbox status=%q err=%v", status, err)
		}
	})
}

func TestMeteredEnqueueReplayAndSuppressionCannotBypassAllowance(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		s, period := recipientFixture(t, ctx, db, sql.NullInt64{Int64: 1, Valid: true})
		inbox := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO inboxes(id,org_id,address,status)
  VALUES($1,$2,'sender@local.neuralmail','active')`, inbox, period.OrgID); err != nil {
			t.Fatal(err)
		}
		message := func(key, recipient string) OutboxMessage {
			return OutboxMessage{OrgID: period.OrgID, InboxID: inbox, Provider: "smtp",
				IdempotencyKey: key, To: recipient, From: "sender@local.neuralmail",
				Subject: key, TextBody: "body"}
		}
		first, err := s.EnqueueOutboxMessage(ctx, message("first", "first@example.test"))
		if err != nil {
			t.Fatal(err)
		}
		replayed, err := s.EnqueueOutboxMessage(ctx, message("first", "first@example.test"))
		if err != nil || replayed != first {
			t.Fatalf("idempotent replay=%q first=%q err=%v", replayed, first, err)
		}
		if _, err := s.EnqueueOutboxMessage(ctx, message("second", "second@example.test")); !errors.Is(err, ErrRecipientLimit) {
			t.Fatalf("over allowance enqueue: %v", err)
		}
		if err := s.AddSuppression(ctx, period.OrgID, "blocked@example.test", "hard_bounce", "bounce"); err != nil {
			t.Fatal(err)
		}
		blocked, err := s.EnqueueOutboxMessage(ctx, message("blocked", "blocked@example.test"))
		if err != nil {
			t.Fatal(err)
		}
		assertRecipientCounters(t, ctx, db, period, 1, 0)
		if replayed, err := s.ReplayOutboxMessage(ctx, period.OrgID, blocked); replayed || !errors.Is(err, ErrRecipientReplayRequiresNewMessage) {
			t.Fatalf("suppression replay=%v err=%v", replayed, err)
		}
	})
}

func TestProviderMigrationRetainsUnresolvedMeteredOperation(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		s, period := recipientFixture(t, ctx, db, sql.NullInt64{Int64: 2, Valid: true})
		inbox := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO inboxes(id,org_id,address,status)
  VALUES($1,$2,'sender@local.neuralmail','active')`, inbox, period.OrgID); err != nil {
			t.Fatal(err)
		}
		enqueue := func(key string) string {
			t.Helper()
			id, err := s.EnqueueOutboxMessage(ctx, OutboxMessage{OrgID: period.OrgID, InboxID: inbox,
				Provider: "smtp", IdempotencyKey: key, To: key + "@example.test",
				From: "sender@local.neuralmail", Subject: key, TextBody: "body"})
			if err != nil {
				t.Fatal(err)
			}
			return id
		}
		unknown := enqueue("unknown")
		claimed, err := s.ClaimOutboxMessages(ctx, 1, "metered-migration", time.Now().UTC(), time.Minute)
		if err != nil || len(claimed) != 1 || claimed[0].ID != unknown {
			t.Fatalf("claim=%+v err=%v", claimed, err)
		}
		if _, err := s.BeginOutboxProviderOperationState(ctx, claimed[0]); err != nil {
			t.Fatal(err)
		}
		queued := enqueue("queued")
		migrated, err := s.MigrateOutboxProviderToResend(ctx)
		if err != nil || migrated != 1 {
			t.Fatalf("migrated=%d err=%v", migrated, err)
		}
		for id, want := range map[string]string{unknown: "smtp", queued: "resend"} {
			var provider string
			if err := db.QueryRowContext(ctx, `SELECT provider FROM outbox_messages
  WHERE org_id=$1 AND id=$2`, period.OrgID, id).Scan(&provider); err != nil || provider != want {
				t.Fatalf("outbox %s provider=%q want=%q err=%v", id, provider, want, err)
			}
		}
		assertRecipientCounters(t, ctx, db, period, 2, 0)
	})
}

func TestOutboxDefinitiveOutcomesSettleRecipientReservation(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		s, period := recipientFixture(t, ctx, db, sql.NullInt64{Int64: 4, Valid: true})
		inbox := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO inboxes(id,org_id,address,status)
  VALUES($1,$2,'sender@local.neuralmail','active')`, inbox, period.OrgID); err != nil {
			t.Fatal(err)
		}
		enqueue := func(key string) string {
			t.Helper()
			id, err := s.EnqueueOutboxMessage(ctx, OutboxMessage{OrgID: period.OrgID, InboxID: inbox,
				Provider: "smtp", IdempotencyKey: key, To: key + "@example.test",
				From: "sender@local.neuralmail", Subject: key, TextBody: "body"})
			if err != nil {
				t.Fatal(err)
			}
			return id
		}
		accepted := enqueue("accepted")
		rejected := enqueue("rejected")
		inFlight := enqueue("in-flight")
		assertRecipientCounters(t, ctx, db, period, 3, 0)
		if err := s.MarkOutboxMessageSent(ctx, accepted, "provider-accepted"); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkOutboxMessageFailed(ctx, rejected, "pre-provider-reject"); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkOutboxMessageFailed(ctx, rejected, "pre-provider-reject"); err != nil {
			t.Fatal(err)
		}
		assertRecipientCounters(t, ctx, db, period, 1, 1)
		if err := s.MarkOutboxMessageSent(ctx, rejected, "contradiction"); !errors.Is(err, ErrRecipientLedgerConflict) {
			t.Fatalf("contradictory outcome: %v", err)
		}
		var status string
		if err := db.QueryRowContext(ctx, `SELECT status FROM outbox_messages WHERE id=$1`, rejected).Scan(&status); err != nil || status != "failed" {
			t.Fatalf("contradiction changed outbox status=%q err=%v", status, err)
		}
		claimed, err := s.ClaimOutboxMessages(ctx, 1, "recipient-worker", time.Now().UTC(), time.Minute)
		if err != nil || len(claimed) != 1 || claimed[0].ID != inFlight {
			t.Fatalf("claim=%+v err=%v", claimed, err)
		}
		if err := s.RequeueClaimedOutboxMessage(ctx, inFlight, claimed[0].LockedBy.String, time.Now().UTC(), "unknown"); err != nil {
			t.Fatal(err)
		}
		assertRecipientCounters(t, ctx, db, period, 1, 1)
		claimed, err = s.ClaimOutboxMessages(ctx, 1, "recipient-worker", time.Now().UTC(), time.Minute)
		if err != nil || len(claimed) != 1 || claimed[0].ID != inFlight {
			t.Fatalf("reclaim=%+v err=%v", claimed, err)
		}
		if err := s.MarkClaimedOutboxMessageSent(ctx, inFlight, claimed[0].LockedBy.String, "", "provider-in-flight"); err != nil {
			t.Fatal(err)
		}
		assertRecipientCounters(t, ctx, db, period, 0, 2)
		if id := enqueue("after-release"); id == "" {
			t.Fatal("released capacity unavailable")
		}
		assertRecipientCounters(t, ctx, db, period, 1, 2)
	})
}

func TestRecipientPeriodFencePreservesUnknownAndReleasesUnstarted(t *testing.T) {
	for _, scenario := range []string{"before_start", "unknown_after_start", "known_reject_after_start"} {
		t.Run(scenario, func(t *testing.T) {
			withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
				s, period := recipientFixture(t, ctx, db, sql.NullInt64{Int64: 1, Valid: true})
				inbox := uuid.NewString()
				if _, err := db.ExecContext(ctx, `INSERT INTO inboxes(id,org_id,address,status)
  VALUES($1,$2,'sender@local.neuralmail','active')`, inbox, period.OrgID); err != nil {
					t.Fatal(err)
				}
				id, err := s.EnqueueOutboxMessage(ctx, OutboxMessage{OrgID: period.OrgID, InboxID: inbox,
					Provider: "smtp", IdempotencyKey: scenario, To: "recipient@example.test",
					From: "sender@local.neuralmail", Subject: scenario, TextBody: "body"})
				if err != nil {
					t.Fatal(err)
				}
				claim := func() OutboxMessage {
					t.Helper()
					messages, err := s.ClaimOutboxMessages(ctx, 1, "recipient-worker", time.Now().UTC(), time.Minute)
					if err != nil || len(messages) != 1 || messages[0].ID != id {
						t.Fatalf("claim=%+v err=%v", messages, err)
					}
					return messages[0]
				}
				msg := claim()
				var operation OutboxProviderOperation
				if scenario != "before_start" {
					operation, err = s.BeginOutboxProviderOperationState(ctx, msg)
					if err != nil || operation.ID != "outbox:"+id || operation.StartedAt.IsZero() {
						t.Fatalf("start=%+v err=%v", operation, err)
					}
				}
				if _, err := db.ExecContext(ctx, `UPDATE org_recipient_periods SET admission_closed=true
  WHERE org_id=$1 AND period_id=$2`, period.OrgID, period.PeriodID); err != nil {
					t.Fatal(err)
				}
				switch scenario {
				case "before_start":
					if _, err := s.BeginOutboxProviderOperationState(ctx, msg); !errors.Is(err, ErrRecipientLimit) {
						t.Fatalf("closed period first start: %v", err)
					}
					assertRecipientCounters(t, ctx, db, period, 0, 0)
					if replayed, err := s.ReplayOutboxMessage(ctx, period.OrgID, id); replayed || !errors.Is(err, ErrRecipientReplayRequiresNewMessage) {
						t.Fatalf("replay closed metered outbox=%v err=%v", replayed, err)
					}
				case "unknown_after_start":
					if err := s.RequeueClaimedOutboxMessage(ctx, id, msg.LockedBy.String, time.Now().UTC(), "unknown"); err != nil {
						t.Fatal(err)
					}
					assertRecipientCounters(t, ctx, db, period, 1, 0)
					msg = claim()
					replayed, err := s.BeginOutboxProviderOperationState(ctx, msg)
					if err != nil || replayed.ID != operation.ID || !replayed.StartedAt.Equal(operation.StartedAt) {
						t.Fatalf("unknown replay=%+v err=%v", replayed, err)
					}
					if err := s.MarkClaimedOutboxMessageSent(ctx, id, msg.LockedBy.String, replayed.ID, "provider-accepted"); err != nil {
						t.Fatal(err)
					}
					assertRecipientCounters(t, ctx, db, period, 0, 1)
				case "known_reject_after_start":
					if err := s.RequeueClaimedOutboxKnownProviderFailure(ctx, id, msg.LockedBy.String,
						operation.ID, time.Now().UTC(), "provider-rejected"); err != nil {
						t.Fatal(err)
					}
					assertRecipientCounters(t, ctx, db, period, 0, 0)
				}
			})
		})
	}
}
