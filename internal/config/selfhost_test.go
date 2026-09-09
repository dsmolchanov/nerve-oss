package config

import "testing"

func TestSelfhostBindRequiresExplicitExposure(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8088", "[::1]:8088", "localhost:8088", ":8088", "0.0.0.0:8088", "[::]:8088", "192.0.2.1:8088"} {
		for _, mode := range []string{"anonymous", "key", "override", "cloud"} {
			cfg := Default()
			cfg.HTTP.Addr = addr
			switch mode {
			case "key":
				cfg.Security.APIKey = "test-key"
			case "override":
				cfg.Security.AllowUnauthenticated = true
			case "cloud":
				cfg.Cloud.Mode = true
			}
			wantErr := mode == "anonymous" && addr != "127.0.0.1:8088" && addr != "[::1]:8088" && addr != "localhost:8088"
			if err := cfg.ValidateSelfhost(); (err != nil) != wantErr {
				t.Fatalf("%s/%s: %v", addr, mode, err)
			}
		}
	}
	cfg := Default()
	cfg.Security.APIKey = "duplicate"
	cfg.Security.LocalAPIKeys = []LocalAPIKey{{Token: "duplicate", InboxIDs: []string{"inbox"}}}
	if cfg.ValidateSelfhost() == nil {
		t.Fatal("duplicate admin/mailbox identity accepted")
	}
	cfg.Security.APIKey = ""
	cfg.Security.LocalAPIKeys[0].InboxIDs = nil
	if cfg.ValidateSelfhost() == nil {
		t.Fatal("empty inbox set accepted")
	}
}
