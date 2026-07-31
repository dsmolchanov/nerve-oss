package smtp

import (
	"net/mail"
	"strings"
	"testing"

	"neuralmail/internal/emailtransport"
)

func TestEnvelopeAddressUsesBareMailbox(t *testing.T) {
	formatted := (&mail.Address{Name: "Агата AI", Address: "support@ahata.ai"}).String()

	for _, value := range []string{"support@ahata.ai", "Agatha AI <support@ahata.ai>", formatted} {
		t.Run(value, func(t *testing.T) {
			got, err := envelopeAddress(value)
			if err != nil {
				t.Fatalf("extract envelope address: %v", err)
			}
			if got != "support@ahata.ai" {
				t.Fatalf("expected bare envelope sender, got %q", got)
			}
		})
	}
}

func TestEnvelopeAddressRejectsHeaderInjection(t *testing.T) {
	if _, err := envelopeAddress("Agatha <support@ahata.ai>\r\nBcc: attacker@example.com"); err == nil {
		t.Fatal("expected newline in sender mailbox to be rejected")
	}
}

func TestBuildMIMEMessagePreservesDisplayName(t *testing.T) {
	from := (&mail.Address{Name: "Агата AI", Address: "support@ahata.ai"}).String()
	payload, err := buildMIMEMessage(emailtransport.OutboundMessage{
		From:     from,
		To:       []string{"invitee@example.com"},
		Subject:  "Invitation",
		TextBody: "Open the invitation link",
	})
	if err != nil {
		t.Fatalf("build MIME message: %v", err)
	}

	message, err := mail.ReadMessage(strings.NewReader(payload))
	if err != nil {
		t.Fatalf("parse MIME message: %v", err)
	}
	parsed, err := mail.ParseAddress(message.Header.Get("From"))
	if err != nil {
		t.Fatalf("parse MIME From header: %v", err)
	}
	if parsed.Name != "Агата AI" || parsed.Address != "support@ahata.ai" {
		t.Fatalf("unexpected MIME From header: %#v", parsed)
	}
}
