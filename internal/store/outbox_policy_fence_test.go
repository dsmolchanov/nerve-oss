package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

func TestCoreMigration29PreservesLegacyOutboxAndClaim(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToVersion(t, ctx, db, 28)
		orgID, inboxID := insertPolicyFenceTenant(t, ctx, db, "legacy-29")
		outboxID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `
			INSERT INTO outbox_messages (
			  id, org_id, inbox_id, provider, idempotency_key, "to", "from", subject, text_body
			) VALUES ($1, $2, $3, 'smtp', $4, 'to@local.neuralmail',
			          'legacy-29@local.neuralmail', 'legacy', 'body')
		`, outboxID, orgID, inboxID, "legacy-29"); err != nil {
			t.Fatal(err)
		}

		if err := MigrateCore(ctx, db); err != nil {
			t.Fatal(err)
		}
		st := &Store{db: db, q: db}
		var epoch sql.NullInt64
		var startedAt, resolvedAt sql.NullTime
		var operationID sql.NullString
		if err := db.QueryRowContext(ctx, `
			SELECT autonomous_policy_epoch, provider_started_at, provider_operation_id, provider_resolved_at
			FROM outbox_messages WHERE id = $1
		`, outboxID).Scan(&epoch, &startedAt, &operationID, &resolvedAt); err != nil {
			t.Fatal(err)
		}
		if epoch.Valid || startedAt.Valid || operationID.Valid || resolvedAt.Valid {
			t.Fatalf("legacy row gained fence evidence: epoch=%v start=%v op=%v resolved=%v", epoch, startedAt, operationID, resolvedAt)
		}
		claimed, err := st.ClaimOutboxMessages(ctx, 1, "legacy-worker", time.Now().UTC(), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if len(claimed) != 1 || claimed[0].ID != outboxID || claimed[0].AutonomousPolicyEpoch != 0 {
			t.Fatalf("legacy claim=%+v", claimed)
		}
	})
}

func TestCoreMigration29DownRefusesPolicyAndProviderEvidence(t *testing.T) {
	for _, test := range []struct {
		name  string
		seed  func(context.Context, *sql.DB, string, string, string) error
		match string
	}{
		{
			name: "policy epoch",
			seed: func(ctx context.Context, db *sql.DB, orgID, _, _ string) error {
				_, err := db.ExecContext(ctx, `INSERT INTO org_outbound_policy_state (org_id) VALUES ($1)`, orgID)
				return err
			},
			match: "outbound policy epoch rows exist",
		},
		{
			name: "saved autonomous epoch",
			seed: func(ctx context.Context, db *sql.DB, _, _, outboxID string) error {
				_, err := db.ExecContext(ctx, `UPDATE outbox_messages SET autonomous_policy_epoch = 1 WHERE id = $1`, outboxID)
				return err
			},
			match: "outbox provider fence evidence exists",
		},
		{
			name: "unresolved provider start",
			seed: func(ctx context.Context, db *sql.DB, _, _, outboxID string) error {
				_, err := db.ExecContext(ctx, `
					UPDATE outbox_messages
					SET autonomous_policy_epoch = 1,
					    provider_started_at = now(),
					    provider_operation_id = 'outbox:' || id::text
					WHERE id = $1
				`, outboxID)
				return err
			},
			match: "outbox provider fence evidence exists",
		},
		{
			name: "resolved provider start",
			seed: func(ctx context.Context, db *sql.DB, _, _, outboxID string) error {
				_, err := db.ExecContext(ctx, `
					UPDATE outbox_messages
					SET autonomous_policy_epoch = 1,
					    provider_started_at = now(),
					    provider_operation_id = 'outbox:' || id::text,
					    provider_resolved_at = now()
					WHERE id = $1
				`, outboxID)
				return err
			},
			match: "outbox provider fence evidence exists",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
				migrateToLatest(t, ctx, db)
				orgID, inboxID := insertPolicyFenceTenant(t, ctx, db, "down-29")
				outboxID := insertPolicyFenceOutbox(t, ctx, db, orgID, inboxID, 0)
				if err := test.seed(ctx, db, orgID, inboxID, outboxID); err != nil {
					t.Fatal(err)
				}
				goose.SetDialect("postgres")
				goose.SetTableName(migrationTableCore)
				err := goose.DownToContext(ctx, db, coreMigrationDir(t), 28)
				if err == nil || !containsError(err, test.match) {
					t.Fatalf("down error=%v, want %q", err, test.match)
				}
			})
		})
	}
}

