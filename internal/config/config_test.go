package config

import (
	"os"
	"testing"
)

func TestLoadEnvOverrides(t *testing.T) {
	t.Setenv("NM_JMAP_URL", "http://example.com/jmap")
	t.Setenv("NM_HTTP_ADDR", ":9000")
	t.Setenv("NM_DEV_MODE", "false")

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

	_ = os.Unsetenv("NM_JMAP_URL")
}
