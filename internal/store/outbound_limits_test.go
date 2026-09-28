package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func TestAutonomousOutboundRecoveredReplayRecognizesLegacyRawKey(t *testing.T) {
	withOutboundLimitStore(t, func(ctx context.Context, st *Store, orgID, inboxID string) {
		const rawKey = "legacy-raw-key"
		legacy := outboundLimitMessage(orgID, inboxID, rawKey, "legacy@example.test", false)
		legacy.AutonomousLimits = nil
		legacyID, err := st.EnqueueOutboxMessage(ctx, legacy)
		if err != nil {
			t.Fatalf("insert pre-upgrade outbox row: %v", err)
		}

		retry := outboundLimitMessage(orgID, inboxID, rawKey, "legacy@example.test", false)
		retry.AllowLegacyIdempotencyReplay = true
		replayedID, err := st.EnqueueOutboxMessage(ctx, retry)
		if err != nil {
			t.Fatalf("replay pre-upgrade outbox row: %v", err)
		}
		if replayedID != legacyID {
			t.Fatalf("legacy replay returned %s, want %s", replayedID, legacyID)
		}

		var rows, usageEvents int
		if err := st.q.QueryRowContext(ctx, `
			SELECT count(*) FROM outbox_messages
			WHERE org_id = $1 AND idempotency_key IN ($2, $3)
		`, orgID, rawKey, OutboundIdempotencyKey("send_reply", rawKey)).Scan(&rows); err != nil {
			t.Fatalf("count legacy and scoped outbox rows: %v", err)
		}
		if err := st.q.QueryRowContext(ctx, `
			SELECT count(*) FROM usage_events
			WHERE replay_id = $1
		`, UsageReplayID(orgID, "send_reply", rawKey, meterOutboundReplyDay, "")).Scan(&usageEvents); err != nil {
			t.Fatalf("count replay usage events: %v", err)
		}
		if rows != 1 || usageEvents != 0 {
			t.Fatalf("outbox rows=%d usage events=%d, want 1,0", rows, usageEvents)
		}

		conflict := retry
		conflict.Subject = "different payload"
		if _, err := st.EnqueueOutboxMessage(ctx, conflict); !errors.Is(err, ErrOutboxIdempotencyConflict) {
			t.Fatalf("changed legacy replay err=%v, want ErrOutboxIdempotencyConflict", err)
		}
	})
}

func TestAutonomousOutboundRecoveredReplayPrefersScopedRowOverOtherToolLegacyKey(t *testing.T) {
	withOutboundLimitStore(t, func(ctx context.Context, st *Store, orgID, inboxID string) {
		const rawKey = "cross-tool-legacy-key"
		legacyReply := outboundLimitMessage(orgID, inboxID, rawKey, "legacy-reply@example.test", false)
		legacyReply.AutonomousLimits = nil
		legacyID, err := st.EnqueueOutboxMessage(ctx, legacyReply)
		if err != nil {
			t.Fatalf("insert other-tool legacy row: %v", err)
		}

		compose := outboundLimitMessage(orgID, inboxID, rawKey, "scoped-compose@example.test", true)
		composeID, err := st.EnqueueOutboxMessage(ctx, compose)
		if err != nil {
			t.Fatalf("insert scoped compose row: %v", err)
		}
		if composeID == legacyID {
			t.Fatal("fresh compose collapsed into other-tool legacy row")
		}

		recovered := compose
		recovered.AllowLegacyIdempotencyReplay = true
		replayedID, err := st.EnqueueOutboxMessage(ctx, recovered)
		if err != nil {
			t.Fatalf("replay recovered compose row: %v", err)
		}
		if replayedID != composeID {
			t.Fatalf("recovered replay returned %s, want scoped row %s", replayedID, composeID)
		}

		var rows int
		if err := st.q.QueryRowContext(ctx, `
			SELECT count(*) FROM outbox_messages
			WHERE org_id = $1 AND idempotency_key IN ($2, $3)
		`, orgID, rawKey, OutboundIdempotencyKey("compose_email", rawKey)).Scan(&rows); err != nil {
			t.Fatalf("count legacy/scoped rows: %v", err)
		}
		if rows != 2 {
			t.Fatalf("legacy/scoped outbox rows=%d, want 2", rows)
		}
		assertUsageCounterMatchesEvents(t, ctx, st, orgID, meterOutboundSendDay, 1)
	})
}

