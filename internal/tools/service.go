package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"os"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"neuralmail/internal/config"
	"neuralmail/internal/embed"
	"neuralmail/internal/llm"
	"neuralmail/internal/observability"
	"neuralmail/internal/policy"
	"neuralmail/internal/store"
	"neuralmail/internal/vector"
)

type Service struct {
	Config   config.Config
	Store    *store.Store
	LLM      llm.Provider
	Vector   vector.Store
	Policy   policy.Policy
	Embedder embed.Provider
}

type ToolContext struct {
	Actor    string
	ReplayID string
}

func NewService(cfg config.Config, store *store.Store, llmProvider llm.Provider, vectorStore vector.Store, policyObj policy.Policy, embedder embed.Provider) *Service {
	return &Service{Config: cfg, Store: store, LLM: llmProvider, Vector: vectorStore, Policy: policyObj, Embedder: embedder}
}

func (s *Service) ListThreads(ctx context.Context, inboxID string, status string, limit int) (any, error) {
	threads, err := s.Store.ListThreads(ctx, inboxID, status, limit)
	if err != nil {
		return nil, err
	}
	return map[string]any{"threads": threads}, nil
}

func (s *Service) GetThread(ctx context.Context, threadID string) (any, error) {
	thread, messages, err := s.Store.GetThread(ctx, threadID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"thread": thread, "messages": messages}, nil
}

func (s *Service) SearchInbox(ctx context.Context, inboxID string, query string, topK int) (any, error) {
	if s.Vector != nil && s.Embedder != nil {
		return s.searchVector(ctx, inboxID, query, topK)
	}
	results, err := s.Store.SearchInboxFTS(ctx, inboxID, query, topK)
	if err != nil {
		return nil, err
	}
	return map[string]any{"results": results}, nil
}

func (s *Service) searchVector(ctx context.Context, inboxID, query string, topK int) (any, error) {
	if s.Embedder == nil {
		return nil, errors.New("embedding provider not configured")
	}
	vectors, err := s.Embedder.Embed(ctx, []string{query})
	if err != nil || len(vectors) == 0 {
		return nil, err
	}
	filter := map[string]any{
		"must": []map[string]any{{
			"key":   "inbox_id",
			"match": map[string]any{"value": inboxID},
		}},
	}
	hits, err := s.Vector.Search(ctx, vectors[0], topK, filter)
	if err != nil {
		return nil, err
	}
	results := make([]map[string]any, 0, len(hits))
	for _, hit := range hits {
		results = append(results, map[string]any{
			"message_id": hit.Payload["message_id"],
			"thread_id":  hit.Payload["thread_id"],
			"score":      hit.Score,
			"snippet":    hit.Payload["snippet"],
		})
	}
	return map[string]any{"results": results}, nil
}

func (s *Service) TriageMessage(ctx context.Context, messageID string) (any, error) {
	msg, err := s.Store.GetMessage(ctx, messageID)
	if err != nil {
		return nil, err
	}
	classification, err := s.LLM.Classify(ctx, msg.Text, nil)
	if err != nil {
		return nil, err
	}
	_ = s.Store.UpdateThreadSignals(ctx, msg.ThreadID, ptrFloat(classificationConfidenceToSentiment(classification.Sentiment)), classification.Urgency)
	return map[string]any{
		"intent":          classification.Intent,
		"urgency":         classification.Urgency,
		"sentiment":       classification.Sentiment,
		"confidence":      classification.Confidence,
		"suggested_route": "support",
	}, nil
}

func (s *Service) ExtractToSchema(ctx context.Context, messageID string, schemaID string) (any, error) {
	msg, err := s.Store.GetMessage(ctx, messageID)
	if err != nil {
		return nil, err
	}
	schema, err := LoadSchema(schemaID)
	if err != nil {
		return nil, err
	}
	result, err := s.LLM.Extract(ctx, msg.Text, schema, nil)
	if err != nil {
		return nil, err
	}
	validated, validationErrors := validateJSON(schema, result.Data)
	if !validated {
		result.ValidationErrors = validationErrors
		// One repair attempt
		repair, err := s.LLM.Extract(ctx, msg.Text, schema, nil)
		if err == nil {
			result = repair
			validated, validationErrors = validateJSON(schema, result.Data)
			if !validated {
				result.ValidationErrors = validationErrors
				result.Confidence = 0
			}
		}
	}
	return map[string]any{
		"data":              result.Data,
		"confidence":        result.Confidence,
		"missing_fields":    result.MissingFields,
		"validation_errors": result.ValidationErrors,
	}, nil
}

func (s *Service) DraftReply(ctx context.Context, threadID string, goal string) (any, error) {
	thread, messages, err := s.Store.GetThread(ctx, threadID)
	if err != nil {
		return nil, err
	}
	contextText := buildThreadContext(thread, messages)
	draft, err := s.LLM.Draft(ctx, contextText, nil, goal)
	if err != nil {
		return nil, err
	}
	adjusted, eval := policy.Evaluate(draft.Text, s.Policy)
	if !eval.Allowed && eval.ViolationLevel == "critical" {
		return map[string]any{
			"draft":                "",
			"risk_flags":           eval.RiskFlags,
			"cited_message_ids":    nil,
			"needs_human_approval": true,
			"policy_blocked":       true,
			"reason":               eval.Reason,
		}, nil
	}
	return map[string]any{
		"draft":                adjusted,
		"risk_flags":           eval.RiskFlags,
		"cited_message_ids":    []string{lastMessageID(messages)},
		"needs_human_approval": eval.NeedsApproval || draft.NeedsApproval,
	}, nil
}

