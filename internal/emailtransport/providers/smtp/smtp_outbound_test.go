package smtp

import (
	"net/mail"
	"strings"
	"testing"

	"neuralmail/internal/emailtransport"
	"neuralmail/internal/store"
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

func TestBuildMIMEMessageAttachmentGolden(t *testing.T) {
	msg := emailtransport.OutboundMessage{
		From:     "sender@example.com",
		To:       []string{"recipient@example.com"},
		Subject:  "Résumé",
		TextBody: "Body",
		Attachments: []store.OutboundAttachment{
			{Filename: "résumé 2026.pdf", ContentType: "application/pdf", Content: []byte("PDF")},
		},
	}
	boundary := mimeBoundary("mixed", msg)
	if boundary != "nerve_mixed_0ee2f6f3dece62e765368ccf" {
		t.Fatalf("boundary=%q", boundary)
	}
	payload, err := buildMIMEMessage(msg)
	if err != nil {
		t.Fatal(err)
	}
	want := "From: sender@example.com\r\n" +
		"To: recipient@example.com\r\n" +
		"Subject: Résumé\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"" + boundary + "\"\r\n\r\n" +
		"--" + boundary + "\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		"Body\r\n" +
		"--" + boundary + "\r\n" +
		"Content-Type: application/pdf; filename*=UTF-8''r%C3%A9sum%C3%A9%202026.pdf\r\n" +
		"Content-Disposition: attachment; filename*=UTF-8''r%C3%A9sum%C3%A9%202026.pdf\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		"UERG\r\n" +
		"--" + boundary + "--\r\n"
	if payload != want {
		t.Fatalf("MIME payload mismatch\n--- got ---\n%q\n--- want ---\n%q", payload, want)
	}
}
