package hybrid

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"neuralmail/internal/emailtransport"
	"neuralmail/internal/store"
)

// hybridRuntimeStore opens the runtime database these adapters write into.
// The deduplication guarantee belongs to the store's own
// (inbox_id, provider_message_id) key, so proving it against a fake would
// prove nothing.
func hybridRuntimeStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dsn := os.Getenv("NM_TEST_DB_DSN")
	if dsn == "" {
		if os.Getenv("NM_REQUIRE_DB") == "1" {
			t.Fatal("NM_REQUIRE_DB=1 but NM_TEST_DB_DSN is unset")
		}
		t.Skip("set NM_TEST_DB_DSN to exercise hybrid ingest against the runtime store")
	}
	st, err := store.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := store.MigrateCore(ctx, st.DB()); err != nil {
		t.Fatal(err)
	}
	// A fresh mailbox per test: deduplication is scoped to the inbox, so this
	// isolates runs without needing a database per test.
	address := fmt.Sprintf("hybrid-%s@local.nerve.email", strings.ReplaceAll(uuid.NewString(), "-", ""))
	inboxID, err := st.EnsureDefaults(ctx, address)
	if err != nil {
		t.Fatal(err)
	}
	return st, inboxID
}

func inboundFixture(t *testing.T, deliveries []Delivery) (*InboundAdapter, *cloudServer) {
	t.Helper()
	client, _ := newTestClient(t, "")
	server, state := newCloudServer(t, client.InstallationID, client.InboxID)
	client.BaseURL = server.URL
	queue := append([]Delivery(nil), deliveries...)
	acked := map[string]bool{}
	state.handler = func(action string, input map[string]any) (int, any) {
		switch action {
		case "poll":
			if len(queue) == 0 {
				return http.StatusOK, map[string]any{"delivery": nil}
			}
			next := queue[0]
			queue = queue[1:]
			return http.StatusOK, map[string]any{"delivery": next}
		case "ack":
			acked[fmt.Sprint(input["delivery_id"])] = true
			return http.StatusOK, map[string]any{"acked": true}
		}
		return http.StatusOK, map[string]any{}
	}
	return &InboundAdapter{Client: client, AuthorityID: "cloud.example.test"}, state
}

func delivery(id, providerID string) Delivery {
	return Delivery{
		ID: id, OrgID: "33333333-3333-4333-8333-333333333333",
		InstallationID:    "11111111-1111-4111-8111-111111111111",
		InboxID:           "22222222-2222-4222-8222-222222222222",
		ProviderMessageID: providerID, Subject: "Synthetic", Body: "synthetic body",
		Sender: "sender@example.test", Recipient: "agent@example.test", LeaseToken: uuid.NewString(),
	}
}

// A runtime that crashed between storing a message and acknowledging it sees
// the same delivery again on restart. The local key is derived only from
// identifiers that outlive the process, so the redelivery deduplicates instead
// of producing a second copy of the mail.
func TestHybridIngestDeduplicatesRedeliveryAcrossRestart(t *testing.T) {
	st, inboxID := hybridRuntimeStore(t)
	ctx := context.Background()
	first := delivery(uuid.NewString(), "provider-1")

	adapter, _ := inboundFixture(t, []Delivery{first})
	_, stored, err := adapter.Ingest(ctx, st, inboxID, "")
	if err != nil || len(stored) != 1 {
		t.Fatalf("first ingest: stored=%v err=%v", stored, err)
	}

	// Restart: a new adapter and client, the same Cloud identity, and a lease
	// token that differs because the old lease expired.
	replay := first
	replay.LeaseToken = uuid.NewString()
	restarted, _ := inboundFixture(t, []Delivery{replay})
	restarted.AuthorityID = adapter.AuthorityID
	_, again, err := restarted.Ingest(ctx, st, inboxID, "")
	if err != nil || len(again) != 1 {
		t.Fatalf("redelivery ingest: stored=%v err=%v", again, err)
	}
	if again[0] != stored[0] {
		t.Fatalf("redelivery created message %s, want the original %s", again[0], stored[0])
	}
	var count int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM messages WHERE inbox_id = $1`, inboxID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("inbox holds %d messages after one delivery and one redelivery", count)
	}
}

// Two Cloud deployments can hand out the same delivery UUID. Without the
// authority namespace the second one's mail would be silently dropped as a
// replay of the first.
func TestHybridIngestKeepsAuthoritiesApart(t *testing.T) {
	st, inboxID := hybridRuntimeStore(t)
	ctx := context.Background()
	shared := delivery(uuid.NewString(), "provider-1")

	first, _ := inboundFixture(t, []Delivery{shared})
	first.AuthorityID = "cloud-a.example.test"
	if _, stored, err := first.Ingest(ctx, st, inboxID, ""); err != nil || len(stored) != 1 {
		t.Fatalf("first authority: %v %v", stored, err)
	}
	second, _ := inboundFixture(t, []Delivery{shared})
	second.AuthorityID = "cloud-b.example.test"
	if _, stored, err := second.Ingest(ctx, st, inboxID, ""); err != nil || len(stored) != 1 {
		t.Fatalf("second authority: %v %v", stored, err)
	}
	var count int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM messages WHERE inbox_id = $1`, inboxID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("two authorities produced %d messages, want 2", count)
	}
}

