package main

import (
	"context"
	"errors"
	"neuralmail/internal/llm"
	"neuralmail/internal/store"
	"reflect"
	"testing"
	"time"
)

func TestSelfhostNoopAILeavesSignalsUntouchedAndFTSWorks(t *testing.T) {
	a := selfhostApp(t)
	ctx := context.Background()
	ids, err := a.Store.ListInboxes(ctx)
	if err != nil || len(ids) != 1 {
		t.Fatalf("inboxes: %v %v", ids, err)
	}
	thread, message, err := a.Store.InsertMessageWithThread(ctx, ids[0], "ai-test", store.Message{
		Direction: "inbound", Subject: "test", Text: "Critical outage refund quasar", CreatedAt: time.Now().UTC(),
		From: store.Participant{Email: "sender@example.test"}, To: []store.Participant{{Email: a.Config.SMTP.From}},
	})
	if err != nil {
		t.Fatal(err)
	}
	before, _, err := a.Store.GetThread(ctx, thread)
	if err != nil {
		t.Fatal(err)
	}
	svc := a.MCP.Tools
	if svc.LLM.Name() != "noop" || svc.Vector != nil {
		t.Fatal("expected default noop/FTS runtime")
	}
	calls := []func() (any, error){
		func() (any, error) { return svc.TriageMessage(ctx, message) },
		func() (any, error) { return svc.ExtractToSchema(ctx, message, "invoice") },
		func() (any, error) { return svc.DraftReply(ctx, thread, "reply") },
	}
	for _, call := range calls {
		result, err := call()
		if !errors.Is(err, llm.ErrUnavailable) || result != nil {
			t.Fatalf("fake AI: %#v %v", result, err)
		}
	}
	after, _, err := a.Store.GetThread(ctx, thread)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("AI modified thread: %#v -> %#v", before, after)
	}
	result, err := svc.SearchInbox(ctx, ids[0], "quasar", 10)
	if err != nil {
		t.Fatal(err)
	}
	rows := result.(map[string]any)["results"].([]store.SearchResult)
	if len(rows) != 1 || rows[0].MessageID != message {
		t.Fatalf("FTS failed: %#v", rows)
	}
}
