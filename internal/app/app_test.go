package app

import (
	"strings"
	"testing"
	"time"

	"neuralmail/internal/config"
	"neuralmail/internal/mcp"
)

func TestNewOnboardingProvisionerIsFailClosedUntilFullyConfigured(t *testing.T) {
	cfg := config.Default()
	provisioner, err := newOnboardingProvisioner(cfg)
	if err != nil || provisioner != nil {
		t.Fatalf("unconfigured provisioner=%T err=%v", provisioner, err)
	}

	cfg.Onboarding.ControlPlaneURL = "https://control.internal.nerve.email"
	if _, err := newOnboardingProvisioner(cfg); err == nil || !strings.Contains(err.Error(), "requires control-plane URL, key ID, and secret together") {
		t.Fatalf("partial configuration error=%v", err)
	}
}

func TestNewOnboardingProvisionerBuildsBoundedFixedOriginClient(t *testing.T) {
	cfg := config.Default()
	cfg.Onboarding.ControlPlaneURL = "https://control.internal.nerve.email"
	cfg.Onboarding.DelegationKeyID = "runtime-current"
	cfg.Onboarding.DelegationSecret = "delegation-secret"
	cfg.Onboarding.Timeout = 3 * time.Second
	provisioner, err := newOnboardingProvisioner(cfg)
	if err != nil || provisioner == nil {
		t.Fatalf("configured provisioner=%T err=%v", provisioner, err)
	}
	runtime := newMCPServer(cfg, nil, nil, nil, nil, provisioner)
	if runtime.Onboarding == nil {
		t.Fatal("production MCP server did not register the configured onboarding provisioner")
	}
	delegated, err := newDelegationClient(cfg)
	if err != nil {
		t.Fatalf("delegation client: %v", err)
	}
	if _, ok := any(delegated).(mcp.BillingProvisioner); !ok {
		t.Fatal("production delegation client does not implement billing provisioner")
	}

	cfg.Onboarding.ControlPlaneURL = "http://control.internal.nerve.email"
	if _, err := newOnboardingProvisioner(cfg); err == nil || !strings.Contains(err.Error(), "must use HTTPS") {
		t.Fatalf("unsafe origin error=%v", err)
	}
}
