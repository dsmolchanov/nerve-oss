package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"neuralmail/internal/config"
	"neuralmail/internal/mcp"
)

func TestMCPTestLegacyRouterSession(t *testing.T) {
	cfg := config.Config{}
	cfg.Dev.Mode = true
	cfg.Security.APIKey = "test-key"
	server := mcp.NewServer(cfg, nil, nil, nil)
	var methods []string
	adapter := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("missing configured bearer")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var request struct {
			ID     int             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Error(err)
			return
		}
		methods = append(methods, request.Method)
		switch request.Method {
		case "initialize", "tools/list":
			// Actual router + legacy adapter verify the version and initialize session.
			server.HandleRoutedHTTP(w, r)
		default:
			if r.Header.Get("MCP-Session-Id") == "" {
				t.Error("session lost after tools/list")
			}
			result := map[string]any{"inbox_ids": []string{"inbox-1"}}
			if request.Method == "tools/call" {
				var params struct {
					Name      string `json:"name"`
					Arguments struct {
						InboxID string `json:"inbox_id"`
					} `json:"arguments"`
				}
				if err := json.Unmarshal(request.Params, &params); err != nil {
					t.Error(err)
					return
				}
				if params.Name != "list_threads" || params.Arguments.InboxID != "inbox-1" {
					t.Errorf("unexpected tool params: %s", request.Params)
				}
				result = map[string]any{"threads": []any{}}
			}
			// Like the actual legacy adapter, subsequent responses omit the session header.
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
		}
	})
	httpServer := httptest.NewServer(mcp.NewRouter(cfg, nil, adapter, nil))
	defer httpServer.Close()
	cfg.HTTP.Addr = strings.TrimPrefix(httpServer.URL, "http://")
	var output bytes.Buffer
	if err := mcpTest(cfg, &output); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(methods, ","); got != "initialize,tools/list,resources/read,tools/call" {
		t.Fatal(got)
	}
	if !strings.Contains(output.String(), "tools/call list_threads:") {
		t.Fatal(output.String())
	}
}

