package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"neuralmail/internal/config"
	"neuralmail/internal/mcp"
	"neuralmail/internal/startup"
	"neuralmail/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		return
	}
	cmd := os.Args[1]
	cfg, err := config.Load(config.ConfigPathFromEnv())
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	if handled, err := startup.SchemaQuiescence(context.Background(), os.Getenv("NERVE_SCHEMA_TRANSITION_MODE"), cfg.Cloud.Mode, "administrative-command", ""); handled {
		if err != nil {
			log.Fatalf("schema quiescence: %v", err)
		}
		return
	}

	switch cmd {
	case "up":
		runCompose("up", "-d")
	case "down":
		runCompose("down")
	case "migrate-core":
		runMigrations(cfg, migrateCoreToRuntimeWindow)
	case "migrate-cloud":
		runMigrations(cfg, migrateCloudToRuntimeWindow)
	case "migrate-all":
		runMigrations(cfg, migrateAllToRuntimeWindow)
	case "seed":
		if err := seed(cfg, "/tmp/nerve-seed.done"); err != nil {
			log.Fatalf("seed failed: %v", err)
		}
	case "doctor":
		doctor(cfg)
	case "send-test":
		if err := sendTest(cfg); err != nil {
			log.Fatalf("send-test failed: %v", err)
		}
	case "mcp-test":
		if err := mcpTest(cfg, os.Stdout); err != nil {
			log.Fatalf("mcp-test failed: %v", err)
		}
	default:
		usage()
	}
}

func migrateCoreToRuntimeWindow(ctx context.Context, db *sql.DB) error {
	return store.MigrateUpToCore(ctx, db, startup.CoreMaxSupported)
}

func migrateCloudToRuntimeWindow(ctx context.Context, db *sql.DB) error {
	return store.MigrateUpToCloud(ctx, db, startup.RuntimeCloudMaxSupported)
}

func migrateAllToRuntimeWindow(ctx context.Context, db *sql.DB) error {
	if err := migrateCoreToRuntimeWindow(ctx, db); err != nil {
		return err
	}
	return migrateCloudToRuntimeWindow(ctx, db)
}

func runCompose(args ...string) {
	cmd := exec.Command("docker", append([]string{"compose"}, args...)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Fatalf("docker compose failed: %v", err)
	}
}

func runMigrations(cfg config.Config, migrate func(context.Context, *sql.DB) error) {
	db, err := sql.Open("pgx", cfg.Database.DSN)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()
	if err := migrate(context.Background(), db); err != nil {
		log.Fatalf("migration failed: %v", err)
	}
	fmt.Println("migrations complete")
}

func seed(cfg config.Config, seedFlag string) error {
	if _, err := os.Stat(seedFlag); err == nil {
		fmt.Printf("seed already applied; delete %s to re-run\n", seedFlag)
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect seed marker: %w", err)
	}
	messages := []struct {
		Subject string
		Body    string
	}{
		{"Critical server outage", "Our production system is down. Please fix ASAP."},
		{"Angry refund request", "I want a refund now. This is unacceptable."},
		{"Invoice request", "Please send our latest invoice for February."},
		{"Spam offer", "You won a prize, click here."},
		{"General question", "Can you help me change my plan?"},
	}
	for _, msg := range messages {
		if err := sendSMTP(cfg, msg.Subject, msg.Body); err != nil {
			return fmt.Errorf("send demo email: %w", err)
		}
	}
	if err := os.WriteFile(seedFlag, []byte(time.Now().Format(time.RFC3339)), 0o644); err != nil {
		return fmt.Errorf("write seed marker: %w", err)
	}
	fmt.Println("seeded demo emails")
	return nil
}

func doctor(cfg config.Config) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	checks := []struct {
		Name string
		Fn   func() error
	}{
		{"smtp", func() error { return pingSMTP(cfg) }},
		{"database", func() error { return pingDatabase(ctx, cfg.Database.DSN) }},
		{"redis", func() error { return pingTCP(cfg.Redis.URL) }},
		{"qdrant", func() error { return pingHTTP(cfg.Qdrant.URL) }},
		{"jmap", func() error { return pingJMAP(cfg) }},
		{"mcp", func() error { return pingHTTP(fmt.Sprintf("%s/healthz", localHTTPBase(cfg))) }},
	}
	for _, check := range checks {
		if err := check.Fn(); err != nil {
			fmt.Printf("%s: FAIL (%v)\n", check.Name, err)
			continue
		}
		fmt.Printf("%s: OK\n", check.Name)
	}
	_ = ctx
}

func sendTest(cfg config.Config) error {
	if err := sendSMTP(cfg, "Nerve test", "This is a test email from nerve CLI."); err != nil {
		return err
	}
	fmt.Println("sent test email")
	return nil
}

