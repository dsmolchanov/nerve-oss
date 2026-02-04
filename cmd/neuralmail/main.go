package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/smtp"
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
		{"jmap", func() error { return pingHTTP(cfg.JMAP.URL) }},
		{"mcp", func() error { return pingHTTP("http://localhost:8088/healthz") }},
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
	url := "http://localhost:8088/mcp"
	initReq := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{}}
	resp, session := callMCP(url, initReq, "")
	fmt.Printf("initialize: %s\n", resp)
	listReq := map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": map[string]any{}}
	resp, _ = callMCP(url, listReq, session)
	fmt.Printf("tools/list: %s\n", resp)
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

	auth := smtp.PlainAuth("", cfg.SMTP.Username, cfg.SMTP.Password, host)
	if err := smtp.SendMail(addr, auth, from, []string{to}, []byte(msg)); err != nil {
		log.Printf("smtp send failed: %v", err)
	}
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