func TestUsageReplayNamespacesPersistAcrossOrganizationsAndTools(t *testing.T) {
	withOutboundLimitStore(t, func(ctx context.Context, st *Store, orgID, inboxID string) {
		secondOrgID, secondInboxID := insertOutboundLimitTenant(t, ctx, st, "namespace-second")
		const crossOrgKey = "namespace-cross-org"
		for _, message := range []OutboxMessage{
			outboundLimitMessage(orgID, inboxID, crossOrgKey, "org-one@example.test", false),
			outboundLimitMessage(secondOrgID, secondInboxID, crossOrgKey, "org-two@example.test", false),
		} {
			if _, err := st.EnqueueOutboxMessage(ctx, message); err != nil {
				t.Fatalf("enqueue cross-org reservation for %s: %v", message.OrgID, err)
			}
		}

		const crossToolUsageKey = "namespace-cross-tool"
		replyMessage := outboundLimitMessage(orgID, inboxID, crossToolUsageKey, "reply-tool@example.test", false)
		composeMessage := outboundLimitMessage(orgID, inboxID, crossToolUsageKey, "compose-tool@example.test", true)
		for _, message := range []OutboxMessage{replyMessage, composeMessage} {
			if _, err := st.EnqueueOutboxMessage(ctx, message); err != nil {
				t.Fatalf("enqueue cross-tool reservation for %s: %v", message.AutonomousLimits.ToolName, err)
			}
		}

		replayIDs := []string{
			UsageReplayID(orgID, "send_reply", crossOrgKey, meterOutboundReplyDay, ""),
			UsageReplayID(secondOrgID, "send_reply", crossOrgKey, meterOutboundReplyDay, ""),
			UsageReplayID(orgID, "send_reply", crossToolUsageKey, meterOutboundReplyDay, ""),
			UsageReplayID(orgID, "compose_email", crossToolUsageKey, meterOutboundSendDay, ""),
		}
		var events, distinctReplayIDs int
		if err := st.q.QueryRowContext(ctx, `
			SELECT count(*), count(DISTINCT replay_id)
			FROM usage_events
			WHERE replay_id IN ($1, $2, $3, $4)
		`, replayIDs[0], replayIDs[1], replayIDs[2], replayIDs[3]).Scan(&events, &distinctReplayIDs); err != nil {
			t.Fatalf("read persisted replay namespaces: %v", err)
		}
		if events != 4 || distinctReplayIDs != 4 {
			t.Fatalf("persisted events=%d distinct replay IDs=%d, want 4,4", events, distinctReplayIDs)
		}

		var outboxRows int
		crossOrgOutboxKey := OutboundIdempotencyKey("send_reply", crossOrgKey)
		replyOutboxKey := OutboundIdempotencyKey("send_reply", crossToolUsageKey)
		composeOutboxKey := OutboundIdempotencyKey("compose_email", crossToolUsageKey)
		if err := st.q.QueryRowContext(ctx, `
			SELECT count(*)
			FROM outbox_messages
			WHERE (org_id = $1 AND idempotency_key IN ($3, $4, $5))
			   OR (org_id = $2 AND idempotency_key = $3)
		`, orgID, secondOrgID, crossOrgOutboxKey, replyOutboxKey, composeOutboxKey).Scan(&outboxRows); err != nil {
			t.Fatalf("count namespaced outbox rows: %v", err)
		}
		if outboxRows != 4 {
			t.Fatalf("namespaced outbox rows=%d, want 4", outboxRows)
		}
		assertUsageCounterMatchesEvents(t, ctx, st, orgID, meterOutboundReplyDay, 2)
		assertUsageCounterMatchesEvents(t, ctx, st, orgID, meterOutboundSendDay, 1)
		assertUsageCounterMatchesEvents(t, ctx, st, secondOrgID, meterOutboundReplyDay, 1)
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
		Abuse struct {
			HardBounceSuspension struct {
				MinimumAttempts int64   `yaml:"minimum_attempts_per_utc_day"`
				RateGTE         float64 `yaml:"rate_gte"`
			} `yaml:"hard_bounce_suspension"`
		} `yaml:"abuse"`
	}
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatalf("parse outbound policy: %v", err)
	}
	if document.ComposeProfiles.ReplyOnly.Replies != limitReplyPerDay ||
		document.ComposeProfiles.ReplyOnly.PerRecipient != limitReplyPerRecipientDay ||
		document.ComposeProfiles.Compose.Sends != limitSendPerDay ||
		document.ComposeProfiles.Compose.FirstRecipients != limitFirstRecipientsPerDay ||
		document.Abuse.HardBounceSuspension.MinimumAttempts != limitHardBounceAttempts ||
		int64(document.Abuse.HardBounceSuspension.RateGTE*10_000) != limitHardBounceBasisPoints {
		t.Fatalf("compiled limits drifted from autonomous-outbound-v1.yaml: compose=%#v abuse=%#v", document.ComposeProfiles, document.Abuse)
	}
}

