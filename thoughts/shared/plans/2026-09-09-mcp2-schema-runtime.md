package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"neuralmail/internal/app"
	"neuralmail/internal/config"
	"neuralmail/internal/emailtransport"
	resendtransport "neuralmail/internal/emailtransport/providers/resend"
	smtptransport "neuralmail/internal/emailtransport/providers/smtp"
	"neuralmail/internal/embed"
	"neuralmail/internal/mcp"
	"neuralmail/internal/memguard"
	"neuralmail/internal/queue"
	"neuralmail/internal/release"
	"neuralmail/internal/startup"
	"neuralmail/internal/store"
	"neuralmail/internal/vector"
)

func main() {
 if handled, err := release.HandleRuntimeCompatibility(os.Args[1:], os.Stdout); handled {
  if err != nil { log.Fatalf("runtime compatibility: %v", err) }
  return
 }

	if len(os.Args) < 2 {
		usage()
		return
	}
	cmd := os.Args[1]
	cfgPath := config.ConfigPathFromEnv()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("config error: %v", err)
	}
	release.LogRuntimeBanner(log.Default(), cfg.Cloud.Mode)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if handled, err := startup.SchemaQuiescence(ctx, os.Getenv("NERVE_SCHEMA_TRANSITION_MODE"), cfg.Cloud.Mode, cmd, cfg.HTTP.Addr); handled {
		if err != nil {
			log.Fatalf("schema quiescence: %v", err)
		}
		return
	}

	switch cmd {
	case "serve":
		withWorker, err := serveWorkerOption(cfg.Cloud.Mode, os.Args[2:])
		if err != nil {
			log.Fatalf("serve options: %v", err)
		}
		if err := runServe(ctx, cfg, withWorker); err != nil {
			log.Fatalf("serve: %v", err)
		}
	case "worker":
		runWorker(ctx, cfg)
	case "mcp-stdio":
		runStdio(ctx, cfg)
	case "migrate-core":
		runMigrations(ctx, cfg, migrateCoreToRuntimeWindow)
	case "migrate-cloud":
		runMigrations(ctx, cfg, migrateCloudToRuntimeWindow)
	case "migrate-all":
		runMigrations(ctx, cfg, migrateAllToRuntimeWindow)
	default:
		usage()
	}
}

func migrateCoreToRuntimeWindow(ctx context.Context, db *sql.DB) error {
	return store.MigrateUpToCore(ctx, db, startup.CoreMaxSupported)
}

func migrateCloudToRuntimeWindow(ctx context.Context, db *sql.DB) error {
	return store.MigrateUpToCloud(ctx, db, startup.RuntimeCloudMaxSupported)
}

func migrateAllToRuntimeWindow(ctx context.Context, db *sql.DB) error {
	if err := migrateCoreToRuntimeWindow(ctx, db); err != nil {
		return err
	}
	return migrateCloudToRuntimeWindow(ctx, db)
}

func serveWorkerOption(cloudMode bool, args []string) (bool, error) {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	withWorker := flags.Bool("with-worker", !cloudMode, "run the outbox worker alongside HTTP")
	if err := flags.Parse(args); err != nil {
		return false, err
	}
	if flags.NArg() != 0 {
		return false, errors.New("unexpected serve arguments")
	}
	return *withWorker, nil
}

func runServe(ctx context.Context, cfg config.Config, withWorker bool) error {
	if err := cfg.ValidateSelfhost(); err != nil {
		return err
	}
	appInstance, err := app.New(ctx, cfg)
	if err != nil {
		return fmt.Errorf("app init: %w", err)
	}
	defer appInstance.Close()
	inboxAddr := cfg.SMTP.From
	if inboxAddr == "" {
		inboxAddr = "dev@local.nerve.email"
	}
	inboxID, err := appInstance.Store.EnsureDefaults(ctx, inboxAddr)
	if err != nil {
		return fmt.Errorf("default inbox: %w", err)
	}
	loops := []serveLoop{
		{name: "HTTP", run: appInstance.Serve},
		{name: "poll", run: func(ctx context.Context) error { return appInstance.PollLoop(ctx, inboxID) }},
	}
	if withWorker {
		worker := emailtransport.NewOutboxWorker(appInstance.Store, appInstance.EmailTransport, "nerve-runtime-serve", appInstance.MCP.MemoryBudget)
		loops = append(loops, serveLoop{name: "outbox", run: worker.Run})
	}
	log.Printf("nerve-runtime serving on %s (outbox worker: %t)", cfg.HTTP.Addr, withWorker)
	// All loops join before the deferred Close releases the shared store/queue.
	return runServeLoops(ctx, loops)
}

