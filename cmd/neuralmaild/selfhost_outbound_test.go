package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"neuralmail/internal/app"
	"neuralmail/internal/config"
	"neuralmail/internal/emailtransport"
	"neuralmail/internal/store"
	"neuralmail/internal/tools"
)

func selfhostApp(t *testing.T) *app.App {
	t.Helper()
	dsn := os.Getenv("NM_TEST_DB_DSN")
	if dsn == "" {
		dsn = "postgres://neuralmail:neuralmail@127.0.0.1:54320/neuralmail?sslmode=disable"
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/postgres"
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		if os.Getenv("NM_REQUIRE_DB") == "1" {
			t.Fatal(err)
		}
		t.Skipf("PostgreSQL unavailable: %v", err)
	}
	name := "selfhost_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := db.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1`, name)
		_, _ = db.Exec("DROP DATABASE " + name)
	})
	u.Path = "/" + name
	cfg := config.Default()
	cfg.Database.DSN = u.String()
	cfg.Redis.URL = "redis://127.0.0.1:6379/0"
	cfg.Policy.DefaultPath = filepath.Join("..", "..", "configs", "policy", "support-default-v1.yaml")
	cfg.Metering.ToolCostPath = filepath.Join("..", "..", "configs", "meters", "tool_costs.yaml")
	cfg.Security.AllowOutbound = true
	a, err := app.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

type selfhostDeliveryProbe struct {
	count atomic.Int32
	send  func(context.Context) error
}

func (p *selfhostDeliveryProbe) Name() string { return "smtp" }
func (p *selfhostDeliveryProbe) SendMessage(ctx context.Context, _ emailtransport.OutboundMessage, key string) (string, error) {
	p.count.Add(1)
	if p.send != nil {
		if err := p.send(ctx); err != nil {
			return "", err
		}
	}
	return key, nil
}

