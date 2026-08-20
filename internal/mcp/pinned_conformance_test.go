//go:build conformance

package mcp

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"neuralmail/internal/config"
)

const pinnedMCPConformanceRevision = "81eb1c3edaed87d7fd585d7b80186da7a2960660"

const pinnedServerConformanceEval = `
import { pathToFileURL } from "node:url";
void (async () => {
  const runner = await import(pathToFileURL(process.env.MCP_CONFORMANCE_ENTRYPOINT).href);
  const result = await runner.runServerConformanceTest(
    process.env.NERVE_MCP_CONFORMANCE_URL,
    "tools-list",
    undefined,
    process.env.NERVE_MCP_CONFORMANCE_VERSION,
  );
  const summary = runner.printServerResults(result.checks, result.scenarioDescription);
  if (summary.failed > 0) process.exitCode = 1;
})().catch((error) => {
  console.error(error);
  process.exitCode = 1;
});
`

func TestPinnedMCP2026ConformanceAcrossHandlerInstances(t *testing.T) {
	conformanceCommand := os.Getenv("MCP_CONFORMANCE_COMMAND")
	if conformanceCommand == "" {
		t.Fatal("MCP_CONFORMANCE_COMMAND is required for the conformance build tag")
	}
	conformanceEntrypoint := os.Getenv("MCP_CONFORMANCE_ENTRYPOINT")
	if conformanceEntrypoint == "" {
		t.Fatal("MCP_CONFORMANCE_ENTRYPOINT is required for the conformance build tag")
	}
	if got := os.Getenv("MCP_CONFORMANCE_REVISION"); got != pinnedMCPConformanceRevision {
		t.Fatalf("MCP_CONFORMANCE_REVISION=%q want pinned %q", got, pinnedMCPConformanceRevision)
	}

	cfg := config.Default()
	var calls [2]atomic.Int32
	handlers := [2]http.Handler{
		NewSDKHandler(NewServer(cfg, nil, nil, nil), true),
		NewSDKHandler(NewServer(cfg, nil, nil, nil), true),
	}
	var next atomic.Uint32
	balanced := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := int(next.Add(1)-1) % len(handlers)
		calls[index].Add(1)
		handlers[index].ServeHTTP(w, r)
	})
	hosted := httptest.NewServer(balanced)
	defer hosted.Close()

	// One logical modern client crosses the instance boundary between its
	// server/discover probe and tools/list request. A stateful adapter would
	// lose its bootstrap state on the second request.
	client := newModernSDKTestClient()
	session, err := client.Connect(context.Background(), &sdkmcp.StreamableClientTransport{
		Endpoint: hosted.URL, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connect modern client across handler instances: %v", err)
	}
	if _, err := session.ListTools(context.Background(), nil); err != nil {
		_ = session.Close()
		t.Fatalf("list tools on second handler instance: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("close modern stateless client: %v", err)
	}
	for index := range handlers {
		if got := calls[index].Load(); got != 1 {
			t.Fatalf("cross-instance client handler %d received %d requests, want exactly one", index+1, got)
		}
		calls[index].Store(0)
	}
	next.Store(0)

	// The draft tools-list scenario is one stateless request. Run the exact
	// pinned official consumer twice through a round-robin boundary so each
	// request lands on a separately constructed handler and runtime. Neither
	// invocation can inherit process-local MCP session state from the other.
	for run := 1; run <= len(handlers); run++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		command := exec.CommandContext(ctx, conformanceCommand, "--eval", pinnedServerConformanceEval)
		command.Env = append(os.Environ(),
			"MCP_CONFORMANCE_ENTRYPOINT="+conformanceEntrypoint,
			"NERVE_MCP_CONFORMANCE_URL="+hosted.URL,
			"NERVE_MCP_CONFORMANCE_VERSION="+ModernProtocolVersion,
		)
		var output bytes.Buffer
		command.Stdout = &output
		command.Stderr = &output
		err := command.Run()
		cancel()
		if err != nil {
			t.Fatalf("pinned MCP conformance run %d failed: %v\n%s", run, err, output.String())
		}
		if !bytes.Contains(output.Bytes(), []byte("Passed: 3/3, 0 failed")) {
			t.Fatalf("pinned MCP conformance run %d did not report all checks passing:\n%s", run, output.String())
		}
	}

	for index := range handlers {
		if got := calls[index].Load(); got != 1 {
			t.Fatalf("handler instance %d received %d requests, want exactly one", index+1, got)
		}
	}
	if got := next.Load(); got != uint32(len(handlers)) {
		t.Fatalf("balanced request count=%d want=%d", got, len(handlers))
	}
}