func TestMCPTestRejectsFailureResponses(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"http", `{"jsonrpc":"2.0","id":1,"result":{}}`, 401},
		{"rpc", `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"failed"}}`, 200},
		{"tool", `{"jsonrpc":"2.0","id":1,"result":{"isError":true}}`, 200},
		{"malformed", `not json`, 200},
		{"empty", `{}`, 200},
		{"missing_result", `{"jsonrpc":"2.0","id":1}`, 200},
		{"wrong_id", `{"jsonrpc":"2.0","id":9,"result":{"protocolVersion":"2025-11-25"}}`, 200},
		{"wrong_version", `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2026-07-28"}}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("MCP-Session-Id", "session")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			cfg := config.Config{}
			cfg.HTTP.Addr = strings.TrimPrefix(server.URL, "http://")
			if err := mcpTest(cfg, io.Discard); err == nil {
				t.Fatal("false successful smoke")
			}
		})
	}
}

func TestMCPRequestRejectsRedirect(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	if _, _, err := callMCP(server.URL, map[string]any{"id": 1}, "session", "test-key"); err == nil {
		t.Fatal("redirect accepted")
	}
	if targetCalls.Load() != 0 {
		t.Fatal("redirect followed")
	}
}

// smtpFixture accepts sequential SMTP sessions and can reject a selected phase.
// It sends only synthetic local test messages and does not contact a real relay.
func smtpFixture(t *testing.T, reject string) (config.Config, *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				reply := func(s string) { _, _ = io.WriteString(conn, s+"\r\n") }
				if reject == "greeting" {
					reply("421 unavailable")
					return
				}
				reply("220 localhost ESMTP")
				scan := bufio.NewScanner(conn)
				inData := false
				for scan.Scan() {
					line := scan.Text()
					if inData {
						if line != "." {
							continue
						}
						inData = false
						if reject == "accept" || (reject == "accept-after-two" && accepted.Load() == 2) {
							reply("554 message rejected")
						} else {
							accepted.Add(1)
							reply("250 accepted")
						}
						continue
					}
					verb := strings.Fields(line)[0]
					switch verb {
					case "EHLO", "HELO":
						if reject == "hello" {
							reply("550 rejected")
						} else {
							reply("250 localhost")
						}
					case "MAIL", "RCPT":
						if reject == verb {
							reply("550 rejected")
						} else {
							reply("250 OK")
						}
					case "DATA":
						if reject == "DATA" {
							reply("554 rejected")
						} else {
							reply("354 send message")
							inData = true
						}
					case "QUIT":
						if reject == "quit" {
							return
						}
						reply("221 bye")
						return
					default:
						reply("500 unsupported")
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); <-done })
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{}
	cfg.SMTP.Host = host
	cfg.SMTP.Port, _ = strconv.Atoi(port)
	cfg.SMTP.From = "dev@local.nerve.email"
	return cfg, &accepted
}

func TestSeedMarkerRequiresEverySMTPAcceptance(t *testing.T) {
	for _, phase := range []string{"greeting", "hello", "MAIL", "RCPT", "DATA", "accept"} {
		t.Run(phase, func(t *testing.T) {
			cfg, accepted := smtpFixture(t, phase)
			marker := filepath.Join(t.TempDir(), "seed.done")
			if err := seed(cfg, marker); err == nil {
				t.Fatal("seed concealed SMTP error")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("marker exists after failure: %v", err)
			}
			if accepted.Load() != 0 {
				t.Fatal("unexpected SMTP acceptance")
			}
			if err := sendTest(cfg); err == nil {
				t.Fatal("send-test concealed SMTP error")
			}
		})
	}
}

func TestSeedSuccessAndMarkerWriteFailure(t *testing.T) {
	cfg, accepted := smtpFixture(t, "")
	marker := filepath.Join(t.TempDir(), "seed.done")
	if err := seed(cfg, marker); err != nil {
		t.Fatal(err)
	}
	if accepted.Load() != 5 {
		t.Fatal(accepted.Load())
	}
	if err := seed(cfg, marker); err != nil {
		t.Fatal(err)
	}
	if accepted.Load() != 5 {
		t.Fatal("already seeded sent again")
	}
	badMarker := filepath.Join(t.TempDir(), "missing", "seed.done")
	if err := seed(cfg, badMarker); err == nil || !strings.Contains(err.Error(), "write seed marker") {
		t.Fatalf("marker write failure hidden: %v", err)
	}
}

func TestSMTPAcceptanceSurvivesQuitFailure(t *testing.T) {
	cfg, accepted := smtpFixture(t, "quit")
	if err := sendTest(cfg); err != nil {
		t.Fatal(err)
	}
	if accepted.Load() != 1 {
		t.Fatal("message not accepted")
	}
}

func TestSMTPRequiresConfiguredSecurity(t *testing.T) {
	for _, setting := range []string{"tls", "auth"} {
		t.Run(setting, func(t *testing.T) {
			cfg, accepted := smtpFixture(t, "")
			if setting == "tls" {
				cfg.SMTP.RequireStartTLS = true
			} else {
				cfg.SMTP.Username = "test-user"
			}
			if err := sendTest(cfg); err == nil {
				t.Fatal("silently ignored security requirement")
			}
			if accepted.Load() != 0 {
				t.Fatal("message sent before security requirement")
			}
		})
	}
}

func TestSendCommandsRejectUnavailableSMTP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	host, port, _ := net.SplitHostPort(addr)
	cfg := config.Config{}
	cfg.SMTP.Host = host
	cfg.SMTP.Port, _ = strconv.Atoi(port)
	marker := filepath.Join(t.TempDir(), "seed.done")
	if err := seed(cfg, marker); err == nil {
		t.Fatal("seed succeeded with unavailable relay")
	}
	if err := sendTest(cfg); err == nil {
		t.Fatal("send-test succeeded with unavailable relay")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("unexpected marker: %v", err)
	}
}

func TestPartialSeedDoesNotWriteSuccessMarker(t *testing.T) {
	cfg, accepted := smtpFixture(t, "accept-after-two")
	marker := filepath.Join(t.TempDir(), "seed.done")
	if err := seed(cfg, marker); err == nil {
		t.Fatal("partial seed reported success")
	}
	if accepted.Load() != 2 {
		t.Fatalf("accepted %d messages, want 2", accepted.Load())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("partial seed marker: %v", err)
	}
}
