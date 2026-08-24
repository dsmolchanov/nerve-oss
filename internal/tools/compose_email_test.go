package tools

import (
	"net/mail"
	"testing"
)

func TestFormatSenderMailbox(t *testing.T) {
	tests := []struct {
		name            string
		fromName        string
		wantDisplayName string
	}{
		{name: "legacy bare address"},
		{name: "ASCII display name", fromName: "Agatha AI", wantDisplayName: "Agatha AI"},
		{name: "Unicode display name", fromName: "Агата AI", wantDisplayName: "Агата AI"},
		{name: "display name requiring quoting", fromName: "Doe, Jane", wantDisplayName: "Doe, Jane"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mailbox, address, err := formatSenderMailbox("support@ahata.ai", tt.fromName)
			if err != nil {
				t.Fatalf("format sender mailbox: %v", err)
			}
			if address != "support@ahata.ai" {
				t.Fatalf("expected bare sender address, got %q", address)
			}

			parsed, err := mail.ParseAddress(mailbox)
			if err != nil {
				t.Fatalf("parse formatted mailbox %q: %v", mailbox, err)
			}
			if parsed.Address != "support@ahata.ai" {
				t.Fatalf("expected parsed address support@ahata.ai, got %q", parsed.Address)
			}
			if parsed.Name != tt.wantDisplayName {
				t.Fatalf("expected display name %q, got %q", tt.wantDisplayName, parsed.Name)
			}
		})
	}
}

func TestNormalizeFromName(t *testing.T) {
	name, err := normalizeFromName("  Агата AI  ")
	if err != nil {
		t.Fatalf("normalize display name: %v", err)
	}
	if name != "Агата AI" {
		t.Fatalf("expected trimmed display name, got %q", name)
	}
}

func TestNormalizeFromNameRejectsControlCharacters(t *testing.T) {
	for _, value := range []string{
		"Agatha\r\nBcc: attacker@example.com",
		"Agatha\x00AI",
		"Agatha\tAI",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := normalizeFromName(value); err == nil || err.Error() != "invalid from_name" {
				t.Fatalf("expected invalid from_name, got %v", err)
			}
		})
	}
}

func TestFormatSenderMailboxRejectsInvalidAddress(t *testing.T) {
	if _, _, err := formatSenderMailbox("not an address", "Agatha AI"); err == nil || err.Error() != "invalid sender address" {
		t.Fatalf("expected invalid sender address, got %v", err)
	}
}

func TestCanonicalOutboundInboxAddressPreservesStorageOnlyInternally(t *testing.T) {
	for _, value := range []string{
		"Agent@Example.TEST",
		"\u00a0\u2003agent@example.test\u3000",
		"agent@example.test.",
	} {
		t.Run(value, func(t *testing.T) {
			got, err := canonicalOutboundInboxAddress(value)
			if err != nil {
				t.Fatal(err)
			}
			if got != "agent@example.test" {
				t.Fatalf("canonical sender=%q", got)
			}
		})
	}
	if _, err := canonicalOutboundInboxAddress("agent@@example.test"); err == nil || err.Error() != "invalid sender address" {
		t.Fatalf("invalid legacy sender error=%v", err)
	}
}
