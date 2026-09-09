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
	"neuralmail/internal/embed"
	"neuralmail/internal/localauth"
	"neuralmail/internal/mcp"
	"neuralmail/internal/store"
	"neuralmail/internal/vector"
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
	// The schema permits independent inbox/thread foreign keys. Include rows
	// whose message belongs to the other mailbox but points at the allowed thread.
	malformed := []string{}
	for owner := 0; owner < 2; owner++ {
		id, err := a.Store.InsertMessage(ctx, store.Message{InboxID: inboxes[1-owner], ThreadID: threads[owner], Direction: "inbound", Subject: "foreignmisbound", Text: "foreignmisbound", CreatedAt: time.Now().UTC().Add(time.Minute), From: store.Participant{Email: "foreign@example.test"}})
		if err != nil {
			t.Fatal(err)
		}
		malformed = append(malformed, id)
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
		own, err := svc.GetThread(restricted, threads[owner])
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(own)
		if strings.Contains(string(raw), "foreignmisbound") {
			t.Fatalf("thread leaked mismatched message: %s", raw)
		}
		for _, id := range malformed {
			if err := svc.CheckLocalAccess(restricted, "message", id); !errors.Is(err, localauth.ErrForbidden) {
				t.Fatalf("mismatched message allowed: %v", err)
			}
		}
		found, err := svc.SearchInbox(restricted, inboxes[owner], "foreignmisbound", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(found.(map[string]any)["results"].([]store.SearchResult)) != 0 {
			t.Fatalf("FTS leaked: %#v", found)
		}
		reply, err := svc.SendReply(restricted, threads[owner], "reply", "", false, "mismatch-reply-"+inboxes[owner])
		if err != nil {
			t.Fatal(err)
		}
		var recipient string
		if err := a.Store.DB().QueryRowContext(ctx, `SELECT "to" FROM outbox_messages WHERE id=$1`, reply.(map[string]any)["message_id"]).Scan(&recipient); err != nil {
			t.Fatal(err)
		}
		if recipient != "sender@example.test" {
			t.Fatalf("reply used foreign message recipient: %s", recipient)
		}
		svc.Vector = localSearchProbe{hits: []vector.SearchHit{
			{Score: 1, Payload: map[string]any{"message_id": malformed[owner], "snippet": "foreignmisbound"}},
			{Score: .9, Payload: map[string]any{"message_id": messages[1-owner], "snippet": "foreignmisbound"}},
			{Score: .8, Payload: map[string]any{"message_id": messages[owner], "thread_id": threads[1-owner], "snippet": "foreignmisbound"}},
		}}
		svc.Embedder = embed.NewNoop(2)
		found, err = svc.SearchInbox(restricted, inboxes[owner], "quasar", 10)
		if err != nil {
			t.Fatal(err)
		}
		hits := found.(map[string]any)["results"].([]map[string]any)
		if len(hits) != 1 || hits[0]["message_id"] != messages[owner] || hits[0]["thread_id"] != threads[owner] || hits[0]["snippet"] != "quasar" {
			t.Fatalf("vector index leaked content: %#v", hits)
		}
		svc.Vector = nil

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
		for _, uri := range []string{"email://threads/" + threads[1-owner], "email://messages/" + messages[1-owner], "email://messages/" + malformed[0], "email://messages/" + malformed[1]} {
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

type localSearchProbe struct{ hits []vector.SearchHit }

func (p localSearchProbe) Name() string                                 { return "test" }
func (p localSearchProbe) EnsureCollection(context.Context, int) error  { return nil }
func (p localSearchProbe) Upsert(context.Context, []vector.Point) error { return nil }
func (p localSearchProbe) Search(context.Context, []float32, int, map[string]any) ([]vector.SearchHit, error) {
	return p.hits, nil
}
