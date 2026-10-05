package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"neuralmail/internal/auth"
	"neuralmail/internal/config"
	"neuralmail/internal/emailtransport"
	"neuralmail/internal/store"
	"neuralmail/internal/tools"
)

// These are the actual modern tool service entrypoints, not calls directly to
// the shared enqueue helper. Both paths include a real attachment and replay.
func TestModernComposeAndReplyShareRecipientAllowanceWithAttachments(t *testing.T) {
	withRecipientToolStore(t, func(ctx context.Context, st *store.Store) {
		org, inbox := uuid.NewString(), uuid.NewString()
		if _, err := st.DB().ExecContext(ctx, `INSERT INTO orgs(id,name) VALUES($1,'modern tool meter')`, org); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().ExecContext(ctx, `INSERT INTO inboxes(id,org_id,address,status) VALUES($1,$2,'agent@local.neuralmail','active')`, inbox, org); err != nil {
			t.Fatal(err)
		}
		period := store.RecipientPeriod{OrgID: org, PeriodID: uuid.NewString(), StartsAt: time.Now().UTC().Add(-time.Hour), EndsAt: time.Now().UTC().Add(time.Hour), Limit: sql.NullInt64{Int64: 2, Valid: true}}
		if err := st.RunAsOrg(ctx, org, func(tx *store.Store) error {
			for flag, value := range map[string]bool{"autonomous_outbound_policy": true, "email_outbound_suspended": false, "email_compose_org_enabled": true} {
				if _, err := tx.SetFeatureFlag(ctx, &org, flag, value, "test"); err != nil {
					return err
				}
			}
			return tx.InstallRecipientPeriod(ctx, period)
		}); err != nil {
			t.Fatal(err)
		}
		var thread string
		if err := st.RunAsOrg(ctx, org, func(tx *store.Store) error {
			var err error
			thread, _, err = tx.InsertMessageWithThread(ctx, inbox, "real-inbound-thread", store.Message{ID: uuid.NewString(), InboxID: inbox, Direction: "inbound", Subject: "question", Text: "inbound", CreatedAt: time.Now().UTC(), ReceivedEmailID: "synthetic-durable-envelope", From: store.Participant{Email: "reply@example.test"}, To: []store.Participant{{Email: "agent@local.neuralmail"}}})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		cfg := config.Default()
		cfg.Cloud.Mode = true
		cfg.Security.AllowOutbound = true
		registry := emailtransport.NewRegistry()
		if err := registry.RegisterOutbound(recipientToolNoSendAdapter{}); err != nil {
			t.Fatal(err)
		}
		svc := &tools.Service{Config: cfg, Store: st, Transport: registry}
		caller := auth.WithPrincipal(ctx, auth.Principal{Kind: auth.PrincipalM2MOrg, OrgID: org})
		attachment := []store.OutboundAttachment{{Filename: "proof.txt", ContentType: "text/plain", Content: []byte("retained outbound bytes")}}
		firstIDs := map[string]string{}
		for _, kind := range []string{"compose", "reply"} {
			for replay := 0; replay < 2; replay++ {
				var result any
				var err error
				if kind == "compose" {
					result, err = svc.ComposeEmailWithOptions(caller, inbox, "compose@example.test", "question", "body", "", "compose-meter", tools.ComposeEmailOptions{Attachments: attachment})
				} else {
					result, err = svc.SendReplyWithAttachments(caller, thread, "answer", "", false, "reply-meter", attachment)
				}
				if err != nil {
					t.Fatalf("%s replay%d: %v", kind, replay, err)
				}
				out, ok := result.(map[string]any)
				if !ok {
					t.Fatalf("unexpected tool result: %T", result)
				}
				id, ok := out["message_id"].(string)
				if !ok || id == "" {
					t.Fatalf("missing %s message identity", kind)
				}
				if replay == 0 {
					firstIDs[kind] = id
				} else if id != firstIDs[kind] {
					t.Fatal("tool replay created another message")
				}
			}
		}
		if firstIDs["compose"] == firstIDs["reply"] {
			t.Fatal("distinct sends shared message identity")
		}
		for _, kind := range []string{"compose", "reply"} {
			var err error
			if kind == "compose" {
				_, err = svc.ComposeEmailWithOptions(caller, inbox, "other@example.test", "extra", "body", "", "compose-over", tools.ComposeEmailOptions{Attachments: attachment})
			} else {
				_, err = svc.SendReplyWithAttachments(caller, thread, "extra", "", false, "reply-over", attachment)
			}
			if !errors.Is(err, store.ErrRecipientAllowanceExhausted) {
				t.Fatalf("%s bypassed shared allowance: %v", kind, err)
			}
		}
		var reserved, committed int64
		var outboxes, attachments, messages int
		if err := st.DB().QueryRowContext(ctx, `SELECT p.reserved,p.committed,
   (SELECT count(*) FROM outbox_messages WHERE org_id=$1::uuid),
   (SELECT count(*) FROM outbox_attachments WHERE org_id=$1::uuid),
   (SELECT count(*) FROM messages WHERE org_id=$1::uuid AND direction='outbound')
   FROM org_recipient_periods p WHERE p.org_id=$1::uuid AND p.period_id=$2::uuid`, org, period.PeriodID).Scan(&reserved, &committed, &outboxes, &attachments, &messages); err != nil {
			t.Fatal(err)
		}
		if reserved != 2 || committed != 0 || outboxes != 2 || attachments != 2 || messages != 2 {
			t.Fatalf("tool quota/replay mutated graph: %d/%d outbox=%d attachments=%d messages=%d", reserved, committed, outboxes, attachments, messages)
		}
	})
}

type recipientToolNoSendAdapter struct{}

func (recipientToolNoSendAdapter) Name() string { return "smtp" }
func (recipientToolNoSendAdapter) SendMessage(context.Context, emailtransport.OutboundMessage, string) (string, error) {
	return "", errors.New("provider call forbidden in tool enqueue test")
}
func (recipientToolNoSendAdapter) GetDeliveryStatus(context.Context, string) (emailtransport.DeliveryStatus, error) {
	return emailtransport.DeliveryStatusUnknown, emailtransport.ErrNotSupported
}

func withRecipientToolStore(t *testing.T, run func(context.Context, *store.Store)) {
	t.Helper()
	dsn := os.Getenv("NM_TEST_DB_DSN")
	if dsn == "" {
		dsn = "postgres://neuralmail:neuralmail@127.0.0.1:54320/neuralmail?sslmode=disable"
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/postgres"
	admin, err := sql.Open("pgx", parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		if os.Getenv("NM_REQUIRE_DB") == "1" {
			t.Fatalf("PostgreSQL required: %v", err)
		}
		t.Skipf("PostgreSQL unavailable: %v", err)
	}
	name := "nerve_tools_meter_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", name)); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = admin.ExecContext(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1`, name)
		if _, err := admin.ExecContext(context.Background(), fmt.Sprintf("DROP DATABASE %s", name)); err != nil {
			t.Error(err)
		}
	}()
	parsed.Path = "/" + name
	st, err := store.Open(parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := store.MigrateCore(ctx, st.DB()); err != nil {
		t.Fatal(err)
	}
	run(ctx, st)
}