func TestCoreMigration29EmptyDownSucceeds(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		goose.SetDialect("postgres")
		goose.SetTableName(migrationTableCore)
		if err := goose.DownToContext(ctx, db, coreMigrationDir(t), 28); err != nil {
			t.Fatal(err)
		}
		var table sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT to_regclass('public.org_outbound_policy_state')`).Scan(&table); err != nil {
			t.Fatal(err)
		}
		if table.Valid {
			t.Fatalf("policy state table survived down: %s", table.String)
		}
	})
}

func TestAutonomousEnqueueFailsClosedOnSuspendedPolicy(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, inboxID := insertPolicyFenceTenant(t, ctx, db, "enqueue-suspended")
		seedPolicyFenceState(t, ctx, st, orgID)
		if err := st.RunInTx(ctx, func(scoped *Store) error {
			_, err := scoped.SetFeatureFlag(ctx, &orgID, "email_outbound_suspended", true, "test")
			return err
		}); err != nil {
			t.Fatal(err)
		}

		_, err := st.EnqueueOutboxMessage(ctx, OutboxMessage{
			OrgID: orgID, InboxID: inboxID, Provider: "smtp", IdempotencyKey: "suspended-enqueue",
			To: "to@example.test", From: "from@local.neuralmail", Subject: "subject", TextBody: "body",
			AutonomousLimits: &OutboundLimitInput{
				ToolName: "send_reply", IdempotencyKey: "suspended-enqueue", Recipient: "to@example.test",
			},
		})
		if !errors.Is(err, ErrOutboxPolicyRevoked) {
			t.Fatalf("enqueue error=%v, want policy revoked", err)
		}
		var rows int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM outbox_messages WHERE org_id = $1`, orgID).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 0 {
			t.Fatalf("suspended autonomous outbox rows=%d, want 0", rows)
		}
	})
}

