//go:build mcp2026artifact

package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"neuralmail/internal/auth"
)

const immutableSDK02Python = `
import asyncio
import sys

from nerve_email import NerveClient, __version__


async def main():
    if __version__ != "0.2.0":
        raise SystemExit(f"unexpected SDK version: {__version__}")
    async with NerveClient(
        base_url=sys.argv[1],
        rest_base_url=sys.argv[1],
        api_key="immutable-sdk-0.2-proof",
        timeout=15,
        max_retries=0,
    ) as client:
        if not await client.health_check():
            raise SystemExit("SDK 0.2 health check failed")
        tools = await client.list_tools()
        if not tools:
            raise SystemExit("SDK 0.2 legacy tools/list was empty")
    print("immutable-sdk-0.2-ok")


asyncio.run(main())
`

func TestImmutableSDK02AndNativeMCP2026ShareEndpoint(t *testing.T) {
	python := os.Getenv("NERVE_IMMUTABLE_SDK_0_2_PYTHON")
	if python == "" {
		t.Fatal("NERVE_IMMUTABLE_SDK_0_2_PYTHON must name the verified SDK 0.2 environment")
	}

	cfg := hostedRouterConfig()
	authService := auth.NewService(cfg, nil)
	runtime := NewServer(cfg, nil, authService, nil)
	authenticator := authenticatorFunc(func(request *http.Request) (auth.Principal, error) {
		switch request.Header.Get("MCP-Protocol-Version") {
		case LegacyProtocolVersion:
			return auth.Principal{
				Kind: auth.PrincipalCloudAPIKey, OrgID: "legacy-org",
				Scopes: []string{"nerve:email.read"}, AuthMethod: "cloud_api_key",
			}, nil
		case ModernProtocolVersion:
			return auth.Principal{
				Kind: auth.PrincipalM2MOrg, OrgID: "modern-org", ClientID: "agent-client",
				Generation: 1, Scopes: []string{"nerve:email.read"}, AuthMethod: "m2m_bearer",
			}, nil
		default:
			return auth.Principal{}, fmt.Errorf("unexpected protocol at authenticator")
		}
	})
	router := NewRouter(cfg, authenticator, NewLegacyHandler(runtime), NewSDKHandler(runtime, true))
	barrier := newDualProfileBarrier(router)
	mux := http.NewServeMux()
	mux.Handle("/mcp", barrier)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	hosted := httptest.NewServer(mux)
	defer hosted.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	legacy := exec.CommandContext(ctx, python, "-c", immutableSDK02Python, hosted.URL)
	legacyOutput := &strings.Builder{}
	legacy.Stdout, legacy.Stderr = legacyOutput, legacyOutput
	if err := legacy.Start(); err != nil {
		t.Fatalf("start immutable SDK 0.2 client: %v", err)
	}

	client := newModernSDKTestClient()
	session, err := client.Connect(ctx, &sdkmcp.StreamableClientTransport{
		Endpoint: hosted.URL + "/mcp",
		HTTPClient: &http.Client{Transport: originRoundTripper{
			base: http.DefaultTransport, origin: "https://agent.example",
		}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		_ = legacy.Process.Kill()
		_ = legacy.Wait()
		t.Fatalf("connect native MCP 2026 client: %v; SDK 0.2 output=%s", err, legacyOutput.String())
	}
	defer session.Close()
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("native MCP 2026 tools/list: %v", err)
	}
	if listed.CacheScope != "private" || len(listed.Tools) == 0 {
		t.Fatalf("native MCP 2026 catalog is not private/non-empty: %#v", listed)
	}
	if err := legacy.Wait(); err != nil {
		t.Fatalf("immutable SDK 0.2 client: %v; output=%s", err, legacyOutput.String())
	}
	if !strings.Contains(legacyOutput.String(), "immutable-sdk-0.2-ok") {
		t.Fatalf("immutable SDK 0.2 proof marker missing: %s", legacyOutput.String())
	}
	if !barrier.sawBoth() {
		t.Fatal("both protocol profiles did not reach the shared /mcp endpoint concurrently")
	}
}

type dualProfileBarrier struct {
	next  http.Handler
	ready chan struct{}
	once  sync.Once
	mu    sync.Mutex
	seen  map[string]bool
}

func newDualProfileBarrier(next http.Handler) *dualProfileBarrier {
	return &dualProfileBarrier{next: next, ready: make(chan struct{}), seen: make(map[string]bool)}
}

func (barrier *dualProfileBarrier) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	version := request.Header.Get("MCP-Protocol-Version")
	barrier.mu.Lock()
	barrier.seen[version] = true
	if barrier.seen[LegacyProtocolVersion] && barrier.seen[ModernProtocolVersion] {
		barrier.once.Do(func() { close(barrier.ready) })
	}
	barrier.mu.Unlock()

	select {
	case <-barrier.ready:
		barrier.next.ServeHTTP(w, request)
	case <-request.Context().Done():
		http.Error(w, "dual-profile rendezvous canceled", http.StatusRequestTimeout)
	}
}

func (barrier *dualProfileBarrier) sawBoth() bool {
	barrier.mu.Lock()
	defer barrier.mu.Unlock()
	return barrier.seen[LegacyProtocolVersion] && barrier.seen[ModernProtocolVersion]
}
