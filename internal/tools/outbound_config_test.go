package tools

import (
	"errors"
	"testing"

	"neuralmail/internal/config"
	"neuralmail/internal/emailtransport"
	smtptransport "neuralmail/internal/emailtransport/providers/smtp"
)

func TestLocalOutboundConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, to, host string
		port           int
		allow          bool
		provider       string
		want           string
	}{
		{"disabled", "a@example.test", "localhost", 2525, false, "smtp", "outbound_disabled"},
		{"sandbox", "a@local.nerve.email", "localhost", 2525, false, "smtp", ""},
		{"missing host", "a@example.test", "", 2525, true, "smtp", "smtp_unconfigured"},
		{"invalid port", "a@example.test", "localhost", 0, true, "smtp", "smtp_unconfigured"},
		{"out of range", "a@example.test", "localhost", 65536, true, "smtp", "smtp_unconfigured"},
		{"unknown transport", "a@example.test", "localhost", 2525, true, "unknown", "outbound_transport_unconfigured"},
		{"ready", "a@example.test", "localhost", 2525, true, "smtp", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Security.AllowOutbound = tc.allow
			cfg.SMTP.Host = tc.host
			cfg.SMTP.Port = tc.port
			registry := emailtransport.NewRegistry()
			_ = registry.RegisterOutbound(smtptransport.NewOutboundAdapter(smtptransport.Config{}))
			svc := Service{Config: cfg, Transport: registry}
			err := svc.checkOutboundConfiguration(tc.to, tc.provider)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var setup *OutboundConfigurationError
			if !errors.As(err, &setup) || setup.Code != tc.want || setup.Remediation == "" {
				t.Fatalf("error=%#v", err)
			}
		})
	}
}

func TestCloudOutboundConfigurationPreservesLegacyErrors(t *testing.T) {
	cfg := config.Default()
	cfg.Cloud.Mode = true
	svc := Service{Config: cfg}
	err := svc.checkOutboundConfiguration("a@example.test", "smtp")
	if err == nil || err.Error() != "outbound disabled for non-local domains" {
		t.Fatal(err)
	}
	var setup *OutboundConfigurationError
	if errors.As(err, &setup) {
		t.Fatal("changed cloud error contract")
	}
}