// Acknowledging before the local write commits would let a crash in between
// lose the mail outright, because Cloud is then free to purge its copy.
func TestHybridIngestNeverAcknowledgesBeforeTheLocalWrite(t *testing.T) {
	st, inboxID := hybridRuntimeStore(t)
	ctx := context.Background()
	client, _ := newTestClient(t, "")
	server, state := newCloudServer(t, client.InstallationID, client.InboxID)
	client.BaseURL = server.URL

	target := delivery(uuid.NewString(), "provider-1")
	var order []string
	served := false
	state.handler = func(action string, _ map[string]any) (int, any) {
		switch action {
		case "poll":
			if served {
				return http.StatusOK, map[string]any{"delivery": nil}
			}
			served = true
			order = append(order, "poll")
			return http.StatusOK, map[string]any{"delivery": target}
		case "ack":
			var count int
			if err := st.DB().QueryRowContext(ctx,
				`SELECT count(*) FROM messages WHERE inbox_id = $1 AND provider_message_id = $2`,
				inboxID, LocalDeliveryID("cloud.example.test", target)).Scan(&count); err != nil {
				t.Error(err)
			}
			if count != 1 {
				t.Errorf("cloud was acknowledged while the local store held %d copies", count)
			}
			order = append(order, "ack")
			return http.StatusOK, map[string]any{"acked": true}
		}
		return http.StatusOK, map[string]any{}
	}
	adapter := &InboundAdapter{Client: client, AuthorityID: "cloud.example.test"}
	if _, _, err := adapter.Ingest(ctx, st, inboxID, ""); err != nil {
		t.Fatal(err)
	}
	if len(order) < 2 || order[0] != "poll" || order[1] != "ack" {
		t.Fatalf("call order %v, want poll before ack", order)
	}
}

// A delivery left unacknowledged goes back to the Cloud queue when its lease
// expires. Swallowing the store failure and acknowledging anyway would lose it.
func TestHybridIngestLeavesAFailedWriteUnacknowledged(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestClient(t, "")
	server, state := newCloudServer(t, client.InstallationID, client.InboxID)
	client.BaseURL = server.URL
	acked := false
	state.handler = func(action string, _ map[string]any) (int, any) {
		if action == "ack" {
			acked = true
		}
		if action == "poll" {
			return http.StatusOK, map[string]any{"delivery": delivery(uuid.NewString(), "provider-1")}
		}
		return http.StatusOK, map[string]any{}
	}
	// A store whose connection is closed stands in for any write failure.
	broken, err := store.Open("postgres://127.0.0.1:1/closed?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	_ = broken.Close()
	adapter := &InboundAdapter{Client: client, AuthorityID: "cloud.example.test"}
	if _, stored, err := adapter.Ingest(ctx, broken, uuid.NewString(), ""); err == nil || len(stored) != 0 {
		t.Fatalf("failed write reported success: stored=%v err=%v", stored, err)
	}
	if acked {
		t.Fatal("cloud was acknowledged for a message that was never stored")
	}
}

// Cloud settles a send to exactly one of these. Reporting the wrong one either
// duplicates mail on the recipient's side or drops it.
func TestHybridOutboundResolvesEverySendStatus(t *testing.T) {
	cases := map[string]struct {
		receipt SendReceipt
		id      string
		err     error
	}{
		"sent":      {SendReceipt{Status: "sent", ProviderMessageID: "p1"}, "p1", nil},
		"uncertain": {SendReceipt{Status: "uncertain"}, "", ErrSendUncertain},
		"accepted":  {SendReceipt{Status: "accepted"}, "", ErrSendPending},
		"claimed":   {SendReceipt{Status: "claimed"}, "", ErrSendPending},
		"unknown":   {SendReceipt{Status: "invented"}, "", ErrUnavailable},
		// "sent" with no provider id is Cloud contradicting itself; treating
		// it as success would record a delivery nothing can be traced to.
		"sent without id": {SendReceipt{Status: "sent"}, "", ErrUnavailable},
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			client, _ := newTestClient(t, "")
			server, state := newCloudServer(t, client.InstallationID, client.InboxID)
			client.BaseURL = server.URL
			state.handler = func(string, map[string]any) (int, any) { return http.StatusOK, want.receipt }
			adapter := &OutboundAdapter{Client: client, Kind: "reply"}
			id, err := adapter.SendMessage(context.Background(),
				emailtransport.OutboundMessage{To: []string{"a@example.test"}, Subject: "s", TextBody: "b"}, "op-1")
			if id != want.id || !errors.Is(err, want.err) {
				t.Fatalf("id=%q err=%v, want id=%q err=%v", id, err, want.id, want.err)
			}
		})
	}
}

