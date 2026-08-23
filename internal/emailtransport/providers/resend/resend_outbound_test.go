package resend

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"neuralmail/internal/emailtransport"
	"neuralmail/internal/store"
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
		Attachments: []store.OutboundAttachment{
			{Filename: "first.txt", ContentType: "text/plain", Content: []byte("first bytes")},
			{Filename: "second.pdf", ContentType: "application/pdf", Content: []byte{0, 1, 2, 3}},
		},
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
	attachments, _ := gotBody["attachments"].([]any)
	if len(attachments) != 2 {
		t.Fatalf("attachments=%#v, want two", gotBody["attachments"])
	}
	first, _ := attachments[0].(map[string]any)
	second, _ := attachments[1].(map[string]any)
	if first["filename"] != "first.txt" || first["content"] != base64.StdEncoding.EncodeToString([]byte("first bytes")) {
		t.Fatalf("first attachment=%#v", first)
	}
	if second["filename"] != "second.pdf" || second["content"] != base64.StdEncoding.EncodeToString([]byte{0, 1, 2, 3}) {
		t.Fatalf("second attachment=%#v", second)
	}
}

func TestResendOutboundReplayPayloadIsByteStable(t *testing.T) {
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"email_stable"}`))
	}))
	defer srv.Close()

	adapter := NewOutboundAdapter(Config{APIKey: "re_test_key", BaseURL: srv.URL, HTTPClient: srv.Client()})
	message := emailtransport.OutboundMessage{
		From: "sender@example.com", To: []string{"to@example.com"}, Subject: "Stable", TextBody: "body",
		Tags: map[string]string{"zeta": "last", "alpha": "first", "middle": "value"},
	}
	for i := 0; i < 2; i++ {
		if _, err := adapter.SendMessage(context.Background(), message, "same-operation"); err != nil {
			t.Fatal(err)
		}
	}
	if len(bodies) != 2 || string(bodies[0]) != string(bodies[1]) {
		t.Fatalf("replay payload drifted:\nfirst=%s\nsecond=%s", bodies[0], bodies[1])
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

func TestResendOutboundAdapterRedactsProviderErrorBodies(t *testing.T) {
	const (
		secretMarker    = "re_live_response_secret"
		oversizedMarker = "OVERSIZED_PROVIDER_SECRET"
		malformedMarker = "MALFORMED_PROVIDER_SECRET"
	)
	tests := []struct {
		name   string
		body   string
		marker string
	}{
		{name: "secret JSON", body: `{"message":"` + secretMarker + `"}`, marker: secretMarker},
		{
			name:   "oversized body",
			body:   strings.Repeat(oversizedMarker, (1<<20)/len(oversizedMarker)+1),
			marker: oversizedMarker,
		},
		{
			name:   "malformed body",
			body:   string([]byte{0xff, 0xfe, 0xfd}) + malformedMarker,
			marker: malformedMarker,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: outboundRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusForbidden,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(tc.body)),
				}, nil
			})}
			adapter := NewOutboundAdapter(Config{
				APIKey: "re_request_secret", BaseURL: "https://api.resend.invalid", HTTPClient: client,
			})

			_, err := adapter.SendMessage(context.Background(), emailtransport.OutboundMessage{
				From: "sender@example.com", To: []string{"to@example.com"}, Subject: "Hello", TextBody: "body",
			}, "redaction-test")
			if err == nil {
				t.Fatal("expected provider error")
			}
			var providerErr *emailtransport.ProviderError
			if !errors.As(err, &providerErr) {
				t.Fatalf("error type=%T, want *emailtransport.ProviderError", err)
			}
			if !providerErr.Permanent || providerErr.StatusCode != http.StatusForbidden || providerErr.Reason != "forbidden" {
				t.Fatalf("provider error=%+v", providerErr)
			}
			got := err.Error()
			want := "resend send failed: status=403 reason=forbidden retryable=false"
			if got != want {
				t.Fatalf("error=%q, want %q", got, want)
			}
			if len(got) > maxResendOutboundErrorBytes {
				t.Fatalf("error length=%d, max=%d", len(got), maxResendOutboundErrorBytes)
			}
			if strings.Contains(got, tc.marker) || strings.Contains(got, "re_request_secret") {
				t.Fatalf("provider/request secret leaked in error: %q", got)
			}
		})
	}
}

func TestResendOutboundProviderErrorHasBoundedRetryDetails(t *testing.T) {
	tests := []struct {
		status    int
		permanent bool
		reason    string
		retryable string
	}{
		{status: http.StatusForbidden, permanent: true, reason: "forbidden", retryable: "retryable=false"},
		{status: http.StatusTooManyRequests, permanent: false, reason: "rate_limited", retryable: "retryable=true"},
		{status: http.StatusServiceUnavailable, permanent: false, reason: "server_error", retryable: "retryable=true"},
	}
	for _, tc := range tests {
		err := newResendSendProviderError(tc.status)
		if err.StatusCode != tc.status || err.Permanent != tc.permanent || err.Reason != tc.reason {
			t.Fatalf("status %d: provider error=%+v", tc.status, err)
		}
		if len(err.Error()) > maxResendOutboundErrorBytes {
			t.Fatalf("status %d: error length=%d, max=%d", tc.status, len(err.Error()), maxResendOutboundErrorBytes)
		}
		if !strings.Contains(err.Error(), tc.retryable) {
			t.Fatalf("status %d: error=%q, want %q", tc.status, err.Error(), tc.retryable)
		}
	}
}

type outboundRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn outboundRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}