func TestOutboundV2TierAndWarmupCapsUseUTCDays(t *testing.T) {
	first := time.Date(2026, time.March, 28, 23, 30, 0, 0, time.UTC)
	for _, test := range []struct {
		name string
		tier string
		days int
		want outboundDailyCaps
	}{
		{"starter first", "starter", 0, outboundDailyCaps{100, 25}},
		{"starter after warmup", "starter", 7, outboundDailyCaps{100, 25}},
		{"growth day three", "growth", 2, outboundDailyCaps{100, 25}},
		{"growth day four across Madrid DST", "growth", 3, outboundDailyCaps{500, 125}},
		{"scale day seven", "scale", 6, outboundDailyCaps{500, 125}},
		{"scale day eight", "scale", 7, outboundDailyCaps{1500, 375}},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := first.Truncate(24 * time.Hour).Add(time.Duration(test.days)*24*time.Hour + time.Minute)
			got, err := effectiveOutboundV2Caps(test.tier, first, now)
			if err != nil || got != test.want {
				t.Fatalf("tier=%s day=%d caps=%+v err=%v, want %+v", test.tier, test.days, got, err, test.want)
			}
		})
	}
	for _, test := range []struct {
		tier       string
		first, now time.Time
	}{
		{"legacy", first, first},
		{"growth", first.Add(24 * time.Hour), first},
	} {
		if _, err := effectiveOutboundV2Caps(test.tier, test.first, test.now); !errors.Is(err, ErrOutboundPolicyVersionUnavailable) {
			t.Fatalf("unsafe policy tier=%s first=%s now=%s: %v", test.tier, test.first, test.now, err)
		}
	}
}

