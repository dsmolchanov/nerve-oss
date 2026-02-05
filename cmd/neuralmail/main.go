package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"neuralmail/internal/config"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		return
	}
	cmd := os.Args[1]
	cfg, err := config.Load(os.Getenv("NM_CONFIG"))
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	switch cmd {
	case "up":
		runCompose("up", "-d")
	case "down":
		runCompose("down")
	case "seed":
		seed(cfg)
	case "doctor":
		doctor(cfg)
	case "send-test":
		sendTest(cfg)
	case "mcp-test":
		mcpTest(cfg)
	default:
		usage()
	}
}

func runCompose(args ...string) {
	cmd := exec.Command("docker", append([]string{"compose"}, args...)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Fatalf("docker compose failed: %v", err)
	}
}

func seed(cfg config.Config) {
	seedFlag := "/tmp/nerve-seed.done"
	if _, err := os.Stat(seedFlag); err == nil {
		fmt.Println("seed already applied; delete /tmp/nerve-seed.done to re-run")
		return
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
		sendSMTP(cfg, msg.Subject, msg.Body)
	}
	_ = os.WriteFile(seedFlag, []byte(time.Now().Format(time.RFC3339)), 0o644)
	fmt.Println("seeded demo emails")
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

func sendTest(cfg config.Config) {
	sendSMTP(cfg, "Nerve test", "This is a test email from neuralmail CLI.")
	fmt.Println("sent test email")
}

func mcpTest(cfg config.Config) {
	url := fmt.Sprintf("%s/mcp", localHTTPBase(cfg))
	initReq := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{}}
	resp, session := callMCP(url, initReq, "")
	_, err := parseMCPResponse(resp)
	if err != nil {
		log.Fatalf("initialize failed: %v", err)
	}
	fmt.Printf("initialize: %s\n", resp)

	listReq := map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": map[string]any{}}
	resp, _ = callMCP(url, listReq, session)
	if _, err := parseMCPResponse(resp); err != nil {
		log.Fatalf("tools/list failed: %v", err)
	}
	fmt.Printf("tools/list: %s\n", resp)

	inboxesReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "resources/read",
		"params": map[string]any{
			"uri": "email://inboxes",
		},
	}
	resp, _ = callMCP(url, inboxesReq, session)
	inboxesParsed, err := parseMCPResponse(resp)
	if err != nil {
		log.Fatalf("resources/read inboxes failed: %v", err)
	}
	inboxID, err := firstInboxID(inboxesParsed.Result)
	if err != nil {
		log.Fatalf("no inbox available for list_threads: %v", err)
	}

	callReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      4,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "list_threads",
			"arguments": map[string]any{
				"inbox_id": inboxID,
				"limit":    10,
			},
		},
	}
	resp, _ = callMCP(url, callReq, session)
	if _, err := parseMCPResponse(resp); err != nil {
		log.Fatalf("tools/call list_threads failed: %v", err)
	}
	fmt.Printf("tools/call list_threads: %s\n", resp)
}

func callMCP(url string, payload map[string]any, session string) (string, string) {
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if session != "" {
		req.Header.Set("MCP-Session-Id", session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err.Error(), session
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	return buf.String(), resp.Header.Get("MCP-Session-Id")
}

func sendSMTP(cfg config.Config, subject, body string) {
	host := cfg.SMTP.Host
	if host == "" {
		host = "localhost"
	}
	addr := fmt.Sprintf("%s:%d", host, cfg.SMTP.Port)
	from := cfg.SMTP.From
	if from == "" {
		from = "dev@local.neuralmail"
	}
	to := "dev@local.neuralmail"
	msg := strings.Join([]string{
		"From: " + from,
		"To: " + to,
		"Subject: " + subject,
		"",
		body,
	}, "\r\n")

	helo := smtpHeloDomain(from)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		log.Printf("smtp send failed: %v", err)
		return
	}
	defer conn.Close()
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		log.Printf("smtp send failed: %v", err)
		return
	}
	defer client.Quit()
	if err := client.Hello(helo); err != nil {
		log.Printf("smtp send failed: %v", err)
		return
	}
	if (cfg.SMTP.Username != "" || cfg.SMTP.Password != "") && supportsAuth(client) {
		auth := smtp.PlainAuth("", cfg.SMTP.Username, cfg.SMTP.Password, host)
		if err := client.Auth(auth); err != nil {
			log.Printf("smtp send failed: %v", err)
			return
		}
	}
	if err := client.Mail(from); err != nil {
		log.Printf("smtp send failed: %v", err)
		return
	}
	if err := client.Rcpt(to); err != nil {
		log.Printf("smtp send failed: %v", err)
		return
	}
	writer, err := client.Data()
	if err != nil {
		log.Printf("smtp send failed: %v", err)
		return
	}
	if _, err := writer.Write([]byte(msg)); err != nil {
		_ = writer.Close()
		log.Printf("smtp send failed: %v", err)
		return
	}
	if err := writer.Close(); err != nil {
		log.Printf("smtp send failed: %v", err)
		return
	}
	_ = client.Quit()
}

func smtpHeloDomain(addr string) string {
	parts := strings.Split(addr, "@")
	if len(parts) == 2 && parts[1] != "" {
		return parts[1]
	}
	return "local.neuralmail"
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
	addr := fmt.Sprintf("%s:%d", host, cfg.SMTP.Port)
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
	fmt.Println("Usage: neuralmail <up|down|seed|doctor|send-test|mcp-test>")
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
	if parsed.Error != nil {
		return parsed, fmt.Errorf("rpc error %d: %s", parsed.Error.Code, parsed.Error.Message)
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
	if len(payload.InboxIDs) == 0 {
		return "", errors.New("empty inbox_ids")
	}
	return payload.InboxIDs[0], nil
}