func TestOutboundPolicyWriterAdvancesEpochAndDoesNotReviveQueuedMail(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, inboxID := insertPolicyFenceTenant(t, ctx, db, "writer-fence")
		seedPolicyFenceState(t, ctx, st, orgID)
		outboxID := insertPolicyFenceOutbox(t, ctx, db, orgID, inboxID, 1)

		for _, enabled := range []bool{true, false} {
			if err := st.RunInTx(ctx, func(scoped *Store) error {
				_, err := scoped.SetFeatureFlag(ctx, &orgID, "email_outbound_suspended", enabled, "test")
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		var epoch int64
		var status, lastError string
		if err := db.QueryRowContext(ctx, `SELECT policy_epoch FROM org_outbound_policy_state WHERE org_id = $1`, orgID).Scan(&epoch); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRowContext(ctx, `SELECT status, last_error FROM outbox_messages WHERE id = $1`, outboxID).Scan(&status, &lastError); err != nil {
			t.Fatal(err)
		}
		if epoch != 3 || status != "failed" || lastError != "policy_revoked" {
			t.Fatalf("epoch=%d status=%q error=%q", epoch, status, lastError)
		}
		claimed, err := st.ClaimOutboxMessages(ctx, 10, "writer-check", time.Now().UTC(), time.Minute)
		if err != nil || len(claimed) != 0 {
			t.Fatalf("cleared policy revived mail: claimed=%+v err=%v", claimed, err)
		}
	})
}

func TestComposePolicyWriterDoesNotRevokeQueuedReply(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, inboxID := insertPolicyFenceTenant(t, ctx, db, "compose-writer-fence")
		seedPolicyFenceState(t, ctx, st, orgID)
		outboxID := insertPolicyFenceOutbox(t, ctx, db, orgID, inboxID, 1)

		if err := st.RunInTx(ctx, func(scoped *Store) error {
			_, err := scoped.SetFeatureFlag(ctx, &orgID, "email_compose_org_enabled", true, "test")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var epoch int64
		var status string
		if err := db.QueryRowContext(ctx, `SELECT policy_epoch FROM org_outbound_policy_state WHERE org_id = $1`, orgID).Scan(&epoch); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRowContext(ctx, `SELECT status FROM outbox_messages WHERE id = $1`, outboxID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if epoch != 1 || status != "queued" {
			t.Fatalf("compose transition changed delivery fence: epoch=%d status=%q", epoch, status)
		}
	})
}

func TestCoreMigration29SupportsFrozenLegacyOutboxSQL(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		orgID, inboxID := insertPolicyFenceTenant(t, ctx, db, "legacy-runtime-sql")
		id := uuid.NewString()
		// Frozen v0.0.17-style insert: it names no Core 0029 column.
		if _, err := db.ExecContext(ctx, `
			INSERT INTO outbox_messages
			  (id, org_id, inbox_id, provider, idempotency_key, "to", "from", subject, text_body)
			VALUES ($1, $2, $3, 'smtp', $4, 'to@example.test', 'from@local.neuralmail', 'legacy', 'body')
		`, id, orgID, inboxID, "legacy-runtime:"+id); err != nil {
			t.Fatal(err)
		}
		// Frozen claim/update projection intentionally omits every new column.
		var claimedID, lockedBy string
		if err := db.QueryRowContext(ctx, `
			WITH picked AS (
			  SELECT id FROM outbox_messages
			  WHERE status = 'queued' AND next_attempt_at <= now()
			  ORDER BY next_attempt_at LIMIT 1 FOR UPDATE SKIP LOCKED
			)
			UPDATE outbox_messages o
			SET status = 'sending', locked_at = now(), locked_by = 'v0.0.17', attempt_count = attempt_count + 1
			FROM picked WHERE o.id = picked.id
			RETURNING o.id::text, o.locked_by
		`).Scan(&claimedID, &lockedBy); err != nil {
			t.Fatal(err)
		}
		if claimedID != id || lockedBy != "v0.0.17" {
			t.Fatalf("legacy claim id=%q worker=%q", claimedID, lockedBy)
		}
		if _, err := db.ExecContext(ctx, `
			UPDATE outbox_messages
			SET status = 'sent', provider_message_id = $2, locked_at = NULL, locked_by = NULL, terminal_at = now()
			WHERE id = $1
		`, id, "legacy-provider-id"); err != nil {
			t.Fatal(err)
		}
		assertPolicyFenceRow(t, ctx, db, id, "sent", false, false)
	})
}

