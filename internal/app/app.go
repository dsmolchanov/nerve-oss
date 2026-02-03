package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"neuralmail/internal/config"
	"neuralmail/internal/embed"
	"neuralmail/internal/jmap"
	"neuralmail/internal/llm"
	"neuralmail/internal/mcp"
	"neuralmail/internal/policy"
	"neuralmail/internal/queue"
	"neuralmail/internal/store"
	"neuralmail/internal/tools"
	"neuralmail/internal/vector"
)

type App struct {
	Config   config.Config
	Store    *store.Store
	Queue    *queue.Queue
	Vector   vector.Store
	Embedder embed.Provider
	LLM      llm.Provider
	Policy   policy.Policy
	MCP      *mcp.Server
}

func New(ctx context.Context, cfg config.Config) (*App, error) {
	st, err := store.Open(cfg.Database.DSN)
	if err != nil {
		return nil, err
	}
	if err := store.Migrate(ctx, st.DB()); err != nil {
		return nil, err
	}
	inboxAddr := cfg.SMTP.From
	if inboxAddr == "" {
		inboxAddr = "dev@local.neuralmail"
	}
	_, _ = st.EnsureDefaults(ctx, inboxAddr)

	q, err := queue.New(cfg.Redis.URL)
	if err != nil {
		return nil, err
	}

	pol, err := policy.Load(cfg.Policy.DefaultPath)
	if err != nil {
		return nil, err
	}

	llmProvider := selectLLM(cfg)
	embedder := selectEmbedder(cfg)

	var vectorStore vector.Store
	if cfg.Embedding.Provider != "noop" {
		vectorStore = vector.NewQdrant(cfg.Qdrant.URL, cfg.Qdrant.Collection)
	}

	toolSvc := tools.NewService(cfg, st, llmProvider, vectorStore, pol, embedder)
	mcpServer := mcp.NewServer(cfg, toolSvc)

	return &App{
		Config:   cfg,
		Store:    st,
		Queue:    q,
		Vector:   vectorStore,
		Embedder: embedder,
		LLM:      llmProvider,
		Policy:   pol,
		MCP:      mcpServer,
	}, nil
}

func (a *App) Close() error {
	var err error
	if a.Store != nil {
		err = a.Store.Close()
	}
	if a.Queue != nil {
		_ = a.Queue.Close()
	}
	return err
}

func (a *App) Serve(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := a.Store.Ping(ctx); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		if err := a.Queue.Ping(ctx); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})
	mux.HandleFunc("/debug", a.handleDebug)
	mux.HandleFunc("/mcp", a.MCP.HandleHTTP)
	mux.HandleFunc("/mcp/sse", a.MCP.HandleSSEStub)
	mux.HandleFunc("/jmap/push", a.handleJMAPPush)

	srv := &http.Server{
		Addr:              a.Config.HTTP.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	return srv.ListenAndServe()
}

func (a *App) handleJMAPPush(w http.ResponseWriter, r *http.Request) {
	secret := r.Header.Get("X-NM-Push-Secret")
	if a.Config.JMAP.PushSecret != "" && secret != a.Config.JMAP.PushSecret {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (a *App) handleDebug(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	queueDepth, _ := a.Queue.Depth(ctx)
	inboxes, _ := a.Store.ListInboxes(ctx)
	lastStates := make(map[string]string)
	for _, id := range inboxes {
		state, _ := a.Store.LastInboxState(ctx, id)
		lastStates[id] = state
	}
	audit, _ := a.Store.ListAudit(ctx, 20)

	w.Header().Set("Content-Type", "text/html")
	_, _ = fmt.Fprintf(w, "<html><body><h1>Nerve Debug</h1>")
	_, _ = fmt.Fprintf(w, "<p>Queue depth: %d</p>", queueDepth)
	_, _ = fmt.Fprintf(w, "<h2>Inbox checkpoints</h2><ul>")
	for id, state := range lastStates {
		_, _ = fmt.Fprintf(w, "<li>%s: %s</li>", id, state)
	}
	_, _ = fmt.Fprintf(w, "</ul>")
	_, _ = fmt.Fprintf(w, "<h2>Quick actions</h2>")
	_, _ = fmt.Fprintf(w, "<ul><li><a href=\"/healthz\">Check health</a></li></ul>")
	_, _ = fmt.Fprintf(w, "<h2>Recent tool calls</h2><ul>")
	for _, item := range audit {
		_, _ = fmt.Fprintf(w, "<li>%v</li>", item)
	}
	_, _ = fmt.Fprintf(w, "</ul></body></html>")
}

func (a *App) PollLoop(ctx context.Context, client jmap.Client, inboxID string) error {
	if client == nil {
		return errors.New("missing jmap client")
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(a.Config.JMAP.PollInterval):
			state, _ := a.Store.GetCheckpoint(ctx, inboxID, client.Name())
			newState, messageIDs, err := jmap.Ingest(ctx, client, a.Store, inboxID, state)
			if err == nil && newState != "" {
				_ = a.Store.UpdateCheckpoint(ctx, inboxID, client.Name(), newState)
			}
			for _, id := range messageIDs {
				_ = a.Queue.PushEmbeddingJob(ctx, id)
			}
		}
	}
}

func selectLLM(cfg config.Config) llm.Provider {
	switch cfg.LLM.Provider {
	case "openai":
		if cfg.LLM.OpenAIKey != "" {
			return llm.NewOpenAI(cfg.LLM.OpenAIKey, cfg.LLM.Model)
		}
	case "ollama":
		if cfg.LLM.OllamaURL != "" {
			return llm.NewOllama(cfg.LLM.OllamaURL, cfg.LLM.Model)
		}
	}
	return llm.NewNoop()
}

func selectEmbedder(cfg config.Config) embed.Provider {
	switch cfg.Embedding.Provider {
	case "openai":
		if cfg.LLM.OpenAIKey != "" {
			return embed.NewOpenAI(cfg.LLM.OpenAIKey, cfg.Embedding.Model, cfg.Embedding.Dim)
		}
	case "ollama":
		if cfg.LLM.OllamaURL != "" {
			return embed.NewOllama(cfg.LLM.OllamaURL, cfg.Embedding.Model, cfg.Embedding.Dim)
		}
	case "noop":
		return embed.NewNoop(cfg.Embedding.Dim)
	}
	return nil
}
