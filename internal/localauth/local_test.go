package localauth

import (
	"net/http/httptest"
	"neuralmail/internal/config"
	"testing"
)

func TestLocalBearerIdentity(t *testing.T) {
	cfg := config.Default()
	cfg.Security.APIKey = "admin-test"
	cfg.Security.LocalAPIKeys = []config.LocalAPIKey{{Token: "mailbox-test", InboxIDs: []string{"inbox-a"}}}
	for _, tt := range []struct {
		header         string
		ok, restricted bool
	}{
		{"", false, false}, {"Basic admin-test", false, false}, {"Bearer wrong", false, false},
		{"Bearer admin-test", true, false}, {"Bearer mailbox-test", true, true},
	} {
		req := httptest.NewRequest("POST", "/mcp", nil)
		req.Header.Set("Authorization", tt.header)
		ctx, err := Authenticate(cfg, req)
		if (err == nil) != tt.ok {
			t.Fatalf("auth ok=%v err=%v", tt.ok, err)
		}
		if err == nil {
			p, restricted := FromContext(ctx)
			if restricted != tt.restricted || (restricted && (len(p.InboxIDs) != 1 || p.InboxIDs[0] != "inbox-a")) {
				t.Fatal("wrong identity")
			}
		}
	}
	req := httptest.NewRequest("POST", "/mcp", nil)
	req.Header.Add("Authorization", "Bearer admin-test")
	req.Header.Add("Authorization", "Bearer mailbox-test")
	if _, err := Authenticate(cfg, req); err == nil {
		t.Fatal("duplicate bearer accepted")
	}
}