// mcpTest exercises the frozen legacy adapter, including its session contract.
// The modern stateless adapter has a separate conformance suite.
func mcpTest(cfg config.Config, output io.Writer) error {
	endpoint := fmt.Sprintf("%s/mcp", localHTTPBase(cfg))
	session := ""
	call := func(payload map[string]any) (mcpResponse, error) {
		raw, nextSession, err := callMCP(endpoint, payload, session, cfg.Security.APIKey)
		if err != nil {
			return mcpResponse{}, err
		}
		session = nextSession
		parsed, err := parseMCPResponse(raw)
		if err != nil {
			return parsed, err
		}
		expectedID, err := json.Marshal(payload["id"])
		if err != nil {
			return parsed, err
		}
		actualID, err := json.Marshal(parsed.ID)
		if err != nil || !bytes.Equal(expectedID, actualID) {
			return parsed, errors.New("MCP response ID does not match request")
		}
		return parsed, nil
	}
	initReq := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
		"protocolVersion": mcp.LegacyProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "neuralmail-cli", "version": "0.1.0"},
	}}
	initialized, err := call(initReq)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	var negotiated struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(initialized.Result, &negotiated); err != nil {
		return fmt.Errorf("initialize result: %w", err)
	}
	if negotiated.ProtocolVersion != mcp.LegacyProtocolVersion || session == "" {
		return errors.New("initialize: missing session or unexpected protocol version")
	}
	fmt.Fprintf(output, "initialize: %s\n", initialized.Result)

	listed, err := call(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": map[string]any{}})
	if err != nil {
		return fmt.Errorf("tools/list: %w", err)
	}
	fmt.Fprintf(output, "tools/list: %s\n", listed.Result)

	inboxes, err := call(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "resources/read", "params": map[string]any{"uri": "email://inboxes"}})
	if err != nil {
		return fmt.Errorf("resources/read inboxes: %w", err)
	}
	inboxID, err := firstInboxID(inboxes.Result)
	if err != nil {
		return fmt.Errorf("no inbox available for list_threads: %w", err)
	}

	threads, err := call(map[string]any{"jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": map[string]any{
		"name": "list_threads", "arguments": map[string]any{"inbox_id": inboxID, "limit": 10},
	}})
	if err != nil {
		return fmt.Errorf("tools/call list_threads: %w", err)
	}
	fmt.Fprintf(output, "tools/call list_threads: %s\n", threads.Result)
	return nil
}

func callMCP(endpoint string, payload map[string]any, session, apiKey string) (string, string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", session, fmt.Errorf("encode MCP request: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", session, fmt.Errorf("create MCP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("MCP-Protocol-Version", mcp.LegacyProtocolVersion)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	if session != "" {
		req.Header.Set("MCP-Session-Id", session)
	}
	client := &http.Client{
		Timeout: 30 * time.Second,
		// A smoke test must fail on a redirect, not follow it with credentials.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", session, fmt.Errorf("MCP request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", session, fmt.Errorf("MCP HTTP status %d", resp.StatusCode)
	}
	const maxResponseBytes = 16 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return "", session, fmt.Errorf("read MCP response: %w", err)
	}
	if len(data) > maxResponseBytes {
		return "", session, errors.New("MCP response too large")
	}
	if next := resp.Header.Get("MCP-Session-Id"); next != "" {
		session = next
	}
	return string(data), session, nil
}

func sendSMTP(cfg config.Config, subject, body string) error {
	host := cfg.SMTP.Host
	if host == "" {
		host = "localhost"
	}
	addr := net.JoinHostPort(host, strconv.Itoa(cfg.SMTP.Port))
	from := cfg.SMTP.From
	if from == "" {
		from = "dev@local.nerve.email"
	}
	to := "dev@local.nerve.email"
	msg := strings.Join([]string{
		"From: " + from,
		"To: " + to,
		"Subject: " + subject,
		"",
		body,
	}, "\r\n")

	helo := smtpHeloDomain(from)
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("smtp send: %w", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return fmt.Errorf("smtp deadline: %w", err)
	}
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return fmt.Errorf("smtp send: %w", err)
	}
	defer client.Close()
	if err := client.Hello(helo); err != nil {
		return fmt.Errorf("smtp send: %w", err)
	}
	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("smtp STARTTLS: %w", err)
		}
	} else if cfg.SMTP.RequireStartTLS {
		return errors.New("smtp STARTTLS is required but unavailable")
	}
	if cfg.SMTP.Username != "" || cfg.SMTP.Password != "" {
		if !supportsAuth(client) {
			return errors.New("smtp AUTH is configured but unavailable")
		}
		auth := smtp.PlainAuth("", cfg.SMTP.Username, cfg.SMTP.Password, host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("smtp send: %w", err)
		}
	}
	if err := client.Mail(from); err != nil {
		return fmt.Errorf("smtp send: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("smtp send: %w", err)
	}
	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp send: %w", err)
	}
	if _, err := writer.Write([]byte(msg)); err != nil {
		_ = writer.Close()
		return fmt.Errorf("smtp send: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("smtp send: %w", err)
	}
	// DATA close already confirmed acceptance; a QUIT failure must not cause a resend.
	_ = client.Quit()
	return nil
}

