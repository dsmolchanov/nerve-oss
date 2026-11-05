package emailtransport

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"

	"neuralmail/internal/memguard"
	"neuralmail/internal/store"
)

func TestForwardedReceivedEmailIDTrustsOnlyThisInboxKey(t *testing.T) {
	cases := map[string]struct {
		key, inboxID, want string
		ok                 bool
	}{
		"forward key":           {key: "fwd:email-1:inbox-1:dest@example.com", inboxID: "inbox-1", want: "email-1", ok: true},
		"destination has colon": {key: "fwd:email-1:inbox-1:Name: <dest@example.com>", inboxID: "inbox-1", want: "email-1", ok: true},
		"other inbox":           {key: "fwd:email-1:inbox-2:dest@example.com", inboxID: "inbox-1"},
		"not a forward":         {key: "tool:email-1:inbox-1:dest@example.com", inboxID: "inbox-1"},
		"missing email id":      {key: "fwd::inbox-1:dest@example.com", inboxID: "inbox-1"},
		"truncated":             {key: "fwd:email-1:inbox-1", inboxID: "inbox-1"},
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			got, ok := forwardedReceivedEmailID(input.key, input.inboxID)
			if got != input.want || ok != input.ok {
				t.Fatalf("forwardedReceivedEmailID(%q) = %q, %v; want %q, %v", input.key, got, ok, input.want, input.ok)
			}
		})
	}
}

func TestForwardReplyToRefusesOwnAddresses(t *testing.T) {
	cases := map[string]struct{ sender, want string }{
		"original sender":       {sender: " Sender@Example.NET ", want: "sender@example.net"},
		"our inbox":             {sender: "Inbox@example.com", want: ""},
		"forward destination":   {sender: "dest@example.org", want: ""},
		"not an email address":  {sender: "not-an-address", want: ""},
		"sender never recorded": {sender: "", want: ""},
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if got := forwardReplyTo(input.sender, "inbox@example.com", "dest@example.org"); got != input.want {
				t.Fatalf("forwardReplyTo(%q) = %q, want %q", input.sender, got, input.want)
			}
		})
	}
}

// resendCaptureAdapter registers the capture adapter under the provider name
// that carries Reply-To.
type resendCaptureAdapter struct{ *captureOutboundAdapter }

func (resendCaptureAdapter) Name() string { return "resend" }

func TestOutboxWorkerSetsForwardReplyToOriginalSender(t *testing.T) {
	withOutboxWorkerDatabase(t, func(ctx context.Context, db *sql.DB, st *store.Store) {
		orgID := uuid.NewString()
		inboxID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'worker-forward')`, orgID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO inboxes (id, org_id, address, status)
			VALUES ($1, $2, 'forward@local.neuralmail', 'active')
		`, inboxID, orgID); err != nil {
			t.Fatal(err)
		}
		receivedEmailID := "received-" + uuid.NewString()
		if _, _, err := st.InsertMessageWithThread(ctx, inboxID, "", store.Message{
			Direction:         "inbound",
			Subject:           "original",
			Text:              "body",
			ProviderMessageID: "provider-" + uuid.NewString(),
			From:              store.Participant{Email: "Original@Sender.example"},
			To:                []store.Participant{{Email: "forward@local.neuralmail"}},
			CreatedAt:         time.Now().UTC(),
			ReceivedEmailID:   receivedEmailID,
		}); err != nil {
			t.Fatal(err)
		}

		// The forward key does not name the provider, so each case needs its
		// own destination.
		forward := func(emailID, provider, destination string) store.OutboxMessage {
			return store.OutboxMessage{
				OrgID: orgID, InboxID: inboxID, Provider: provider,
				IdempotencyKey: "fwd:" + emailID + ":" + inboxID + ":" + destination,
				To:             destination, From: "forward@local.neuralmail",
				Subject: "Fwd: original", TextBody: "body " + emailID + provider,
			}
		}
		recorded := forward(receivedEmailID, "resend", "dest@example.org")
		unrecorded := forward("never-recorded", "resend", "dest@example.org")
		otherProvider := forward(receivedEmailID, "capture", "other-dest@example.org")
		for _, msg := range []store.OutboxMessage{recorded, unrecorded, otherProvider} {
			if _, err := st.EnqueueOutboxMessage(ctx, msg); err != nil {
				t.Fatal(err)
			}
		}

		resend := resendCaptureAdapter{&captureOutboundAdapter{}}
		other := &captureOutboundAdapter{}
		registry := NewRegistry()
		for _, adapter := range []OutboundAdapter{resend, other} {
			if err := registry.RegisterOutbound(adapter); err != nil {
				t.Fatal(err)
			}
		}
		budget, err := memguard.New(1 << 20)
		if err != nil {
			t.Fatal(err)
		}
		worker := NewOutboxWorker(st, registry, "forward-reply-to-test", budget)
		worker.ClaimLimit = 10
		worker.claimAndDeliver(ctx)

		replyTo := map[string][]string{}
		for _, delivery := range resend.deliveries {
			replyTo[delivery.idempotencyKey] = delivery.message.ReplyTo
		}
		if got := replyTo[recorded.IdempotencyKey]; len(got) != 1 || got[0] != "original@sender.example" {
			t.Fatalf("recorded forward Reply-To = %v, want the canonical original sender", got)
		}
		if got, sent := replyTo[unrecorded.IdempotencyKey]; !sent || len(got) != 0 {
			t.Fatalf("unrecorded forward sent=%v Reply-To=%v, want sent without Reply-To", sent, got)
		}
		if len(other.deliveries) != 1 || len(other.deliveries[0].message.ReplyTo) != 0 {
			t.Fatalf("non-Resend deliveries=%d, want one without Reply-To", len(other.deliveries))
		}
	})
}
