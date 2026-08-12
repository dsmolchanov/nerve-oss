package tools

import (
	"context"
	"database/sql"
	"testing"

	"neuralmail/internal/auth"
	"neuralmail/internal/store"
)

type replyMessageStoreFake map[string]store.Message

func (fake replyMessageStoreFake) GetMessage(_ context.Context, messageID string) (store.Message, error) {
	message, ok := fake[messageID]
	if !ok {
		return store.Message{}, sql.ErrNoRows
	}
	return message, nil
}

func TestReplyRecipientUsesLatestRealInboundForM2MOrg(t *testing.T) {
	thread := store.Thread{ID: "thread-1", InboxID: "inbox-1"}
	messages := []store.Message{
		{ID: "inbound-1", ThreadID: thread.ID, InboxID: thread.InboxID, Direction: "inbound"},
		{ID: "inbound-not-real", ThreadID: thread.ID, InboxID: thread.InboxID, Direction: "inbound"},
		{ID: "outbound-1", ThreadID: thread.ID, InboxID: thread.InboxID, Direction: "outbound", From: store.Participant{Email: "agent@nerve.email"}},
	}
	messagesStore := replyMessageStoreFake{
		"inbound-1":        {ID: "inbound-1", ThreadID: thread.ID, InboxID: thread.InboxID, Direction: "inbound", ReceivedEmailID: "received-1", From: store.Participant{Email: " sender@example.net "}},
		"inbound-not-real": {ID: "inbound-not-real", ThreadID: thread.ID, InboxID: thread.InboxID, Direction: "inbound", From: store.Participant{Email: "spoofed@example.net"}},
	}

	recipient, err := replyRecipient(context.Background(), messagesStore, auth.Principal{Kind: auth.PrincipalM2MOrg}, thread, messages)
	if err != nil {
		t.Fatalf("reply recipient: %v", err)
	}
	if recipient != "sender@example.net" {
		t.Fatalf("recipient=%q want latest real inbound sender", recipient)
	}
}

func TestReplyRecipientPreservesLegacyLatestMessageBehavior(t *testing.T) {
	thread := store.Thread{ID: "thread-1", InboxID: "inbox-1"}
	messages := []store.Message{
		{ID: "inbound-1", From: store.Participant{Email: "sender@example.net"}},
		{ID: "outbound-1", From: store.Participant{Email: "legacy-target@example.net"}},
	}

	recipient, err := replyRecipient(context.Background(), nil, auth.Principal{Kind: auth.PrincipalCloudAPIKey}, thread, messages)
	if err != nil {
		t.Fatalf("legacy reply recipient: %v", err)
	}
	if recipient != "legacy-target@example.net" {
		t.Fatalf("legacy recipient=%q", recipient)
	}
}