func TestProviderFenceRejectsStaleWorkerAfterReclaim(t *testing.T) {
	for _, resolveBeforeCrash := range []bool{false, true} {
		t.Run(map[bool]string{false: "crash_after_provider_start", true: "crash_after_provider_response"}[resolveBeforeCrash], func(t *testing.T) {
			withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
				migrateToLatest(t, ctx, db)
				st := &Store{db: db, q: db}
				connA, err := db.Conn(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer connA.Close()
				connB, err := db.Conn(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer connB.Close()
				workerAStore := &Store{db: db, q: connA}
				workerBStore := &Store{db: db, q: connB}
				orgID, inboxID := insertPolicyFenceTenant(t, ctx, db, "reclaim-fence")
				seedPolicyFenceState(t, ctx, st, orgID)
				outboxID := insertPolicyFenceOutbox(t, ctx, db, orgID, inboxID, 1)
				t0 := time.Now().UTC()
				// Production Machines share one configured worker label. Each claim
				// must still receive a distinct persisted lease identity.
				workerA := claimOnePolicyFenceOutbox(t, ctx, workerAStore, outboxID, "nerve-runtime-worker")
				operationID, err := beginProviderOnConnection(ctx, db, connA, workerA)
				if err != nil {
					t.Fatal(err)
				}
				if resolveBeforeCrash {
					if err := workerAStore.ResolveOutboxProviderAttempt(ctx, outboxID, workerA.LockedBy.String, operationID); err != nil {
						t.Fatal(err)
					}
				}
				claimedB, err := workerBStore.ClaimOutboxMessages(ctx, 10, "nerve-runtime-worker", t0.Add(2*time.Minute), time.Minute)
				if err != nil || len(claimedB) != 1 {
					t.Fatalf("worker-b claim=%+v err=%v", claimedB, err)
				}
				if claimedB[0].LockedBy.String == workerA.LockedBy.String {
					t.Fatalf("stale reclaim reused lease %q", workerA.LockedBy.String)
				}
				replayedID, err := beginProviderOnConnection(ctx, db, connB, claimedB[0])
				if err != nil || replayedID != operationID {
					t.Fatalf("recovery operation=%q err=%v", replayedID, err)
				}
				if err := workerBStore.MarkClaimedOutboxMessageSent(ctx, outboxID, claimedB[0].LockedBy.String, operationID, "provider-id"); err != nil {
					t.Fatal(err)
				}
				if err := workerAStore.MarkClaimedOutboxProviderFailure(ctx, outboxID, workerA.LockedBy.String, operationID, "late failure"); !errors.Is(err, ErrOutboxClaimLost) {
					t.Fatalf("stale failure err=%v", err)
				}
				if err := workerAStore.RequeueClaimedOutboxMessage(ctx, outboxID, workerA.LockedBy.String, t0.Add(3*time.Minute), "late retry"); !errors.Is(err, ErrOutboxClaimLost) {
					t.Fatalf("stale requeue err=%v", err)
				}
				assertPolicyFenceRow(t, ctx, db, outboxID, "sent", true, true)
			})
		})
	}
}

