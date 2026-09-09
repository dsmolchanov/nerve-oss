package startup

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestSchemaQuiescenceRejectsBeforeApplicationStartup(t *testing.T) {
	for _, tc := range []struct {
		mode          string
		cloud         bool
		command       string
		handled, fail bool
	}{
		{"", true, "serve", false, false}, {"invalid", true, "serve", true, true},
		{"quiescent", false, "serve", true, true}, {"quiescent", true, "mcp-stdio", true, true},
		{"quiescent", true, "migrate-core", true, true},
	} {
		got, err := SchemaQuiescence(context.Background(), tc.mode, tc.cloud, tc.command, "")
		if got != tc.handled || (err != nil) != tc.fail {
			t.Fatalf("%+v => %v, %v", tc, got, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if handled, err := SchemaQuiescence(ctx, "quiescent", true, "worker", ""); !handled || err != nil {
		t.Fatalf("worker: %v %v", handled, err)
	}
}

func TestSchemaQuiescenceDeniesEveryIngressAndStops(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- serveQuiescence(ctx, listener) }()
	client := &http.Client{Timeout: time.Second}
	for _, method := range []string{"GET", "POST", "PUT", "DELETE", "HEAD", "OPTIONS"} {
		for _, path := range []string{"/health", "/mcp", "/v1/billing", "/webhooks/resend", "/webhooks/stripe"} {
			req, _ := http.NewRequest(method, "http://"+listener.Addr().String()+path, nil)
			req.Header.Set("Authorization", "Bearer previously-issued-token")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 503 || resp.Header.Get("Retry-After") != "60" || resp.Header.Get("Cache-Control") != "no-store" {
				t.Fatalf("%s %s: %v", method, path, resp)
			}
		}
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("quiescence did not stop")
	}
}
