package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

func TestAutonomousReplyLimitsAreAtomicAcrossConnections(t *testing.T) {
	withOutboundLimitStore(t, func(ctx context.Context, st *Store, orgID, inboxID string) {
		var accepted atomic.Int64
		var limited atomic.Int64
		var other atomic.Int64
		var wait sync.WaitGroup
		for index := 0; index < 25; index++ {
			index := index
			wait.Add(1)
			go func() {
				defer wait.Done()
				_, err := st.EnqueueOutboxMessage(ctx, outboundLimitMessage(
					orgID, inboxID, fmt.Sprintf("reply-%d", index),
					fmt.Sprintf("recipient-%d@example.test", index), false,
				))
				var limitErr *OutboundLimitError
				switch {
				case err == nil:
					accepted.Add(1)
				case errors.As(err, &limitErr):
					limited.Add(1)
				default:
					other.Add(1)
				}
			}()
		}
		wait.Wait()
		if accepted.Load() != limitReplyPerDay || limited.Load() != 5 || other.Load() != 0 {
			t.Fatalf("accepted=%d limited=%d other=%d", accepted.Load(), limited.Load(), other.Load())
		}
		assertUsageCounterMatchesEvents(t, ctx, st, orgID, meterOutboundReplyDay, limitReplyPerDay)
	})
}

func TestAutonomousOutboundReplayConsumesOneUnit(t *testing.T) {
	withOutboundLimitStore(t, func(ctx context.Context, st *Store, orgID, inboxID string) {
		message := outboundLimitMessage(orgID, inboxID, "same-key", "one@example.test", false)
		first, err := st.EnqueueOutboxMessage(ctx, message)
		if err != nil {
			t.Fatalf("first enqueue: %v", err)
		}
		second, err := st.EnqueueOutboxMessage(ctx, message)
		if err != nil {
			t.Fatalf("replay enqueue: %v", err)
		}
		if first != second {
			t.Fatalf("replay returned %s, want %s", second, first)
		}
		assertUsageCounterMatchesEvents(t, ctx, st, orgID, meterOutboundReplyDay, 1)
		recipientMeter := meterOutboundReplyRecipient + ":" + outboundRecipientHash("one@example.test")
		assertUsageCounterMatchesEvents(t, ctx, st, orgID, recipientMeter, 1)
	})
}

func TestExpiredOutboundBucketGCLeavesAuditEvents(t *testing.T) {
	withOutboundLimitStore(t, func(ctx context.Context, st *Store, orgID, _ string) {
		start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
		if err := st.EnsureOrgUsageCounter(ctx, orgID, meterOutboundReplyDay, start, start.Add(24*time.Hour)); err != nil {
			t.Fatalf("seed counter: %v", err)
		}
		if err := st.EnsureOrgUsageCounter(ctx, orgID, MeterMCPRequestsPerMinute, start, start.Add(time.Minute)); err != nil {
			t.Fatalf("seed RPM counter: %v", err)
		}
		if err := st.EnsureOrgUsageCounter(ctx, orgID, "mcp_units", start, start.Add(time.Minute)); err != nil {
			t.Fatalf("seed unrelated counter: %v", err)
		}
		if err := st.RecordUsageEventAt(ctx, orgID, meterOutboundReplyDay, 1, "send_reply", "gc-event", "", "success", start); err != nil {
			t.Fatalf("seed event: %v", err)
		}
		if err := st.RecordUsageEventAt(ctx, orgID, MeterMCPRequestsPerMinute, 1, "send_reply", "gc-rpm-event", "", "success", start); err != nil {
			t.Fatalf("seed RPM event: %v", err)
		}
		deleted, err := st.DeleteExpiredOutboundUsageCounters(ctx, start.Add(48*time.Hour))
		if err != nil || deleted != 2 {
			t.Fatalf("deleted=%d err=%v", deleted, err)
		}
		var events int
		if err := st.q.QueryRowContext(ctx, `SELECT count(*) FROM usage_events WHERE replay_id IN ('gc-event', 'gc-rpm-event')`).Scan(&events); err != nil {
			t.Fatalf("count retained events: %v", err)
		}
		if events != 2 {
			t.Fatalf("audit event was removed")
		}
		if _, err := st.GetOrgUsageCounterUsed(ctx, orgID, "mcp_units", start); err != nil {
			t.Fatalf("unrelated counter was removed: %v", err)
		}
	})
}

