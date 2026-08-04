package mcp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"neuralmail/internal/config"
	"neuralmail/internal/memguard"
)

func TestHandleHTTPChargesActualBodyWhenContentLengthIsUnderstated(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	server := NewServer(cfg, nil, nil, nil)
	budget, err := memguard.New(8)
	if err != nil {
		t.Fatalf("new budget: %v", err)
	}
	server.MemoryBudget = budget

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize"}`
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	request.ContentLength = 1
	recorder := httptest.NewRecorder()
	server.HandleHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Retry-After") != "1" {
		t.Fatalf("expected Retry-After header, got %q", recorder.Header().Get("Retry-After"))
	}
	if budget.Used() != 0 {
		t.Fatalf("failed request leaked %d bytes", budget.Used())
	}
}

func TestHandleHTTPRejectsBodyOverWireLimit(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	server := NewServer(cfg, nil, nil, nil)
	body := `{"jsonrpc":"` + strings.Repeat("a", int(maxMCPBodyBytes)) + `"}`
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	server.HandleHTTP(recorder, request)

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d body=%s", recorder.Code, recorder.Body.String())
	}
	if server.MemoryBudget.Used() != 0 {
		t.Fatalf("oversized request leaked %d bytes", server.MemoryBudget.Used())
	}
}

func TestHandleHTTPReleasesBudgetAfterSuccessAndDecodeError(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.HTTP.ReadTimeout = time.Second
	server := NewServer(cfg, nil, nil, nil)
	budget, err := memguard.New(1 << 20)
	if err != nil {
		t.Fatalf("new budget: %v", err)
	}
	server.MemoryBudget = budget

	for _, test := range []struct {
		name string
		body string
		want int
	}{
		{name: "success", body: `{"jsonrpc":"2.0","id":1,"method":"initialize"}`, want: http.StatusOK},
		{name: "decode error", body: `{"jsonrpc":`, want: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(test.body))
			recorder := httptest.NewRecorder()
			server.HandleHTTP(recorder, request)
			if recorder.Code != test.want {
				t.Fatalf("expected %d, got %d body=%s", test.want, recorder.Code, recorder.Body.String())
			}
			if budget.Used() != 0 {
				t.Fatalf("request leaked %d bytes", budget.Used())
			}
		})
	}
}

func TestHandleHTTPReadTimeoutReleasesSlowBodyReservation(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.HTTP.ReadTimeout = 50 * time.Millisecond
	mcpServer := NewServer(cfg, nil, nil, nil)
	budget, err := memguard.New(1 << 20)
	if err != nil {
		t.Fatalf("new budget: %v", err)
	}
	mcpServer.MemoryBudget = budget

	httpServer := httptest.NewUnstartedServer(http.HandlerFunc(mcpServer.HandleHTTP))
	httpServer.Config.ReadTimeout = cfg.HTTP.ReadTimeout
	httpServer.Start()
	defer httpServer.Close()

	reader, writer := io.Pipe()
	request, err := http.NewRequest(http.MethodPost, httpServer.URL, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.ContentLength = 128

	result := make(chan error, 1)
	go func() {
		response, err := httpServer.Client().Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		result <- err
	}()

	if _, err := writer.Write([]byte(`{"jsonrpc":`)); err != nil {
		t.Fatalf("write initial body: %v", err)
	}
	select {
	case <-result:
	case <-time.After(2 * time.Second):
		t.Fatal("slow request outlived read timeout")
	}
	_ = writer.Close()
	if budget.Used() != 0 {
		t.Fatalf("timed-out request leaked %d bytes", budget.Used())
	}
}