func selfhostTool(t *testing.T, a *app.App, name, key string) func() (any, error) {
	t.Helper()
	ctx := context.Background()
	ids, err := a.Store.ListInboxes(ctx)
	if err != nil || len(ids) != 1 {
		t.Fatalf("inboxes: %v %v", ids, err)
	}
	inbox := ids[0]
	if name == "compose_email" {
		return func() (any, error) {
			return a.MCP.Tools.ComposeEmail(ctx, inbox, "sender@example.test", "subject "+key, "hello", "", key)
		}
	}
	thread, _, err := a.Store.InsertMessageWithThread(ctx, inbox, "incoming-"+key, store.Message{
		Direction: "inbound", Subject: "question", Text: "hello", CreatedAt: time.Now().UTC(),
		From: store.Participant{Email: "sender@example.test"}, To: []store.Participant{{Email: a.Config.SMTP.From}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return func() (any, error) { return a.MCP.Tools.SendReply(ctx, thread, "reply", "", false, key) }
}

func waitSelfhostStatus(t *testing.T, a *app.App, id, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		if err := a.Store.DB().QueryRow(`SELECT status FROM outbox_messages WHERE id=$1`, id).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("outbox %s did not become %s", id, want)
}

func TestSelfhostBothToolsRemainIdempotentAcrossConcurrentWorkers(t *testing.T) {
	for _, name := range []string{"compose_email", "send_reply"} {
		t.Run(name, func(t *testing.T) {
			a := selfhostApp(t)
			probe := &selfhostDeliveryProbe{}
			_ = a.EmailTransport.RegisterOutbound(probe)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var workers sync.WaitGroup
			for i := 0; i < 2; i++ {
				w := emailtransport.NewOutboxWorker(a.Store, a.EmailTransport, fmt.Sprintf("worker-%d", i), a.MCP.MemoryBudget)
				w.PollInterval = 10 * time.Millisecond
				workers.Add(1)
				go func() { defer workers.Done(); _ = w.Run(ctx) }()
			}
			defer func() { cancel(); workers.Wait() }()
			call := selfhostTool(t, a, name, "same-request")
			type response struct {
				result any
				err    error
			}
			responses := make(chan response, 8)
			for i := 0; i < 8; i++ {
				go func() { r, e := call(); responses <- response{r, e} }()
			}
			id := ""
			for i := 0; i < 8; i++ {
				r := <-responses
				if r.err != nil {
					t.Fatal(r.err)
				}
				got := r.result.(map[string]any)["message_id"].(string)
				if id != "" && got != id {
					t.Fatal("retry returned a new message")
				}
				id = got
			}
			waitSelfhostStatus(t, a, id, "sent")
			r, err := call()
			if err != nil {
				t.Fatalf("retry after delivery: %v", err)
			}
			if r.(map[string]any)["message_id"] != id {
				t.Fatal("retry after delivery changed message")
			}
			cancel()
			workers.Wait()
			if probe.count.Load() != 1 {
				t.Fatalf("provider called %d times", probe.count.Load())
			}
			var count int
			if err := a.Store.DB().QueryRow(`SELECT count(*) FROM outbox_messages`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("outbox count=%d err=%v", count, err)
			}
		})
	}
}

func TestSelfhostOutboxAndVisibleMessageRollbackTogether(t *testing.T) {
	for _, name := range []string{"compose_email", "send_reply"} {
		t.Run(name, func(t *testing.T) {
			a := selfhostApp(t)
			call := selfhostTool(t, a, name, "rollback")
			_, err := a.Store.DB().Exec(`CREATE FUNCTION reject_test_outbound() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.direction='outbound' THEN RAISE EXCEPTION 'test rejection'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_outbound BEFORE INSERT ON messages FOR EACH ROW EXECUTE FUNCTION reject_test_outbound()`)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := call(); err == nil {
				t.Fatal("expected visible-message failure")
			}
			var count int
			if err := a.Store.DB().QueryRow(`SELECT count(*) FROM outbox_messages`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("partial outbox count=%d err=%v", count, err)
			}
		})
	}
}

func TestSelfhostBothToolsRejectSetupBeforeEnqueue(t *testing.T) {
	for _, name := range []string{"compose_email", "send_reply"} {
		t.Run(name, func(t *testing.T) {
			a := selfhostApp(t)
			call := selfhostTool(t, a, name, "setup")
			for _, code := range []string{"outbound_disabled", "smtp_unconfigured"} {
				a.MCP.Tools.Config.Security.AllowOutbound = code != "outbound_disabled"
				a.MCP.Tools.Config.SMTP.Host = ""
				_, err := call()
				var setup *tools.OutboundConfigurationError
				if !errors.As(err, &setup) || setup.Code != code {
					t.Fatalf("want %s got %v", code, err)
				}
			}
			var count int
			if err := a.Store.DB().QueryRow(`SELECT count(*) FROM outbox_messages`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("setup failure enqueued %d: %v", count, err)
			}
		})
	}
}

func TestSelfhostWorkerFinishesAcknowledgementBeforeStopping(t *testing.T) {
	a := selfhostApp(t)
	first := selfhostTool(t, a, "compose_email", "first")
	result, err := first()
	if err != nil {
		t.Fatal(err)
	}
	firstID := result.(map[string]any)["message_id"].(string)
	second := selfhostTool(t, a, "compose_email", "second")
	result, err = second()
	if err != nil {
		t.Fatal(err)
	}
	secondID := result.(map[string]any)["message_id"].(string)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	probe := &selfhostDeliveryProbe{send: func(ctx context.Context) error { close(entered); <-release; return ctx.Err() }}
	_ = a.EmailTransport.RegisterOutbound(probe)
	workerCtx, stop := context.WithCancel(context.Background())
	defer stop()
	worker := emailtransport.NewOutboxWorker(a.Store, a.EmailTransport, "drain-test", a.MCP.MemoryBudget)
	worker.PollInterval = time.Millisecond
	done := make(chan error, 1)
	go func() { done <- worker.Run(workerCtx) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("delivery not started")
	}
	stop()
	select {
	case <-done:
		t.Fatal("worker returned before provider completed")
	default:
	}
	once.Do(func() { close(release) })
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not drain")
	}
	waitSelfhostStatus(t, a, firstID, "sent")
	waitSelfhostStatus(t, a, secondID, "queued")
	if probe.count.Load() != 1 {
		t.Fatal("started another delivery during shutdown")
	}
	var locked bool
	if err := a.Store.DB().QueryRow(`SELECT locked_by IS NOT NULL FROM outbox_messages WHERE id=$1`, secondID).Scan(&locked); err != nil || locked {
		t.Fatalf("undelivered claim retained: %v %v", locked, err)
	}
}

func (p *selfhostDeliveryProbe) GetDeliveryStatus(context.Context, string) (emailtransport.DeliveryStatus, error) {
	return emailtransport.DeliveryStatus("unknown"), nil
}