// The worker's idempotency key must reach Cloud unchanged: it is what makes a
// retry return the original outcome instead of sending a second copy.
func TestHybridOutboundPassesTheIdempotencyKeyThrough(t *testing.T) {
	client, _ := newTestClient(t, "")
	server, state := newCloudServer(t, client.InstallationID, client.InboxID)
	client.BaseURL = server.URL
	var seen []string
	state.handler = func(action string, input map[string]any) (int, any) {
		if action == "send" {
			seen = append(seen, fmt.Sprint(input["operation_key"]))
		}
		return http.StatusOK, SendReceipt{Status: "sent", ProviderMessageID: "p1"}
	}
	adapter := &OutboundAdapter{Client: client, Kind: "reply"}
	message := emailtransport.OutboundMessage{To: []string{"a@example.test"}, Subject: "s", TextBody: "b"}
	for range 3 {
		if _, err := adapter.SendMessage(context.Background(), message, "outbox-42"); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range seen {
		if key != "outbox-42" {
			t.Fatalf("operation key reached cloud as %q", key)
		}
	}
	if !adapter.SupportsIdempotentReplay() || adapter.IdempotentReplayWindow() <= 0 {
		t.Fatal("adapter must declare its replay window for the worker to retry safely")
	}
}

// Cloud accepts exactly one recipient per operation so a partial delivery
// cannot hide inside one receipt. Refuse locally rather than send a message
// that silently reaches fewer people than asked.
func TestHybridOutboundRefusesUnsendableRequests(t *testing.T) {
	client, _ := newTestClient(t, "")
	server, state := newCloudServer(t, client.InstallationID, client.InboxID)
	client.BaseURL = server.URL
	state.handler = func(string, map[string]any) (int, any) {
		t.Error("an unsendable request reached cloud")
		return http.StatusOK, SendReceipt{Status: "sent", ProviderMessageID: "p1"}
	}
	adapter := &OutboundAdapter{Client: client, Kind: "reply"}
	cases := map[string]struct {
		message emailtransport.OutboundMessage
		key     string
	}{
		"no recipient":   {emailtransport.OutboundMessage{}, "op-1"},
		"two recipients": {emailtransport.OutboundMessage{To: []string{"a@example.test", "b@example.test"}}, "op-1"},
		"empty key":      {emailtransport.OutboundMessage{To: []string{"a@example.test"}}, ""},
		"untrimmed key":  {emailtransport.OutboundMessage{To: []string{"a@example.test"}}, " op-1"},
		"oversized key":  {emailtransport.OutboundMessage{To: []string{"a@example.test"}}, strings.Repeat("k", 129)},
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := adapter.SendMessage(context.Background(), input.message, input.key); err == nil {
				t.Fatal("accepted an unsendable request")
			}
		})
	}
}

// Delivery status is read by operation key, the identifier this runtime chose
// and can still reproduce after a crash. Uncertain is reported as unknown
// rather than as a delivery that did or did not happen.
func TestHybridOutboundReportsDeliveryStatusByOperationKey(t *testing.T) {
	cases := map[string]emailtransport.DeliveryStatus{
		"sent":      emailtransport.DeliveryStatusSent,
		"accepted":  emailtransport.DeliveryStatusQueued,
		"claimed":   emailtransport.DeliveryStatusQueued,
		"uncertain": emailtransport.DeliveryStatusUnknown,
	}
	for status, want := range cases {
		t.Run(status, func(t *testing.T) {
			client, _ := newTestClient(t, "")
			server, state := newCloudServer(t, client.InstallationID, client.InboxID)
			client.BaseURL = server.URL
			var asked string
			state.handler = func(action string, input map[string]any) (int, any) {
				asked = fmt.Sprint(input["operation_key"])
				return http.StatusOK, SendReceipt{Status: status, ProviderMessageID: "p1"}
			}
			adapter := &OutboundAdapter{Client: client}
			got, err := adapter.GetDeliveryStatus(context.Background(), "outbox-42")
			if err != nil || got != want {
				t.Fatalf("status %s gave %q err=%v, want %q", status, got, err, want)
			}
			if asked != "outbox-42" {
				t.Fatalf("cloud was asked about %q", asked)
			}
		})
	}
}
