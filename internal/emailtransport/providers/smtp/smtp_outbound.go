package smtp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"

	"github.com/google/uuid"

	"neuralmail/internal/emailtransport"
)

type Config struct {
	Host            string
	Port            int
	Username        string
	Password        string
	RequireStartTLS bool
	HeloDomain      string
}

type OutboundAdapter struct {
	cfg Config
}

func NewOutboundAdapter(cfg Config) *OutboundAdapter {
	if cfg.Host == "" {
		cfg.Host = "localhost"
	}
	if cfg.Port == 0 {
		cfg.Port = 25
	}
	if strings.TrimSpace(cfg.HeloDomain) == "" {
		cfg.HeloDomain = "local.nerve.email"
	}
	return &OutboundAdapter{cfg: cfg}
}

func (a *OutboundAdapter) Name() string { return "smtp" }

func normalizeNewlines(input string) string {
	if input == "" {
		return ""
	}
	// Normalize to \n first, then convert to CRLF for SMTP.
	s := strings.ReplaceAll(input, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

func containsNewline(value string) bool {
	return strings.Contains(value, "\n") || strings.Contains(value, "\r")
}

func envelopeAddress(value string) (string, error) {
	if containsNewline(value) {
		return "", errors.New("invalid from")
	}
	parsed, err := mail.ParseAddress(strings.TrimSpace(value))
	if err != nil || strings.TrimSpace(parsed.Address) == "" {
		return "", errors.New("invalid from")
	}
	return strings.TrimSpace(parsed.Address), nil
}

func buildMIMEMessage(msg emailtransport.OutboundMessage) (string, error) {
	from := strings.TrimSpace(msg.From)
	if from == "" {
		return "", errors.New("missing from")
	}
	if len(msg.To) == 0 {
		return "", errors.New("missing to")
	}
	to := strings.TrimSpace(msg.To[0])
	subject := strings.TrimSpace(msg.Subject)

	if containsNewline(from) || containsNewline(to) || containsNewline(subject) {
		return "", errors.New("invalid header value")
	}

	textBody := normalizeNewlines(msg.TextBody)
	htmlBody := normalizeNewlines(msg.HTMLBody)

	headers := []string{
		"From: " + from,
		"To: " + to,
		"Subject: " + subject,
		"MIME-Version: 1.0",
	}
	for k, v := range msg.Headers {
		key := strings.TrimSpace(k)
		val := strings.TrimSpace(v)
		if key == "" || containsNewline(key) || containsNewline(val) {
			return "", errors.New("invalid header")
		}
		// Basic injection hardening: forbid colon in key.
		if strings.Contains(key, ":") {
			return "", errors.New("invalid header key")
		}
		headers = append(headers, key+": "+val)
	}

	// Default to plain text if no HTML is provided.
	if strings.TrimSpace(htmlBody) == "" {
		headers = append(headers, "Content-Type: text/plain; charset=utf-8")
		return strings.Join(append(headers, "", textBody), "\r\n"), nil
	}

	// multipart/alternative with both text/plain and text/html.
	// If caller didn't provide a plain text part, send a minimal one.
	if strings.TrimSpace(textBody) == "" {
		textBody = "This message contains HTML content. Use an HTML-capable email client to view it."
	}

	boundary := "nerve_" + uuid.NewString()
	headers = append(headers, fmt.Sprintf("Content-Type: multipart/alternative; boundary=\"%s\"", boundary))

	var b strings.Builder
	b.WriteString(strings.Join(headers, "\r\n"))
	b.WriteString("\r\n\r\n")

	// text/plain part
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(textBody)
	b.WriteString("\r\n")

	// text/html part
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/html; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(htmlBody)
	b.WriteString("\r\n")

	b.WriteString("--" + boundary + "--\r\n")
	return b.String(), nil
}

func (a *OutboundAdapter) SendMessage(ctx context.Context, msg emailtransport.OutboundMessage, key string) (string, error) {
	id, err := a.doSend(ctx, msg, key)
	if err == nil {
		return id, nil
	}
	// If the error is already classified (e.g. from a nested call), pass
	// it through untouched. Otherwise classify SMTP response codes and
	// network errors into a ProviderError so the outbox worker can decide
	// whether to retry.
	var pe *emailtransport.ProviderError
	if errors.As(err, &pe) {
		return id, err
	}
	return id, classifySMTPError(err)
}

func (a *OutboundAdapter) doSend(ctx context.Context, msg emailtransport.OutboundMessage, _ string) (string, error) {
	addr := fmt.Sprintf("%s:%d", a.cfg.Host, a.cfg.Port)
	payload, err := buildMIMEMessage(msg)
	if err != nil {
		return "", err
	}
	envelopeFrom, err := envelopeAddress(msg.From)
	if err != nil {
		return "", err
	}

	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	client, err := smtp.NewClient(conn, a.cfg.Host)
	if err != nil {
		return "", err
	}
	defer client.Close()

	if err := client.Hello(a.cfg.HeloDomain); err != nil {
		return "", err
	}

	if ok, _ := client.Extension("STARTTLS"); ok {
		tlsCfg := &tls.Config{
			ServerName: a.cfg.Host,
			MinVersion: tls.VersionTLS12,
		}
		if err := client.StartTLS(tlsCfg); err != nil {
			return "", err
		}
	} else if a.cfg.RequireStartTLS {
		return "", errors.New("smtp server does not support STARTTLS")
	}

	if (a.cfg.Username != "" || a.cfg.Password != "") && supportsAuth(client) {
		auth := smtp.PlainAuth("", a.cfg.Username, a.cfg.Password, a.cfg.Host)
		if err := client.Auth(auth); err != nil {
			return "", err
		}
	}

	if err := client.Mail(envelopeFrom); err != nil {
		return "", err
	}
	for _, rcpt := range msg.To {
		if err := client.Rcpt(rcpt); err != nil {
			return "", err
		}
	}

	writer, err := client.Data()
	if err != nil {
		return "", err
	}
	if _, err := writer.Write([]byte(payload)); err != nil {
		_ = writer.Close()
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	_ = client.Quit()
	// SMTP has no stable provider message ID; the message row ID is the durable identifier.
	return "", nil
}

// classifySMTPError turns a net/smtp error into a ProviderError. SMTP
// response codes in the 5xx range (550, 551, 553, etc.) are permanent
// failures — no amount of retry will deliver to an invalid recipient or
// over a forbidden auth method. 4xx responses (421, 450, 451) are
// transient. Transport errors (dial, TLS, read) are transient by default.
func classifySMTPError(err error) *emailtransport.ProviderError {
	if err == nil {
		return nil
	}
	var te *textproto.Error
	if errors.As(err, &te) {
		reason := smtpReasonForCode(te.Code)
		if te.Code >= 500 && te.Code < 600 {
			return emailtransport.NewPermanentError(te.Code, reason, err)
		}
		if te.Code >= 400 && te.Code < 500 {
			return emailtransport.NewTransientError(te.Code, reason, err)
		}
	}
	// Dial failures, TLS handshake errors, writer errors — all transient
	// unless the upstream caller has already decided otherwise.
	return emailtransport.NewTransientError(0, "network_error", err)
}

func smtpReasonForCode(code int) string {
	switch code {
	case 421:
		return "service_unavailable"
	case 450, 451, 452:
		return "mailbox_busy"
	case 550:
		return "mailbox_unavailable"
	case 551, 553:
		return "invalid_recipient"
	case 552:
		return "storage_exceeded"
	case 554:
		return "transaction_failed"
	default:
		return "smtp_error"
	}
}

func (a *OutboundAdapter) GetDeliveryStatus(context.Context, string) (emailtransport.DeliveryStatus, error) {
	return emailtransport.DeliveryStatusUnknown, emailtransport.ErrNotSupported
}

func supportsAuth(client *smtp.Client) bool {
	ok, _ := client.Extension("AUTH")
	return ok
}

var _ emailtransport.OutboundAdapter = (*OutboundAdapter)(nil)