func TestOutboundV2CompiledCapsMatchVersionedPolicy(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "configs", "policy", "autonomous-outbound-v2.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Version    string `yaml:"version"`
		Activation struct {
			EntitlementFeature      string `yaml:"entitlement_feature"`
			RequiresRecipientPeriod bool   `yaml:"requires_recipient_period"`
			UnmarkedEntitlement     string `yaml:"unmarked_entitlement"`
		} `yaml:"activation"`
		ComposeProfiles struct {
			ReplyOnly struct {
				Replies      int64 `yaml:"accepted_replies_per_utc_day"`
				PerRecipient int64 `yaml:"accepted_replies_per_recipient_hash_per_utc_day"`
			} `yaml:"reply_only"`
			Compose struct {
				TierLimits map[string]struct {
					Sends int64 `yaml:"accepted_sends_per_utc_day"`
					First int64 `yaml:"first_time_recipients_per_utc_day"`
				} `yaml:"tier_limits"`
				Warmup struct {
					FirstThree struct {
						Sends int64 `yaml:"accepted_sends_per_utc_day"`
						First int64 `yaml:"first_time_recipients_per_utc_day"`
					} `yaml:"first_three_utc_days"`
					FourThroughSeven struct {
						Sends int64 `yaml:"accepted_sends_per_utc_day"`
						First int64 `yaml:"first_time_recipients_per_utc_day"`
					} `yaml:"utc_days_four_through_seven"`
				} `yaml:"warmup"`
			} `yaml:"compose"`
		} `yaml:"compose_profiles"`
	}
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	if document.Version != outboundPolicyV2 {
		t.Fatalf("policy version=%q", document.Version)
	}
	if document.Activation.EntitlementFeature != "outbound_policy_version" ||
		!document.Activation.RequiresRecipientPeriod ||
		document.Activation.UnmarkedEntitlement != "autonomous-outbound-v1" {
		t.Fatalf("unsafe v2 activation contract: %+v", document.Activation)
	}
	if document.ComposeProfiles.ReplyOnly.Replies != limitReplyPerDay ||
		document.ComposeProfiles.ReplyOnly.PerRecipient != limitReplyPerRecipientDay {
		t.Fatalf("v2 reply-only limits changed: %+v", document.ComposeProfiles.ReplyOnly)
	}
	for tier, want := range map[string]outboundDailyCaps{
		"starter": {100, 25}, "growth": {500, 125}, "scale": {1500, 375},
	} {
		configured, ok := document.ComposeProfiles.Compose.TierLimits[tier]
		if !ok || (outboundDailyCaps{configured.Sends, configured.First}) != want {
			t.Fatalf("tier %q configured=%+v, want %+v", tier, configured, want)
		}
	}
	if len(document.ComposeProfiles.Compose.TierLimits) != 3 ||
		(outboundDailyCaps{document.ComposeProfiles.Compose.Warmup.FirstThree.Sends,
			document.ComposeProfiles.Compose.Warmup.FirstThree.First}) != (outboundDailyCaps{100, 25}) ||
		(outboundDailyCaps{document.ComposeProfiles.Compose.Warmup.FourThroughSeven.Sends,
			document.ComposeProfiles.Compose.Warmup.FourThroughSeven.First}) != (outboundDailyCaps{500, 125}) {
		t.Fatalf("v2 warmup policy drifted from compiled caps: %+v", document.ComposeProfiles.Compose)
	}
}