func beginProviderOnConnection(ctx context.Context, db *sql.DB, conn *sql.Conn, msg OutboxMessage) (string, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	scoped := &Store{db: db, q: tx, inTx: true}
	operationID, err := scoped.BeginOutboxProviderOperation(ctx, msg)
	if err != nil {
		_ = tx.Rollback()
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return operationID, nil
}

func TestProviderStartFenceOrdersPolicyTransition(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, inboxID := insertPolicyFenceTenant(t, ctx, db, "provider-fence")
		seedPolicyFenceState(t, ctx, st, orgID)

		t.Run("policy transition first refuses provider start", func(t *testing.T) {
			outboxID := insertPolicyFenceOutbox(t, ctx, db, orgID, inboxID, 1)
			claimed := claimOnePolicyFenceOutbox(t, ctx, st, outboxID, "policy-first")
			if err := st.RunInTx(ctx, func(scoped *Store) error {
				_, terminalized, err := scoped.AdvanceOutboundPolicyEpoch(ctx, orgID)
				if terminalized != 1 {
					t.Fatalf("terminalized pre-provider claim=%d, want 1", terminalized)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := st.BeginOutboxProviderOperation(ctx, claimed); !errors.Is(err, ErrOutboxPolicyRevoked) {
				t.Fatalf("begin error=%v, want policy revoked", err)
			}
			assertPolicyFenceRow(t, ctx, db, outboxID, "failed", false, false)
		})

		t.Run("provider start first remains unresolved", func(t *testing.T) {
			// Use the current epoch left by the preceding transition.
			outboxID := insertPolicyFenceOutbox(t, ctx, db, orgID, inboxID, 2)
			claimed := claimOnePolicyFenceOutbox(t, ctx, st, outboxID, "provider-first")
			operationID, err := st.BeginOutboxProviderOperation(ctx, claimed)
			if err != nil {
				t.Fatal(err)
			}
			if operationID != "outbox:"+outboxID {
				t.Fatalf("operation id=%q", operationID)
			}
			if err := st.RunInTx(ctx, func(scoped *Store) error {
				_, terminalized, err := scoped.AdvanceOutboundPolicyEpoch(ctx, orgID)
				if terminalized != 0 {
					t.Fatalf("terminalized unresolved provider start=%d", terminalized)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			assertPolicyFenceRow(t, ctx, db, outboxID, "sending", true, false)
			// Same claimed operation remains the only authorized recovery identity.
			replayedID, err := st.BeginOutboxProviderOperation(ctx, claimed)
			if err != nil || replayedID != operationID {
				t.Fatalf("recovery begin id=%q err=%v", replayedID, err)
			}
		})
	})
}

func TestClaimAndSuspensionBarrierBothCommitOrders(t *testing.T) {
	for _, suspensionFirst := range []bool{false, true} {
		name := map[bool]string{false: "claim_commits_first", true: "suspension_commits_first"}[suspensionFirst]
		t.Run(name, func(t *testing.T) {
			withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
				migrateToLatest(t, ctx, db)
				st := &Store{db: db, q: db}
				orgID, inboxID := insertPolicyFenceTenant(t, ctx, db, "claim-suspension-"+name)
				seedPolicyFenceState(t, ctx, st, orgID)
				outboxID := insertPolicyFenceOutbox(t, ctx, db, orgID, inboxID, 1)

				if suspensionFirst {
					tx, err := db.BeginTx(ctx, nil)
					if err != nil {
						t.Fatal(err)
					}
					scoped := &Store{db: db, q: tx, inTx: true}
					if _, err := scoped.SetFeatureFlag(ctx, &orgID, "email_outbound_suspended", true, "close-test"); err != nil {
						_ = tx.Rollback()
						t.Fatal(err)
					}
					claimDone := make(chan struct {
						messages []OutboxMessage
						err      error
					}, 1)
					go func() {
						messages, claimErr := st.ClaimOutboxMessages(ctx, 10, "shared-worker", time.Now().UTC(), time.Minute)
						claimDone <- struct {
							messages []OutboxMessage
							err      error
						}{messages, claimErr}
					}()
					var earlyClaim *struct {
						messages []OutboxMessage
						err      error
					}
					select {
					case result := <-claimDone:
						// SKIP LOCKED may make the concurrent claim return empty
						// immediately instead of waiting on the suspension's row lock.
						if result.err != nil || len(result.messages) != 0 {
							_ = tx.Rollback()
							t.Fatalf("claim crossed suspension transaction: messages=%+v err=%v", result.messages, result.err)
						}
						earlyClaim = &result
					case <-time.After(100 * time.Millisecond):
					}
					if err := tx.Commit(); err != nil {
						t.Fatal(err)
					}
					if earlyClaim == nil {
						result := <-claimDone
						if result.err != nil || len(result.messages) != 0 {
							t.Fatalf("post-suspension claim=%+v err=%v", result.messages, result.err)
						}
					}
					assertPolicyFenceRow(t, ctx, db, outboxID, "failed", false, false)
					return
				}

				tx, err := db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				claimStore := &Store{db: db, q: tx, inTx: true}
				claimed, err := claimStore.ClaimOutboxMessages(ctx, 10, "shared-worker", time.Now().UTC(), time.Minute)
				if err != nil || len(claimed) != 1 {
					_ = tx.Rollback()
					t.Fatalf("claim=%+v err=%v", claimed, err)
				}
				suspendDone := make(chan error, 1)
				go func() {
					suspendDone <- st.RunInTx(ctx, func(scoped *Store) error {
						_, setErr := scoped.SetFeatureFlag(ctx, &orgID, "email_outbound_suspended", true, "close-test")
						return setErr
					})
				}()
				select {
				case err := <-suspendDone:
					_ = tx.Rollback()
					t.Fatalf("suspension did not wait for claimed row: %v", err)
				case <-time.After(100 * time.Millisecond):
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				if err := <-suspendDone; err != nil {
					t.Fatal(err)
				}
				if _, err := st.BeginOutboxProviderOperation(ctx, claimed[0]); !errors.Is(err, ErrOutboxPolicyRevoked) {
					t.Fatalf("provider start after suspension error=%v", err)
				}
				assertPolicyFenceRow(t, ctx, db, outboxID, "failed", false, false)
			})
		})
	}
}

func TestProviderStartAndCloseBarrierBothCommitOrders(t *testing.T) {
	for _, closeFirst := range []bool{false, true} {
		name := map[bool]string{false: "provider_start_commits_first", true: "close_commits_first"}[closeFirst]
		t.Run(name, func(t *testing.T) {
			withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
				migrateToLatest(t, ctx, db)
				st := &Store{db: db, q: db}
				orgID, inboxID := insertPolicyFenceTenant(t, ctx, db, "provider-close-"+name)
				seedPolicyFenceState(t, ctx, st, orgID)
				outboxID := insertPolicyFenceOutbox(t, ctx, db, orgID, inboxID, 1)
				claimed := claimOnePolicyFenceOutbox(t, ctx, st, outboxID, "shared-worker")

				if closeFirst {
					closeTx, err := db.BeginTx(ctx, nil)
					if err != nil {
						t.Fatal(err)
					}
					closeStore := &Store{db: db, q: closeTx, inTx: true}
					if _, _, err := closeStore.AdvanceOutboundPolicyEpoch(ctx, orgID); err != nil {
						_ = closeTx.Rollback()
						t.Fatal(err)
					}
					startDone := make(chan error, 1)
					go func() {
						_, startErr := st.BeginOutboxProviderOperation(ctx, claimed)
						startDone <- startErr
					}()
					select {
					case err := <-startDone:
						_ = closeTx.Rollback()
						t.Fatalf("provider start did not wait for close barrier: %v", err)
					case <-time.After(100 * time.Millisecond):
					}
					if err := closeTx.Commit(); err != nil {
						t.Fatal(err)
					}
					if err := <-startDone; !errors.Is(err, ErrOutboxPolicyRevoked) {
						t.Fatalf("provider start after close error=%v", err)
					}
					assertPolicyFenceRow(t, ctx, db, outboxID, "failed", false, false)
					return
				}

				startTx, err := db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				startStore := &Store{db: db, q: startTx, inTx: true}
				operation, err := startStore.BeginOutboxProviderOperationState(ctx, claimed)
				if err != nil {
					_ = startTx.Rollback()
					t.Fatal(err)
				}
				closeDone := make(chan error, 1)
				go func() {
					closeDone <- st.RunInTx(ctx, func(scoped *Store) error {
						_, _, closeErr := scoped.AdvanceOutboundPolicyEpoch(ctx, orgID)
						return closeErr
					})
				}()
				select {
				case err := <-closeDone:
					_ = startTx.Rollback()
					t.Fatalf("close did not wait for provider-start barrier: %v", err)
				case <-time.After(100 * time.Millisecond):
				}
				if err := startTx.Commit(); err != nil {
					t.Fatal(err)
				}
				if err := <-closeDone; err != nil {
					t.Fatal(err)
				}
				if operation.ID != "outbox:"+outboxID || operation.StartedAt.IsZero() {
					t.Fatalf("provider operation=%+v", operation)
				}
				assertPolicyFenceRow(t, ctx, db, outboxID, "sending", true, false)
			})
		})
	}
}

func TestKnownProviderFailureResolvesAndObservesSuspensionAtomically(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, inboxID := insertPolicyFenceTenant(t, ctx, db, "known-provider-suspension")
		seedPolicyFenceState(t, ctx, st, orgID)
		outboxID := insertPolicyFenceOutbox(t, ctx, db, orgID, inboxID, 1)
		claimed := claimOnePolicyFenceOutbox(t, ctx, st, outboxID, "known-provider-worker")
		operationID, err := st.BeginOutboxProviderOperation(ctx, claimed)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.RunInTx(ctx, func(scoped *Store) error {
			_, setErr := scoped.SetFeatureFlag(ctx, &orgID, "email_outbound_suspended", true, "test")
			return setErr
		}); err != nil {
			t.Fatal(err)
		}
		if err := st.RequeueClaimedOutboxKnownProviderFailure(
			ctx, outboxID, claimed.LockedBy.String, operationID, time.Now().UTC().Add(time.Minute), "rate_limited",
		); err != nil {
			t.Fatal(err)
		}
		assertPolicyFenceRow(t, ctx, db, outboxID, "failed", true, true)
		var lastError string
		if err := db.QueryRowContext(ctx, `SELECT last_error FROM outbox_messages WHERE id = $1`, outboxID).Scan(&lastError); err != nil {
			t.Fatal(err)
		}
		if lastError != "policy_revoked" {
			t.Fatalf("last_error=%q", lastError)
		}
	})
}

