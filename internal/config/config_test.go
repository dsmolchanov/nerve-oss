package config

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
)

func TestLoadEnvOverrides(t *testing.T) {
	t.Setenv("NM_JMAP_URL", "http://example.com/jmap")
	t.Setenv("NM_HTTP_ADDR", ":9000")
	t.Setenv("NM_DEV_MODE", "false")
	t.Setenv("NM_CLOUD_MODE", "true")
	t.Setenv("NM_CLOUD_PUBLIC_BASE_URL", "https://cloud.nerve.email")
	t.Setenv("NM_AUTH_ISSUER", "https://auth.nerve.email")
	t.Setenv("NM_AUTH_AUDIENCE", "nerve-runtime")
	t.Setenv("NM_AUTH_JWKS_URL", "https://auth.nerve.email/.well-known/jwks.json")
	t.Setenv("NM_BILLING_PROVIDER", "stripe")
	t.Setenv("NM_STRIPE_SECRET_KEY", "sk_test_123")
	t.Setenv("NM_STRIPE_WEBHOOK_SECRET", "whsec_test_123")
	t.Setenv("NM_METER_TOOL_COST_PATH", "configs/meters/custom_costs.yaml")
	t.Setenv("NM_METER_PAST_DUE_GRACE_DAYS", "14")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.JMAP.URL != "http://example.com/jmap" {
		t.Fatalf("expected jmap url override")
	}
	if cfg.HTTP.Addr != ":9000" {
		t.Fatalf("expected http addr override")
	}
	if cfg.Dev.Mode {
		t.Fatalf("expected dev mode false")
	}
	if !cfg.Cloud.Mode {
		t.Fatalf("expected cloud mode true")
	}
	if cfg.Cloud.PublicBaseURL != "https://cloud.nerve.email" {
		t.Fatalf("expected cloud public base url override")
	}
	if cfg.Auth.Issuer != "https://auth.nerve.email" {
		t.Fatalf("expected auth issuer override")
	}
	if cfg.Auth.Audience != "nerve-runtime" {
		t.Fatalf("expected auth audience override")
	}
	if cfg.Auth.JWKSURL != "https://auth.nerve.email/.well-known/jwks.json" {
		t.Fatalf("expected auth jwks url override")
	}
	if cfg.Billing.Provider != "stripe" {
		t.Fatalf("expected billing provider override")
	}
	if cfg.Billing.StripeSecretKey != "sk_test_123" {
		t.Fatalf("expected stripe secret key override")
	}
	if cfg.Billing.StripeWebhookSecret != "whsec_test_123" {
		t.Fatalf("expected stripe webhook secret override")
	}
	if cfg.Metering.ToolCostPath != "configs/meters/custom_costs.yaml" {
		t.Fatalf("expected metering tool cost path override")
	}
	if cfg.Metering.PastDueGraceDays != 14 {
		t.Fatalf("expected metering grace-day override")
	}

	_ = os.Unsetenv("NM_JMAP_URL")
}

func TestLoadEnvOverridesPrefersNERVEPrefix(t *testing.T) {
	t.Setenv("NM_HTTP_ADDR", ":9000")
	t.Setenv("NERVE_HTTP_ADDR", ":9100")
	t.Setenv("NM_SMTP_FROM", "legacy@local.neuralmail")
	t.Setenv("NERVE_SMTP_FROM", "modern@local.nerve.email")
	t.Setenv("NM_CLOUD_MODE", "false")
	t.Setenv("NERVE_CLOUD_MODE", "true")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.HTTP.Addr != ":9100" {
		t.Fatalf("expected NERVE_HTTP_ADDR to override NM_HTTP_ADDR, got %q", cfg.HTTP.Addr)
	}
	if cfg.SMTP.From != "modern@local.nerve.email" {
		t.Fatalf("expected NERVE_SMTP_FROM to override NM_SMTP_FROM, got %q", cfg.SMTP.From)
	}
	if !cfg.Cloud.Mode {
		t.Fatalf("expected NERVE_CLOUD_MODE to override NM_CLOUD_MODE")
	}
}

func TestConfigPathFromEnvPrefersNERVE(t *testing.T) {
	t.Setenv("NM_CONFIG", "configs/dev/host.yaml")
	t.Setenv("NERVE_CONFIG", "configs/dev/cortex.yaml")
	path := ConfigPathFromEnv()
	if path != "configs/dev/cortex.yaml" {
		t.Fatalf("expected NERVE_CONFIG precedence, got %q", path)
	}
}

func TestDefaultUsesModernLocalDomain(t *testing.T) {
	cfg := Default()
	if cfg.SMTP.From != "dev@local.nerve.email" {
		t.Fatalf("expected modern SMTP default, got %q", cfg.SMTP.From)
	}
	if cfg.SMTP.HeloDomain != "local.nerve.email" {
		t.Fatalf("expected modern HELO default, got %q", cfg.SMTP.HeloDomain)
	}
}

func TestLoadWarnsOnLegacyEnvUsage(t *testing.T) {
	t.Setenv("NM_SMTP_HOST", "legacy-host")
	t.Setenv("NERVE_SMTP_HOST", "")

	var logs bytes.Buffer
	origWriter := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(origWriter)
		log.SetFlags(origFlags)
	})

	if _, err := Load(""); err != nil {
		t.Fatalf("load: %v", err)
	}

	if !strings.Contains(logs.String(), "NM_SMTP_HOST") {
		t.Fatalf("expected deprecation warning to mention NM_SMTP_HOST, got: %s", logs.String())
	}
}