func TestComposeCountsOnlyNewRecipientsAgainstDailyLimit(t *testing.T) {
	withOutboundLimitStore(t, func(ctx context.Context, st *Store, orgID, inboxID string) {
		for index := 0; index < int(limitFirstRecipientsPerDay); index++ {
			_, err := st.EnqueueOutboxMessage(ctx, outboundLimitMessage(
				orgID, inboxID, fmt.Sprintf("compose-%d", index),
				fmt.Sprintf("new-%d@example.test", index), true,
			))
			if err != nil {
				t.Fatalf("compose %d: %v", index, err)
			}
		}
		_, err := st.EnqueueOutboxMessage(ctx, outboundLimitMessage(
			orgID, inboxID, "compose-over-limit", "one-more@example.test", true,
		))
		var limitErr *OutboundLimitError
		if !errors.As(err, &limitErr) || limitErr.MeterName != meterOutboundFirstRecipientDay {
			t.Fatalf("expected first-recipient limit, got %v", err)
		}

		// An already-seen recipient does not consume a second first-recipient
		// unit, but it still consumes the total-send unit.
		_, err = st.EnqueueOutboxMessage(ctx, outboundLimitMessage(
			orgID, inboxID, "compose-known", "new-0@example.test", true,
		))
		if err != nil {
			t.Fatalf("known recipient compose: %v", err)
		}
		assertUsageCounterMatchesEvents(t, ctx, st, orgID, meterOutboundFirstRecipientDay, limitFirstRecipientsPerDay)
		assertUsageCounterMatchesEvents(t, ctx, st, orgID, meterOutboundSendDay, limitFirstRecipientsPerDay+1)
	})
}

func TestOutboundLimitsUsePostgreSQLClockForBucketEventAndRetry(t *testing.T) {
	withOutboundLimitStore(t, func(ctx context.Context, st *Store, orgID, inboxID string) {
		for index := 0; index < int(limitReplyPerRecipientDay); index++ {
			if _, err := st.EnqueueOutboxMessage(ctx, outboundLimitMessage(
				orgID, inboxID, fmt.Sprintf("database-clock-%d", index), "clock@example.test", false,
			)); err != nil {
				t.Fatalf("enqueue %d: %v", index, err)
			}
		}
		_, err := st.EnqueueOutboxMessage(ctx, outboundLimitMessage(
			orgID, inboxID, "database-clock-limited", "clock@example.test", false,
		))
		var limitErr *OutboundLimitError
		if !errors.As(err, &limitErr) {
			t.Fatalf("expected database-clock rate limit, got %v", err)
		}

		physicalMeter := meterOutboundReplyRecipient + ":" + outboundRecipientHash("clock@example.test")
		var periodStart, periodEnd, eventTime, databaseNow time.Time
		if err := st.q.QueryRowContext(ctx, `
			SELECT c.period_start, c.period_end, min(e.created_at), clock_timestamp()
			FROM org_usage_counters c
			JOIN usage_events e
			  ON e.org_id = c.org_id AND e.meter_name = c.meter_name
			WHERE c.org_id = $1 AND c.meter_name = $2
			GROUP BY c.period_start, c.period_end
		`, orgID, physicalMeter).Scan(&periodStart, &periodEnd, &eventTime, &databaseNow); err != nil {
			t.Fatalf("read database-clock evidence: %v", err)
		}
		if eventTime.Before(periodStart) || !eventTime.Before(periodEnd) {
			t.Fatalf("event time %s is outside database bucket [%s,%s)", eventTime, periodStart, periodEnd)
		}
		expectedStart := time.Date(databaseNow.UTC().Year(), databaseNow.UTC().Month(), databaseNow.UTC().Day(), 0, 0, 0, 0, time.UTC)
		if !periodStart.Equal(expectedStart) || !periodEnd.Equal(expectedStart.Add(24*time.Hour)) {
			t.Fatalf("database bucket=[%s,%s), want [%s,%s)", periodStart, periodEnd, expectedStart, expectedStart.Add(24*time.Hour))
		}
		remaining := int(periodEnd.Sub(databaseNow).Seconds())
		if limitErr.RetryAfterSeconds < remaining-2 || limitErr.RetryAfterSeconds > remaining+2 {
			t.Fatalf("retry-after=%d, database bucket remaining about %d", limitErr.RetryAfterSeconds, remaining)
		}
	})
}