func insertPolicyFenceTenant(t *testing.T, ctx context.Context, db *sql.DB, name string) (string, string) {
	t.Helper()
	orgID, inboxID := uuid.NewString(), uuid.NewString()
	if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, $2)`, orgID, name); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO inboxes (id, org_id, address, status)
		VALUES ($1, $2, $3, 'active')
	`, inboxID, orgID, name+"@local.neuralmail"); err != nil {
		t.Fatal(err)
	}
	return orgID, inboxID
}

func insertPolicyFenceOutbox(t *testing.T, ctx context.Context, db *sql.DB, orgID, inboxID string, epoch int64) string {
	t.Helper()
	id := uuid.NewString()
	var savedEpoch any
	if epoch > 0 {
		savedEpoch = epoch
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO outbox_messages (
		  id, org_id, inbox_id, provider, idempotency_key, "to", "from", subject,
		  text_body, autonomous_policy_epoch
		) VALUES ($1, $2, $3, 'smtp', $4, 'to@local.neuralmail', 'from@local.neuralmail', 'subject', 'body', $5)
	`, id, orgID, inboxID, "policy:"+id, savedEpoch); err != nil {
		t.Fatal(err)
	}
	return id
}

func seedPolicyFenceState(t *testing.T, ctx context.Context, st *Store, orgID string) {
	t.Helper()
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
		t.Fatal(err)
	}
}

func claimOnePolicyFenceOutbox(t *testing.T, ctx context.Context, st *Store, id, worker string) OutboxMessage {
	t.Helper()
	claimed, err := st.ClaimOutboxMessages(ctx, 10, worker, time.Now().UTC(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range claimed {
		if msg.ID == id {
			return msg
		}
	}
	t.Fatalf("outbox %s not claimed: %+v", id, claimed)
	return OutboxMessage{}
}

func assertPolicyFenceRow(t *testing.T, ctx context.Context, db *sql.DB, id, wantStatus string, wantStarted, wantResolved bool) {
	t.Helper()
	var status string
	var started, resolved sql.NullTime
	if err := db.QueryRowContext(ctx, `
		SELECT status, provider_started_at, provider_resolved_at
		FROM outbox_messages WHERE id = $1
	`, id).Scan(&status, &started, &resolved); err != nil {
		t.Fatal(err)
	}
	if status != wantStatus || started.Valid != wantStarted || resolved.Valid != wantResolved {
		t.Fatalf("status=%q started=%v resolved=%v, want %q/%v/%v", status, started.Valid, resolved.Valid, wantStatus, wantStarted, wantResolved)
	}
}

func containsError(err error, fragment string) bool {
	return err != nil && fragment != "" && strings.Contains(err.Error(), fragment)
}
