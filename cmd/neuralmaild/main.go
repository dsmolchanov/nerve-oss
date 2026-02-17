package main

import (
	"context"
	"database/sql"
	"errors"
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
	"neuralmail/internal/queue"
	"neuralmail/internal/release"
	"neuralmail/internal/store"
	"neuralmail/internal/vector"
)

func main() {
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

	switch cmd {
	case "serve":
		runServe(ctx, cfg)
	case "worker":
		runWorker(ctx, cfg)
	case "mcp-stdio":
		runStdio(ctx, cfg)
	case "migrate-core":
		runMigrations(ctx, cfg, store.MigrateCore)
	case "migrate-cloud":
		runMigrations(ctx, cfg, store.MigrateCloud)
	case "migrate-all":
		runMigrations(ctx, cfg, store.MigrateAll)
	default:
		usage()
	}
}

func runServe(ctx context.Context, cfg config.Config) {
	appInstance, err := app.New(ctx, cfg)
	if err != nil {
		log.Fatalf("app init error: %v", err)
	}
	defer appInstance.Close()

	inboxAddr := cfg.SMTP.From
	if inboxAddr == "" {
		inboxAddr = "dev@local.nerve.email"
	}
	inboxID, _ := appInstance.Store.EnsureDefaults(ctx, inboxAddr)
	go func() {
		if err := appInstance.PollLoop(ctx, inboxID); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("poll loop stopped: %v", err)
		}
	}()

	log.Printf("nerve-runtime serving on %s", cfg.HTTP.Addr)
	if err := appInstance.Serve(ctx); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func runWorker(ctx context.Context, cfg config.Config) {
	storeInstance, err := store.Open(cfg.Database.DSN)
	if err != nil {
		log.Fatalf("store error: %v", err)
	}
	defer storeInstance.Close()
	if err := store.Migrate(ctx, storeInstance.DB()); err != nil {
		log.Fatalf("migration error: %v", err)
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
	outboxWorker := emailtransport.NewOutboxWorker(storeInstance, transportRegistry, "nerve-runtime-worker")
	go func() {
		if err := outboxWorker.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
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
