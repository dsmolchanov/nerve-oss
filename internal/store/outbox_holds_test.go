package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestOutboxDeliveryHoldBlocksOnlyExactPreheldMessage(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID := uuid.NewString()
		inboxID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'drill-org')`, orgID); err != nil {
			t.Fatalf("insert org: %v", err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO inboxes (id, org_id, address, status)
			VALUES ($1, $2, 'drill@local.neuralmail', 'active')
		`, inboxID, orgID); err != nil {
			t.Fatalf("insert inbox: %v", err)
		}

		const heldKey = "phase8-drill-held"
		hold, changed, holdAuditID, err := st.CreateOutboxDeliveryHoldAudited(
			ctx, orgID, heldKey, "phase8_flag_off_drill", "operator@example.test", 10*time.Minute,
		)
		if err != nil {
			t.Fatalf("create hold: %v", err)
		}
		if !changed || hold.ID == "" || holdAuditID == "" || hold.HoldReplayID != holdAuditID {
			t.Fatalf("unexpected created hold: %+v changed=%v audit=%q", hold, changed, holdAuditID)
		}
		duplicate, duplicateChanged, duplicateAuditID, err := st.CreateOutboxDeliveryHoldAudited(
			ctx, orgID, heldKey, "phase8_flag_off_drill", "operator@example.test", 10*time.Minute,
		)
		if err != nil {
			t.Fatalf("repeat hold: %v", err)
		}
		if duplicateChanged || duplicate.ID != hold.ID || duplicateAuditID == holdAuditID {
			t.Fatalf("repeat was not idempotent: %+v changed=%v audit=%q", duplicate, duplicateChanged, duplicateAuditID)
		}

		heldID, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID: orgID, InboxID: inboxID, Provider: "smtp", IdempotencyKey: heldKey,
			To: "held@local.neuralmail", From: "drill@local.neuralmail", Subject: "held",
			TextBody: "held body",
			Attachments: []OutboundAttachment{{
				Filename: "drill.pdf", ContentType: "application/pdf", Content: []byte("synthetic-pdf"),
			}},
		})
		if err != nil {
			t.Fatalf("enqueue held row: %v", err)
		}
		unrelatedID, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID: orgID, InboxID: inboxID, Provider: "smtp", IdempotencyKey: "phase8-unrelated",
			To: "other@local.neuralmail", From: "drill@local.neuralmail", Subject: "unrelated",
			TextBody: "unrelated body",
		})
		if err != nil {
			t.Fatalf("enqueue unrelated row: %v", err)
		}

		claimed, err := st.ClaimOutboxMessages(ctx, 10, "drill-worker", time.Now().UTC(), 5*time.Minute)
		if err != nil {
			t.Fatalf("claim with hold: %v", err)
		}
		if len(claimed) != 1 || claimed[0].ID != unrelatedID {
			t.Fatalf("claim crossed exact hold: %+v", claimed)
		}

		released, releaseChanged, releaseAuditID, err := st.ReleaseOutboxDeliveryHoldAudited(
			ctx, orgID, heldKey, "operator@example.test",
		)
		if err != nil {
			t.Fatalf("release hold: %v", err)
		}
		if !releaseChanged || released.ID != hold.ID || released.ReleaseReplayID == nil ||
			*released.ReleaseReplayID != releaseAuditID {
			t.Fatalf("unexpected release: %+v changed=%v audit=%q", released, releaseChanged, releaseAuditID)
		}

		claimed, err = st.ClaimOutboxMessages(ctx, 10, "drill-worker", time.Now().UTC(), 5*time.Minute)
		if err != nil {
			t.Fatalf("claim after release: %v", err)
		}
		if len(claimed) != 1 || claimed[0].ID != heldID {
			t.Fatalf("released row was not claimed: %+v", claimed)
		}

		var auditCount int
		if err := db.QueryRowContext(ctx, `
			SELECT count(*) FROM audit_log WHERE actor = 'operator@example.test'
		`).Scan(&auditCount); err != nil {
			t.Fatalf("count hold audit: %v", err)
		}
		if auditCount != 3 {
			t.Fatalf("audit count=%d, want 3", auditCount)
		}
	})
}

func TestOutboxDeliveryHoldExpiresWithoutOperatorRelease(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID := uuid.NewString()
		inboxID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'expiry-org')`, orgID); err != nil {
			t.Fatalf("insert org: %v", err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO inboxes (id, org_id, address, status)
			VALUES ($1, $2, 'expiry@local.neuralmail', 'active')
		`, inboxID, orgID); err != nil {
			t.Fatalf("insert inbox: %v", err)
		}
		hold, _, _, err := st.CreateOutboxDeliveryHoldAudited(
			ctx, orgID, "expiring-drill", "phase8_flag_off_drill", "operator@example.test", MinOutboxDeliveryHoldTTL,
		)
		if err != nil {
			t.Fatalf("create expiring hold: %v", err)
		}
		messageID, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID: orgID, InboxID: inboxID, Provider: "smtp", IdempotencyKey: "expiring-drill",
			To: "expiry-target@local.neuralmail", From: "expiry@local.neuralmail", Subject: "expiry",
			TextBody: "expiry body",
		})
		if err != nil {
			t.Fatalf("enqueue expiring row: %v", err)
		}
		claimed, err := st.ClaimOutboxMessages(ctx, 10, "expiry-worker", hold.ExpiresAt.Add(time.Second), 5*time.Minute)
		if err != nil {
			t.Fatalf("claim after expiry: %v", err)
		}
		if len(claimed) != 1 || claimed[0].ID != messageID {
			t.Fatalf("expired hold still blocked row: %+v", claimed)
		}
	})
}

func TestOutboxDeliveryHoldValidationAndLookup(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		if _, _, _, err := st.CreateOutboxDeliveryHoldAudited(
			ctx, "not-a-uuid", "key", "reason", "actor", time.Minute,
		); err == nil {
			t.Fatal("invalid org id was accepted")
		}
		if _, _, _, err := st.CreateOutboxDeliveryHoldAudited(
			ctx, uuid.NewString(), "key", "reason", "actor", MaxOutboxDeliveryHoldTTL+time.Second,
		); err == nil {
			t.Fatal("unbounded ttl was accepted")
		}
		if _, err := st.LatestOutboxDeliveryHold(ctx, uuid.NewString(), "absent"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("absent lookup error=%v, want sql.ErrNoRows", err)
		}
	})
}

func TestMigration28DownRefusesHoldHistory(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if err := MigrateUpToCore(ctx, db, 28); err != nil {
			t.Fatalf("migrate core to 28: %v", err)
		}
		orgID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'down-org')`, orgID); err != nil {
			t.Fatalf("insert org: %v", err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO outbox_delivery_holds (
			  org_id, idempotency_key, reason, held_by, hold_replay_id, created_at, expires_at
			) VALUES ($1, 'down-key', 'test', 'tester', $2, now(), now() + interval '5 minutes')
		`, orgID, uuid.NewString()); err != nil {
			t.Fatalf("insert hold: %v", err)
		}
		if err := MigrateDownCore(ctx, db); err == nil {
			t.Fatal("migration 0028 down accepted durable hold history")
		}
		version, err := CurrentVersionCore(ctx, db)
		if err != nil || version != 28 {
			t.Fatalf("version after refused down=%d err=%v", version, err)
		}
	})
}
