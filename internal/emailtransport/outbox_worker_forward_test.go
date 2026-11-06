package emailtransport

import (
	"context"
	"database/sql"
	"strings"
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

// idempotentResendAdapter enforces Resend's idempotency contract: a reused
// key with the same payload returns the original id, a different payload is
// refused as a mismatch.
type idempotentResendAdapter struct {
	payloads map[string]string
	ids      map[string]string
	sends    int
}

func (a *idempotentResendAdapter) Name() string                          { return "resend" }
func (a *idempotentResendAdapter) SupportsIdempotentReplay() bool        { return true }
func (a *idempotentResendAdapter) IdempotentReplayWindow() time.Duration { return 24 * time.Hour }
func (a *idempotentResendAdapter) GetDeliveryStatus(context.Context, string) (DeliveryStatus, error) {
	return DeliveryStatusUnknown, ErrNotSupported
}

func (a *idempotentResendAdapter) record(key string, msg OutboundMessage, id string) {
	a.payloads[key] = resendPayloadIdentity(msg)
	a.ids[key] = id
}

func (a *idempotentResendAdapter) SendMessage(_ context.Context, msg OutboundMessage, key string) (string, error) {
	if stored, ok := a.payloads[key]; ok {
		if stored != resendPayloadIdentity(msg) {
			return "", &ProviderError{Permanent: true, StatusCode: 409, Reason: ReasonIdempotencyPayloadMismatch}
		}
		return a.ids[key], nil
	}
	a.sends++
	a.record(key, msg, "new-"+key)
	return a.ids[key], nil
}

func resendPayloadIdentity(msg OutboundMessage) string {
	return strings.Join([]string{msg.From, strings.Join(msg.To, ","), msg.Subject, msg.TextBody, msg.HTMLBody,
		strings.Join(msg.ReplyTo, ","), msg.Headers["X-Nerve-Loop"]}, "\x00")
}

func TestOutboxWorkerReplaysPreReplyToForwardPayload(t *testing.T) {
	withOutboxWorkerDatabase(t, func(ctx context.Context, db *sql.DB, st *store.Store) {
		orgID, inboxID := seedWorkerPolicyFence(t, ctx, db, st, "forward-replay")
		inbox := "forward-replay@local.neuralmail"
		receivedEmailID := "received-" + uuid.NewString()
		if _, _, err := st.InsertMessageWithThread(ctx, inboxID, "", store.Message{
			Direction: "inbound", Subject: "original", Text: "body",
			ProviderMessageID: "provider-" + uuid.NewString(),
			From:              store.Participant{Email: "original@sender.example"},
			To:                []store.Participant{{Email: inbox}},
			CreatedAt:         time.Now().UTC(), ReceivedEmailID: receivedEmailID,
		}); err != nil {
			t.Fatal(err)
		}
		insertForward := func(destination string, epoch int64) (string, string) {
			id := uuid.NewString()
			key := "fwd:" + receivedEmailID + ":" + inboxID + ":" + destination
			var epochArg any
			if epoch > 0 {
				epochArg = epoch
			}
			if _, err := db.ExecContext(ctx, `
				INSERT INTO outbox_messages (id, org_id, inbox_id, provider, idempotency_key, "to", "from", subject, text_body, autonomous_policy_epoch)
				VALUES ($1, $2, $3, 'resend', $4, $5, $6, 'Fwd: original', 'body', $7)
			`, id, orgID, inboxID, key, destination, inbox, epochArg); err != nil {
				t.Fatal(err)
			}
			return id, key
		}
		preUpgrade := func(destination string) OutboundMessage {
			return OutboundMessage{From: inbox, To: []string{destination}, Subject: "Fwd: original", TextBody: "body",
				Headers: map[string]string{"X-Nerve-Loop": inboxID}}
		}
		assertSent := func(id, wantProviderID string) {
			t.Helper()
			var status, providerID string
			if err := db.QueryRowContext(ctx, `SELECT status, coalesce(provider_message_id,'') FROM outbox_messages WHERE id = $1`, id).Scan(&status, &providerID); err != nil {
				t.Fatal(err)
			}
			if status != "sent" || providerID != wantProviderID {
				t.Fatalf("outbox %s status=%q provider id=%q, want sent with %q", id, status, providerID, wantProviderID)
			}
		}

		t.Run("legacy retry", func(t *testing.T) {
			adapter := &idempotentResendAdapter{payloads: map[string]string{}, ids: map[string]string{}}
			id, key := insertForward("legacy-dest@example.org", 0)
			adapter.record(key, preUpgrade("legacy-dest@example.org"), "original-legacy")
			msg := claimWorkerPolicyOutbox(t, ctx, st, id, "legacy-worker")
			if err := policyFenceWorker(t, st, adapter, "legacy-worker").deliverOne(ctx, msg); err != nil {
				t.Fatal(err)
			}
			if adapter.sends != 0 {
				t.Fatalf("sends=%d, want the pre-upgrade send recovered without a second delivery", adapter.sends)
			}
			assertSent(id, "original-legacy")
		})

		t.Run("unresolved operation", func(t *testing.T) {
			adapter := &idempotentResendAdapter{payloads: map[string]string{}, ids: map[string]string{}}
			id, _ := insertForward("fenced-dest@example.org", 1)
			first := claimWorkerPolicyOutbox(t, ctx, st, id, "crashed-worker")
			operationID, err := st.BeginOutboxProviderOperation(ctx, first)
			if err != nil {
				t.Fatal(err)
			}
			adapter.record(operationID, preUpgrade("fenced-dest@example.org"), "original-fenced")
			if _, err := db.ExecContext(ctx, `UPDATE outbox_messages SET locked_at = now() - interval '10 minutes' WHERE id = $1`, id); err != nil {
				t.Fatal(err)
			}
			msg := claimWorkerPolicyOutbox(t, ctx, st, id, "recovery-worker")
			if err := policyFenceWorker(t, st, adapter, "recovery-worker").deliverOne(ctx, msg); err != nil {
				t.Fatal(err)
			}
			if adapter.sends != 0 {
				t.Fatalf("sends=%d, want the unresolved send recovered without a second delivery", adapter.sends)
			}
			assertSent(id, "original-fenced")
		})

		t.Run("new forward", func(t *testing.T) {
			adapter := &idempotentResendAdapter{payloads: map[string]string{}, ids: map[string]string{}}
			id, _ := insertForward("new-dest@example.org", 0)
			msg := claimWorkerPolicyOutbox(t, ctx, st, id, "new-worker")
			if err := policyFenceWorker(t, st, adapter, "new-worker").deliverOne(ctx, msg); err != nil {
				t.Fatal(err)
			}
			if adapter.sends != 1 {
				t.Fatalf("sends=%d, want one", adapter.sends)
			}
			for sentKey, payload := range adapter.payloads {
				if !strings.Contains(payload, "original@sender.example") {
					t.Fatalf("payload=%q, want Reply-To to the original sender", payload)
				}
				assertSent(id, "new-"+sentKey)
			}
		})
	})
}
