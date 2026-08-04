package store

// WebhookEventTypes is the complete customer-selectable event allowlist.
// Keep the order stable because the control-plane validation error returns it
// verbatim to API clients.
var WebhookEventTypes = []string{
	"email.sent",
	"email.delivered",
	"email.delivery_delayed",
	"email.bounced",
	"email.failed",
	"email.complained",
	"email.suppressed",
	"email.received",
}

// SensitiveWebhookEventTypes must always require explicit subscription.
// Empty events arrays continue to mean all non-sensitive events only.
var SensitiveWebhookEventTypes = map[string]bool{
	"email.received": true,
}

func IsValidWebhookEventType(eventType string) bool {
	for _, candidate := range WebhookEventTypes {
		if candidate == eventType {
			return true
		}
	}
	return false
}
