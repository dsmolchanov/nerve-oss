package resend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"neuralmail/internal/emailtransport"
)

func TestResendOutboundAdapterSendsWithIdempotencyHeader(t *testing.T) {
	var gotAuth string
	var gotIdem string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/emails" {
			t.Fatalf("expected /emails, got %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		gotIdem = r.Header.Get("Idempotency-Key")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"email_123"}`))
	}))
	defer srv.Close()

	adapter := NewOutboundAdapter(Config{
		APIKey:     "re_test_key",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
	})

	id, err := adapter.SendMessage(context.Background(), emailtransport.OutboundMessage{
		From:     "sender@example.com",
		To:       []string{"to@example.com"},
		Subject:  "Hello",
		TextBody: "Plain",
		HTMLBody: "<p>HTML</p>",
	}, "idem-1")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if id != "email_123" {
		t.Fatalf("expected provider id email_123, got %s", id)
	}
	if gotAuth != "Bearer re_test_key" {
		t.Fatalf("expected Authorization Bearer, got %q", gotAuth)
	}
	if gotIdem != "idem-1" {
		t.Fatalf("expected Idempotency-Key idem-1, got %q", gotIdem)
	}

	if gotBody["from"] != "sender@example.com" {
		t.Fatalf("expected from in body, got %#v", gotBody["from"])
	}
	to, _ := gotBody["to"].([]any)
	if len(to) != 1 || to[0] != "to@example.com" {
		t.Fatalf("expected to array, got %#v", gotBody["to"])
	}
	if gotBody["subject"] != "Hello" {
		t.Fatalf("expected subject, got %#v", gotBody["subject"])
	}
	if gotBody["text"] != "Plain" {
		t.Fatalf("expected text, got %#v", gotBody["text"])
	}
	if gotBody["html"] != "<p>HTML</p>" {
		t.Fatalf("expected html, got %#v", gotBody["html"])
	}
}