func TestUsageReplayIDNamespacesEveryIdentityDimension(t *testing.T) {
	base := UsageReplayID("org-a", "send_reply", "same-key", meterOutboundReplyDay, "")
	for name, candidate := range map[string]string{
		"org":       UsageReplayID("org-b", "send_reply", "same-key", meterOutboundReplyDay, ""),
		"tool":      UsageReplayID("org-a", "compose_email", "same-key", meterOutboundReplyDay, ""),
		"key":       UsageReplayID("org-a", "send_reply", "other-key", meterOutboundReplyDay, ""),
		"meter":     UsageReplayID("org-a", "send_reply", "same-key", meterOutboundSendDay, ""),
		"dimension": UsageReplayID("org-a", "send_reply", "same-key", meterOutboundReplyDay, "recipient"),
	} {
		if candidate == base {
			t.Fatalf("%s was not namespaced", name)
		}
	}
}

func TestOutboundLimitConstantsMatchVersionedPolicy(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "configs", "policy", "autonomous-outbound-v1.yaml"))
	if err != nil {
		t.Fatalf("read outbound policy: %v", err)
	}
	var document struct {
		ComposeProfiles struct {
			ReplyOnly struct {
				Replies      int64 `yaml:"accepted_replies_per_utc_day"`
				PerRecipient int64 `yaml:"accepted_replies_per_recipient_hash_per_utc_day"`
			} `yaml:"reply_only"`
			Compose struct {
				Sends           int64 `yaml:"accepted_sends_per_utc_day"`
				FirstRecipients int64 `yaml:"first_time_recipients_per_utc_day"`
			} `yaml:"compose"`
		} `yaml:"compose_profiles"`
	}
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatalf("parse outbound policy: %v", err)
	}
	if document.ComposeProfiles.ReplyOnly.Replies != limitReplyPerDay ||
		document.ComposeProfiles.ReplyOnly.PerRecipient != limitReplyPerRecipientDay ||
		document.ComposeProfiles.Compose.Sends != limitSendPerDay ||
		document.ComposeProfiles.Compose.FirstRecipients != limitFirstRecipientsPerDay {
		t.Fatalf("compiled limits drifted from autonomous-outbound-v1.yaml: %#v", document.ComposeProfiles)
	}
}

func outboundLimitMessage(orgID, inboxID, key, recipient string, composeEnabled bool) OutboxMessage {
	toolName := "send_reply"
	if composeEnabled {
		toolName = "compose_email"
	}
	return OutboxMessage{
		OrgID: orgID, InboxID: inboxID, Provider: "smtp", IdempotencyKey: key,
		To: recipient, From: "sender@local.neuralmail", Subject: "subject " + key, TextBody: "body " + key,
		AutonomousLimits: &OutboundLimitInput{
			ToolName: toolName, IdempotencyKey: key, Recipient: recipient,
			ComposeEnabled: composeEnabled,
		},
	}
}

func assertUsageCounterMatchesEvents(
	t *testing.T, ctx context.Context, st *Store, orgID, meter string, want int64,
) {
	t.Helper()
	var start, end time.Time
	var used int64
	if err := st.q.QueryRowContext(ctx, `
		SELECT period_start, period_end, used
		FROM org_usage_counters
		WHERE org_id = $1 AND meter_name = $2
	`, orgID, meter).Scan(&start, &end, &used); err != nil {
		t.Fatalf("read %s counter: %v", meter, err)
	}
	events, err := st.SumUsageEvents(ctx, orgID, meter, start, end)
	if err != nil {
		t.Fatalf("sum %s events: %v", meter, err)
	}
	if used != want || events != want {
		t.Fatalf("%s used=%d events=%d want=%d", meter, used, events, want)
	}
}

func withOutboundLimitStore(t *testing.T, run func(context.Context, *Store, string, string)) {
	t.Helper()
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID := uuid.NewString()
		inboxID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'limit-test')`, orgID); err != nil {
			t.Fatalf("insert org: %v", err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO inboxes (id, org_id, address, status)
			VALUES ($1, $2, 'sender@local.neuralmail', 'active')
		`, inboxID, orgID); err != nil {
			t.Fatalf("insert inbox: %v", err)
		}
		run(ctx, st, orgID, inboxID)
	})
}