func TestOutboundV2RequiresExplicitEntitlementAndPreservesWarmupHistory(t *testing.T) {
	withOutboundLimitStore(t, func(ctx context.Context, st *Store, orgID, inboxID string) {
		now := time.Now().UTC()
		if _, err := st.q.ExecContext(ctx, `INSERT INTO org_entitlements
  (org_id,plan_code,subscription_status,mcp_rpm,monthly_units,max_inboxes,max_domains,
   features,usage_period_start,usage_period_end)
  VALUES ($1,'scale','active',500,0,0,250,'{}',$2,$3)`, orgID,
			now.Add(-time.Hour), now.Add(30*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		readCaps := func() (outboundDailyCaps, error) {
			var got outboundDailyCaps
			err := st.RunInTx(ctx, func(tx *Store) error {
				if err := tx.LockOrgPolicy(ctx, orgID); err != nil {
					return err
				}
				clock, err := tx.readOutboundLimitClock(ctx)
				if err != nil {
					return err
				}
				got, _, err = tx.outboundComposeCaps(ctx, orgID, clock)
				return err
			})
			return got, err
		}
		if got, err := readCaps(); err != nil || got != (outboundDailyCaps{100, 25}) {
			t.Fatalf("legacy Scale acquired v2 caps: %+v, %v", got, err)
		}
		if _, err := st.q.ExecContext(ctx, `UPDATE org_entitlements SET
  features='{"outbound_policy_version":"autonomous-outbound-v2"}'::jsonb WHERE org_id=$1`, orgID); err != nil {
			t.Fatal(err)
		}
		if _, err := readCaps(); !errors.Is(err, ErrOutboundPolicyVersionUnavailable) {
			t.Fatalf("v2 marker without monthly recipient ledger did not fail closed: %v", err)
		}
		if err := st.RunInTx(ctx, func(tx *Store) error {
			return tx.InstallRecipientPeriod(ctx, RecipientPeriod{
				OrgID: orgID, PeriodID: uuid.NewString(),
				StartsAt: now.Add(-time.Hour), EndsAt: now.Add(30 * 24 * time.Hour),
				Limit: sql.NullInt64{Int64: 30000, Valid: true},
			})
		}); err != nil {
			t.Fatal(err)
		}
		if got, err := readCaps(); err != nil || got != (outboundDailyCaps{100, 25}) {
			t.Fatalf("new Scale skipped warmup: %+v, %v", got, err)
		}
		var stored sql.NullTime
		if err := st.q.QueryRowContext(ctx, `SELECT first_compose_accepted_at
  FROM org_outbound_policy_state WHERE org_id=$1`, orgID).Scan(&stored); err != nil || stored.Valid {
			t.Fatalf("policy read started warmup without admission: %+v, %v", stored, err)
		}
		if err := st.RunInTx(ctx, func(tx *Store) error {
			clock, err := tx.readOutboundLimitClock(ctx)
			if err != nil {
				return err
			}
			if err := tx.EnsureOrgUsageCounter(ctx, orgID, meterOutboundSendDay, clock.dayStart, clock.dayEnd); err != nil {
				return err
			}
			_, err = tx.q.ExecContext(ctx, `UPDATE org_usage_counters SET used=100
  WHERE org_id=$1 AND meter_name=$2 AND period_start=$3`, orgID, meterOutboundSendDay, clock.dayStart)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.EnqueueOutboxMessage(ctx, outboundLimitMessage(orgID, inboxID,
			"warmup-denied", "new@example.test", true)); err == nil {
			t.Fatal("first-day Scale send bypassed 100-send warmup")
		} else {
			var limit *OutboundLimitError
			if !errors.As(err, &limit) || limit.MeterName != meterOutboundSendDay {
				t.Fatalf("first-day Scale denial=%v", err)
			}
		}
		if err := st.q.QueryRowContext(ctx, `SELECT first_compose_accepted_at
  FROM org_outbound_policy_state WHERE org_id=$1`, orgID).Scan(&stored); err != nil || stored.Valid {
			t.Fatalf("denied compose started warmup: %+v, %v", stored, err)
		}
		historical := now.Add(-8 * 24 * time.Hour).Truncate(time.Microsecond)
		if _, err := st.q.ExecContext(ctx, `INSERT INTO usage_events
  (org_id,meter_name,quantity,tool_name,status,created_at)
  VALUES ($1,$2,1,'compose_email','success',$3)`, orgID, meterOutboundSendDay,
			historical); err != nil {
			t.Fatal(err)
		}
		if got, err := readCaps(); err != nil || got != (outboundDailyCaps{1500, 375}) {
			t.Fatalf("historical compose did not restore Scale caps: %+v, %v", got, err)
		}
		if _, err := st.EnqueueOutboxMessage(ctx, outboundLimitMessage(orgID, inboxID,
			"post-warmup", "new@example.test", true)); err != nil {
			t.Fatalf("post-warmup Scale send: %v", err)
		}
		if err := st.q.QueryRowContext(ctx, `SELECT first_compose_accepted_at
  FROM org_outbound_policy_state WHERE org_id=$1`, orgID).Scan(&stored); err != nil ||
			!stored.Valid || !stored.Time.Equal(historical) {
			t.Fatalf("accepted compose did not preserve historical origin: %+v, %v", stored, err)
		}
		if _, err := st.q.ExecContext(ctx, `UPDATE org_entitlements SET plan_code='growth' WHERE org_id=$1`, orgID); err != nil {
			t.Fatal(err)
		}
		if got, err := readCaps(); err != nil || got != (outboundDailyCaps{500, 125}) {
			t.Fatalf("tier change reset durable warmup history: %+v, %v", got, err)
		}
		if _, err := st.q.ExecContext(ctx, `UPDATE org_entitlements SET plan_code='unknown' WHERE org_id=$1`, orgID); err != nil {
			t.Fatal(err)
		}
		if _, err := readCaps(); !errors.Is(err, ErrOutboundPolicyVersionUnavailable) {
			t.Fatalf("unknown v2 tier did not fail closed: %v", err)
		}
	})
}

func TestCore32WarmupOriginMigrationAndRollbackGuard(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if err := MigrateUpToCore(ctx, db, 31); err != nil {
			t.Fatal(err)
		}
		orgID := uuid.NewString()
		historical := time.Now().UTC().Add(-8 * 24 * time.Hour).Truncate(time.Microsecond)
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs(id,name) VALUES($1,'warmup migration')`, orgID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO org_outbound_policy_state(org_id) VALUES($1)`, orgID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO usage_events
  (org_id,meter_name,quantity,tool_name,status,created_at)
  VALUES($1,$2,1,'compose_email','success',$3)`, orgID, meterOutboundSendDay, historical); err != nil {
			t.Fatal(err)
		}
		if err := MigrateUpToCore(ctx, db, 32); err != nil {
			t.Fatal(err)
		}
		var index sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT to_regclass('public.idx_usage_events_first_compose')::text`).Scan(&index); err != nil || !index.Valid {
			t.Fatalf("bounded historical lookup index missing: %+v, %v", index, err)
		}
		st := &Store{db: db, q: db}
		if err := st.RunInTx(ctx, func(tx *Store) error {
			if err := tx.LockOrgPolicy(ctx, orgID); err != nil {
				return err
			}
			origin, err := tx.outboundWarmupOrigin(ctx, orgID, time.Now().UTC())
			if err != nil || !origin.Equal(historical) {
				return fmt.Errorf("historical origin=%s, want %s: %v", origin, historical, err)
			}
			return tx.persistOutboundWarmupOrigin(ctx, orgID, origin)
		}); err != nil {
			t.Fatal(err)
		}
		if err := MigrateDownCore(ctx, db); err == nil || !strings.Contains(err.Error(), "warmup origin evidence exists") {
			t.Fatalf("Core32 down discarded warmup history: %v", err)
		}
	})
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
		if err := st.RunInTx(ctx, func(scoped *Store) error {
			if _, err := scoped.SetFeatureFlag(ctx, &orgID, "autonomous_outbound_policy", true, "test"); err != nil {
				return err
			}
			if _, err := scoped.SetFeatureFlag(ctx, &orgID, "email_outbound_suspended", false, "test"); err != nil {
				return err
			}
			_, err := scoped.EnsureOutboundPolicyState(ctx, orgID)
			return err
		}); err != nil {
			t.Fatalf("seed autonomous policy: %v", err)
		}
		run(ctx, st, orgID, inboxID)
	})
}

func insertOutboundLimitTenant(t *testing.T, ctx context.Context, st *Store, label string) (string, string) {
	t.Helper()
	orgID := uuid.NewString()
	inboxID := uuid.NewString()
	if _, err := st.q.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, $2)`, orgID, label); err != nil {
		t.Fatalf("insert %s org: %v", label, err)
	}
	address := label + "@local.neuralmail"
	if _, err := st.q.ExecContext(ctx, `
		INSERT INTO inboxes (id, org_id, address, status)
		VALUES ($1, $2, $3, 'active')
	`, inboxID, orgID, address); err != nil {
		t.Fatalf("insert %s inbox: %v", label, err)
	}
	if err := st.RunInTx(ctx, func(scoped *Store) error {
		if _, err := scoped.SetFeatureFlag(ctx, &orgID, "autonomous_outbound_policy", true, "test"); err != nil {
			return err
		}
		if _, err := scoped.SetFeatureFlag(ctx, &orgID, "email_outbound_suspended", false, "test"); err != nil {
			return err
		}
		_, err := scoped.EnsureOutboundPolicyState(ctx, orgID)
		return err
	}); err != nil {
		t.Fatalf("seed %s autonomous policy: %v", label, err)
	}
	return orgID, inboxID
}
