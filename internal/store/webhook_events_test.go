package store

import "testing"

func TestWebhookEventTypeAllowlist(t *testing.T) {
	seen := make(map[string]bool, len(WebhookEventTypes))
	for _, eventType := range WebhookEventTypes {
		if seen[eventType] {
			t.Fatalf("duplicate webhook event type %q", eventType)
		}
		seen[eventType] = true
		if !IsValidWebhookEventType(eventType) {
			t.Fatalf("allowlisted event %q was rejected", eventType)
		}
	}
	if len(seen) != 8 {
		t.Fatalf("event allowlist size=%d, want seven outbound plus email.received", len(seen))
	}
	if !SensitiveWebhookEventTypes["email.received"] {
		t.Fatal("email.received must remain explicitly sensitive")
	}
	if IsValidWebhookEventType("email.unknown") {
		t.Fatal("unknown event type was accepted")
	}
}