func runWorker(ctx context.Context, cfg config.Config) {
	storeInstance, err := store.Open(cfg.Database.DSN)
	if err != nil {
		log.Fatalf("store error: %v", err)
	}
	defer storeInstance.Close()
	if err := startup.Migrate(ctx, storeInstance.DB(), cfg.Cloud.Mode); err != nil {
		log.Fatalf("migration error: %v", err)
	}
	// Record whether the live Core schema carries the 0029 outbound fence
	// before any worker claims outbox work. Artifact B spans Core [28,29];
	// on Core 28 the legacy outbox path must not emit fence columns.
	if err := storeInstance.RefreshOutboundFenceCapability(ctx); err != nil {
		log.Fatalf("detect outbound fence capability: %v", err)
	}
	queueInstance, err := queue.New(cfg.Redis.URL)
	if err != nil {
		log.Fatalf("queue error: %v", err)
	}
	defer queueInstance.Close()

	var embedder embed.Provider
	switch cfg.Embedding.Provider {
	case "openai":
		embedder = embed.NewOpenAI(cfg.LLM.OpenAIKey, cfg.Embedding.Model, cfg.Embedding.Dim)
	case "ollama":
		embedder = embed.NewOllama(cfg.LLM.OllamaURL, cfg.Embedding.Model, cfg.Embedding.Dim)
	default:
		embedder = embed.NewNoop(cfg.Embedding.Dim)
	}
	vecStore := vector.NewQdrant(cfg.Qdrant.URL, cfg.Qdrant.Collection)
	if err := vecStore.EnsureCollection(ctx, cfg.Embedding.Dim); err != nil {
		log.Printf("qdrant ensure collection failed: %v", err)
	}

	transportRegistry := emailtransport.NewRegistry()
	_ = transportRegistry.RegisterOutbound(smtptransport.NewOutboundAdapter(smtptransport.Config{
		Host:            cfg.SMTP.Host,
		Port:            cfg.SMTP.Port,
		Username:        cfg.SMTP.Username,
		Password:        cfg.SMTP.Password,
		RequireStartTLS: cfg.SMTP.RequireStartTLS,
		HeloDomain:      cfg.SMTP.HeloDomain,
	}))
	if strings.TrimSpace(cfg.Resend.APIKey) != "" {
		_ = transportRegistry.RegisterOutbound(resendtransport.NewOutboundAdapter(resendtransport.Config{
			APIKey:  cfg.Resend.APIKey,
			BaseURL: cfg.Resend.BaseURL,
		}))
	}
	memoryBudget, err := memguard.New(cfg.Memory.BudgetBytes)
	if err != nil {
		log.Fatalf("memory budget error: %v", err)
	}
	outboxWorker := emailtransport.NewOutboxWorker(storeInstance, transportRegistry, "nerve-runtime-worker", memoryBudget)
	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	defer func() { stopWorker(); <-workerDone }()
	go func() {
		defer close(workerDone)
		if err := outboxWorker.Run(workerCtx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("outbox worker stopped: %v", err)
		}
	}()

	log.Println("worker started")
	for {
		select {
		case <-ctx.Done():
			return
		default:
			job, err := queueInstance.PopEmbeddingJob(ctx, 5*time.Second)
			if err != nil {
				continue
			}
			msg, err := storeInstance.GetMessage(ctx, job)
			if err != nil {
				log.Printf("worker message fetch failed: %v", err)
				continue
			}
			inboxID, err := storeInstance.GetThreadInboxID(ctx, msg.ThreadID)
			if err != nil {
				log.Printf("worker thread fetch failed: %v", err)
				continue
			}
			vecs, err := embedder.Embed(ctx, []string{msg.Text})
			if err != nil || len(vecs) == 0 {
				log.Printf("embedding failed: %v", err)
				continue
			}
			point := vector.Point{
				ID:     msg.ID,
				Vector: vecs[0],
				Payload: map[string]any{
					"message_id": msg.ID,
					"thread_id":  msg.ThreadID,
					"inbox_id":   inboxID,
					"snippet":    snippet(msg.Text),
				},
			}
			if err := vecStore.Upsert(ctx, []vector.Point{point}); err != nil {
				log.Printf("qdrant upsert failed: %v", err)
				continue
			}
			log.Printf("processed embedding job: %s", job)
		}
	}
}

func runStdio(ctx context.Context, cfg config.Config) {
	appInstance, err := app.New(ctx, cfg)
	if err != nil {
		log.Fatalf("app init error: %v", err)
	}
	defer appInstance.Close()
	if err := mcp.RunStdio(ctx, appInstance.MCP); err != nil {
		log.Fatalf("stdio error: %v", err)
	}
}

func runMigrations(ctx context.Context, cfg config.Config, migrateFn func(context.Context, *sql.DB) error) {
	storeInstance, err := store.Open(cfg.Database.DSN)
	if err != nil {
		log.Fatalf("store error: %v", err)
	}
	defer storeInstance.Close()
	if err := migrateFn(ctx, storeInstance.DB()); err != nil {
		log.Fatalf("migration error: %v", err)
	}
	log.Println("migrations complete")
}

func usage() {
	fmt.Println("Usage: nerve-runtime (alias: neuralmaild) <serve|worker|mcp-stdio|migrate-core|migrate-cloud|migrate-all>")
}

func snippet(text string) string {
	if len(text) <= 200 {
		return text
	}
	return text[:200] + "..."
}

## Native report increment — 2026-09-09

After acceptance of PR #93, branch `codex/mcp2-runtime-compatibility` starts at merged `9a220a3`. Scope: `internal/release/runtime_compatibility.go` and tests, `cmd/neuralmaild/main.go`, native executable tests in `internal/release`, and this plan. Add `compatibility --json` before config loading: output the exact existing nine-field runtime manifest from linker metadata and compiled startup constants, plus SHA256 of the running executable. Reject missing/malformed build identity and flags. Report `database_verified=false` and `admission_verified=false`; do not claim signed membership, image provenance or a database probe. No new network endpoint or dependency.

This increment implements the native report prerequisite with the existing frozen Core29 window. Configurable bridge/target windows, versioned image publishing, source-pin adoption and combined DB rehearsal remain unchecked. No future migration number is allocated. Tests must build/run the real command with invalid config/DB environment, compare all manifest fields with the existing generator and hash the binary independently; malformed flags and dev metadata fail without application startup.

Native report validation: `go test -race ./internal/release ./internal/startup ./cmd/neuralmaild -count=1`, targeted `go vet`, and `scripts/ci/test_schema_quiescence_executables.py` PASS. The executable test builds release/dev binaries, compares all nine generator fields, hashes the running artifact independently, and verifies environment overrides cannot supply release identity. The current Core29 window remains frozen; this is not a bridge/target or DB admission claim.
