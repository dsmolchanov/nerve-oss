package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"neuralmail/internal/config"
	"neuralmail/internal/localauth"
	"neuralmail/internal/mcp"
	"neuralmail/internal/store"
)

type localBearerTransport struct{ token string }

func (b localBearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c := r.Clone(r.Context())
	c.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(c)
}

func TestSelfhostMailboxKeysIsolateToolsAndResources(t *testing.T) {
	a := selfhostApp(t)
	ctx := context.Background()
	first, err := a.Store.ListInboxes(ctx)
	if err != nil || len(first) != 1 {
		t.Fatalf("inboxes %v %v", first, err)
	}
	second, err := a.Store.EnsureInbox(ctx, "second@local.nerve.email")
	if err != nil {
		t.Fatal(err)
	}
	inboxes := []string{first[0], second}
	threads := []string{}
	messages := []string{}
	for _, id := range inboxes {
		thread, message, err := a.Store.InsertMessageWithThread(ctx, id, "access-"+id, store.Message{Direction: "inbound", Subject: "mailbox", Text: "quasar", CreatedAt: time.Now().UTC(), From: store.Participant{Email: "sender@example.test"}, To: []store.Participant{{Email: a.Config.SMTP.From}}})
		if err != nil {
			t.Fatal(err)
		}
		threads = append(threads, thread)
		messages = append(messages, message)
	}
	for owner := 0; owner < 2; owner++ {
		restricted := localauth.WithIdentity(ctx, localauth.Identity{InboxIDs: []string{inboxes[owner]}})
		foreign := 1 - owner
		svc := a.MCP.Tools
		calls := []func() (any, error){
			func() (any, error) { return svc.ListThreads(restricted, inboxes[foreign], "", 10) },
			func() (any, error) { return svc.SearchInbox(restricted, inboxes[foreign], "quasar", 10) },
			func() (any, error) { return svc.GetThread(restricted, threads[foreign]) },
			func() (any, error) { return svc.TriageMessage(restricted, messages[foreign]) },
			func() (any, error) { return svc.ExtractToSchema(restricted, messages[foreign], "support_v1") },
			func() (any, error) { return svc.DraftReply(restricted, threads[foreign], "reply") },
			func() (any, error) { return svc.SendReply(restricted, threads[foreign], "reply", "", false, "access") },
			func() (any, error) {
				return svc.ComposeEmail(restricted, inboxes[foreign], "test@example.test", "hi", "body", "", "access")
			},
		}
		for i, call := range calls {
			r, e := call()
			if !errors.Is(e, localauth.ErrForbidden) || r != nil {
				t.Fatalf("owner=%d tool=%d: %v %v", owner, i, r, e)
			}
		}
		if _, err := svc.GetThread(restricted, threads[owner]); err != nil {
			t.Fatal(err)
		}
	}
	cfg := a.Config
	cfg.Security.LocalAPIKeys = []config.LocalAPIKey{{Token: "first-test", InboxIDs: []string{inboxes[0]}}, {Token: "second-test", InboxIDs: []string{inboxes[1]}}}
	runtime := mcp.NewServer(cfg, a.MCP.Tools, nil, nil)
	hosted := httptest.NewServer(mcp.NewRouter(cfg, nil, mcp.NewLegacyHandler(runtime), mcp.NewSDKHandler(runtime, true)))
	defer hosted.Close()
	for owner, token := range []string{"first-test", "second-test"} {
		client := sdk.NewClient(&sdk.Implementation{Name: "isolation-test", Version: "1"}, nil)
		session, err := client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: hosted.URL, HTTPClient: &http.Client{Transport: localBearerTransport{token}}, DisableStandaloneSSE: true}, nil)
		if err != nil {
			t.Fatal(err)
		}
		result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "get_thread", Arguments: map[string]any{"thread_id": threads[1-owner]}})
		if err != nil {
			t.Fatal(err)
		}
		if !result.IsError {
			t.Fatalf("modern tools leaked: %#v", result)
		}
		result, err = session.CallTool(ctx, &sdk.CallToolParams{Name: "get_thread", Arguments: map[string]any{"thread_id": threads[owner]}})
		if err != nil || result.IsError {
			t.Fatalf("own thread refused: %v %v", result, err)
		}
		for _, uri := range []string{"email://threads/" + threads[1-owner], "email://messages/" + messages[1-owner]} {
			if got, err := session.ReadResource(ctx, &sdk.ReadResourceParams{URI: uri}); err == nil {
				t.Fatalf("resource leaked: %s %#v", uri, got)
			}
		}
		got, err := session.ReadResource(ctx, &sdk.ReadResourceParams{URI: "email://inboxes"})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(got)
		if !strings.Contains(string(raw), inboxes[owner]) || strings.Contains(string(raw), inboxes[1-owner]) {
			t.Fatalf("inbox list leaked: %s", raw)
		}
		session.Close()
	}
}