func (s *Service) SendReply(ctx context.Context, threadID string, body string, needsApproval bool) (any, error) {
	if needsApproval && !s.Config.Security.AllowSendWithWarnings {
		return nil, errors.New("send blocked: needs human approval")
	}
	thread, messages, err := s.Store.GetThread(ctx, threadID)
	if err != nil {
		return nil, err
	}
	inboxID, _ := s.Store.GetThreadInboxID(ctx, threadID)
	if len(messages) == 0 {
		return nil, errors.New("no messages in thread")
	}
	from := s.Config.SMTP.From
	if from == "" {
		from = "dev@local.neuralmail"
	}
	to := messages[len(messages)-1].From.Email
	if to == "" {
		return nil, errors.New("missing recipient")
	}
	if !s.Config.Security.AllowOutbound && !strings.HasSuffix(to, "@local.neuralmail") {
		return nil, errors.New("outbound disabled for non-local domains")
	}
	if len(s.Config.Security.OutboundDomainAllowlist) > 0 && !domainAllowed(to, s.Config.Security.OutboundDomainAllowlist) {
		return nil, errors.New("recipient domain not allowlisted")
	}
	subject := "Re: " + thread.Subject
	if subject == "Re: " {
		subject = "Reply"
	}
	msg := store.Message{
		InboxID:   inboxID,
		Direction: "outbound",
		Subject:   subject,
		Text:      body,
		CreatedAt: time.Now().UTC(),
		From:      store.Participant{Email: from},
		To:        []store.Participant{{Email: to}},
	}
	msg.ThreadID = thread.ID
	msgID, err := s.Store.InsertMessage(ctx, msg)
	if err != nil {
		return nil, err
	}
	if err := s.sendSMTP(from, to, subject, body); err != nil {
		return nil, err
	}
	return map[string]any{"message_id": msgID, "status": "queued"}, nil
}

func domainAllowed(addr string, allowlist []string) bool {
	parts := strings.Split(addr, "@")
	if len(parts) != 2 {
		return false
	}
	domain := strings.ToLower(parts[1])
	for _, allowed := range allowlist {
		if strings.ToLower(strings.TrimSpace(allowed)) == domain {
			return true
		}
	}
	return false
}

func (s *Service) sendSMTP(from, to, subject, body string) error {
	host := s.Config.SMTP.Host
	if host == "" {
		host = "localhost"
	}
	addr := fmt.Sprintf("%s:%d", host, s.Config.SMTP.Port)
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
		return err
	}
	defer conn.Close()
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer client.Quit()
	if err := client.Hello(helo); err != nil {
		return err
	}
	if (s.Config.SMTP.Username != "" || s.Config.SMTP.Password != "") && supportsAuth(client) {
		auth := smtp.PlainAuth("", s.Config.SMTP.Username, s.Config.SMTP.Password, host)
		if err := client.Auth(auth); err != nil {
			return err
		}
	}
	if err := client.Mail(from); err != nil {
		return err
	}
	if err := client.Rcpt(to); err != nil {
		return err
	}
	writer, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := writer.Write([]byte(msg)); err != nil {
		_ = writer.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return client.Quit()
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

func LoadSchema(schemaID string) (map[string]any, error) {
	if schemaID == "" {
		return nil, errors.New("missing schema id")
	}
	path := fmt.Sprintf("configs/schemas/%s.json", schemaID)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		return nil, err
	}
	return schema, nil
}

func validateJSON(schema map[string]any, data map[string]any) (bool, []string) {
	if schema == nil {
		return true, nil
	}
	schemaBytes, _ := json.Marshal(schema)
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("schema.json", bytes.NewReader(schemaBytes)); err != nil {
		return false, []string{err.Error()}
	}
	compiled, err := compiler.Compile("schema.json")
	if err != nil {
		return false, []string{err.Error()}
	}
	if err := compiled.Validate(data); err != nil {
		return false, []string{err.Error()}
	}
	return true, nil
}

func buildThreadContext(thread store.Thread, messages []store.Message) string {
	contextText := fmt.Sprintf("Thread: %s\n", thread.Subject)
	for _, msg := range messages {
		contextText += fmt.Sprintf("[%s] %s\n", msg.Direction, msg.Text)
	}
	return contextText
}

func lastMessageID(messages []store.Message) string {
	if len(messages) == 0 {
		return ""
	}
	return messages[len(messages)-1].ID
}

func classificationConfidenceToSentiment(sentiment string) float64 {
	switch sentiment {
	case "negative":
		return -0.5
	case "positive":
		return 0.5
	default:
		return 0
	}
}

func ptrFloat(v float64) *float64 {
	return &v
}

func ReplayID() string {
	return observability.NewReplayID()
}
