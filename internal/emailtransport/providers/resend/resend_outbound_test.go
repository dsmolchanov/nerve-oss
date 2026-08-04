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

func TestResendOutboundAdapterRetriesRateLimit(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"statusCode":429,"message":"rate limited"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"email_456"}`))
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
		TextBody: "body",
	}, "idem-retry")
	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if id != "email_456" {
		t.Fatalf("expected email_456, got %s", id)
	}
	if attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts)
	}
}

func TestResendOutboundAdapterNoRetryOnClientError(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"invalid api key"}`))
	}))
	defer srv.Close()

	adapter := NewOutboundAdapter(Config{
		APIKey:     "bad_key",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
	})

	_, err := adapter.SendMessage(context.Background(), emailtransport.OutboundMessage{
		From:     "sender@example.com",
		To:       []string{"to@example.com"},
		Subject:  "Hello",
		TextBody: "body",
	}, "")
	if err == nil {
		t.Fatal("expected error for 403")
	}
	if attempts != 1 {
		t.Fatalf("should not retry 403, got %d attempts", attempts)
	}
}