func smtpHeloDomain(addr string) string {
	parts := strings.Split(addr, "@")
	if len(parts) == 2 && parts[1] != "" {
		return parts[1]
	}
	return "local.nerve.email"
}

func supportsAuth(client *smtp.Client) bool {
	ok, _ := client.Extension("AUTH")
	return ok
}

func localHTTPBase(cfg config.Config) string {
	addr := cfg.HTTP.Addr
	if addr == "" {
		addr = ":8088"
	}
	host := "127.0.0.1"
	port := ""
	if strings.HasPrefix(addr, ":") {
		port = strings.TrimPrefix(addr, ":")
	} else if strings.Contains(addr, ":") {
		parts := strings.Split(addr, ":")
		if parts[0] != "" {
			host = parts[0]
		}
		if len(parts) > 1 {
			port = parts[len(parts)-1]
		}
	} else {
		port = addr
	}
	if port == "" {
		port = "8088"
	}
	return fmt.Sprintf("http://%s:%s", host, port)
}

func pingSMTP(cfg config.Config) error {
	host := cfg.SMTP.Host
	if host == "" {
		host = "localhost"
	}
	addr := net.JoinHostPort(host, strconv.Itoa(cfg.SMTP.Port))
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return err
	}
	return conn.Close()
}

func pingJMAP(cfg config.Config) error {
	sessionURL := cfg.JMAP.SessionURL
	if sessionURL == "" {
		parsed, err := url.Parse(cfg.JMAP.URL)
		if err != nil {
			return err
		}
		sessionURL = fmt.Sprintf("%s://%s/.well-known/jmap", parsed.Scheme, parsed.Host)
	}
	req, err := http.NewRequest(http.MethodGet, sessionURL, nil)
	if err != nil {
		return err
	}
	if cfg.JMAP.Username != "" || cfg.JMAP.Password != "" {
		req.SetBasicAuth(cfg.JMAP.Username, cfg.JMAP.Password)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func pingHTTP(url string) error {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func pingDatabase(ctx context.Context, dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	return db.PingContext(ctx)
}

func pingTCP(rawURL string) error {
	if rawURL == "" {
		return fmt.Errorf("missing url")
	}
	host := rawURL
	if strings.Contains(rawURL, "://") {
		parts := strings.Split(rawURL, "://")
		host = parts[len(parts)-1]
	}
	if strings.Contains(host, "/") {
		host = strings.Split(host, "/")[0]
	}
	if !strings.Contains(host, ":") {
		host += ":6379"
	}
	conn, err := net.DialTimeout("tcp", host, 2*time.Second)
	if err != nil {
		return err
	}
	return conn.Close()
}

func usage() {
	fmt.Println("Usage: neuralmail <up|down|migrate-core|migrate-cloud|migrate-all|seed|doctor|send-test|mcp-test>")
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func parseMCPResponse(raw string) (mcpResponse, error) {
	var parsed mcpResponse
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return parsed, err
	}
	if parsed.JSONRPC != "2.0" {
		return parsed, errors.New("invalid MCP response version")
	}
	if parsed.Error != nil {
		return parsed, fmt.Errorf("rpc error %d: %s", parsed.Error.Code, parsed.Error.Message)
	}
	if len(parsed.Result) == 0 || bytes.Equal(bytes.TrimSpace(parsed.Result), []byte("null")) {
		return parsed, errors.New("missing MCP result")
	}
	var toolResult struct {
		IsError bool `json:"isError"`
	}
	if json.Unmarshal(parsed.Result, &toolResult) == nil && toolResult.IsError {
		return parsed, errors.New("MCP tool returned isError")
	}
	return parsed, nil
}

func firstInboxID(result json.RawMessage) (string, error) {
	var payload struct {
		InboxIDs []string `json:"inbox_ids"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		return "", err
	}
	if len(payload.InboxIDs) == 0 || strings.TrimSpace(payload.InboxIDs[0]) == "" {
		return "", errors.New("empty inbox_ids")
	}
	return payload.InboxIDs[0], nil
}
