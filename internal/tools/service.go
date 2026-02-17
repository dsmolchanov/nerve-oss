package tools

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"neuralmail/internal/auth"
	"neuralmail/internal/config"
	"neuralmail/internal/emailtransport"
	"neuralmail/internal/embed"
	"neuralmail/internal/entitlements"
	"neuralmail/internal/llm"
	"neuralmail/internal/observability"
	"neuralmail/internal/policy"
	"neuralmail/internal/store"
	"neuralmail/internal/vector"
)

type Service struct {
	Config    config.Config
	Store     *store.Store
	LLM       llm.Provider
	Vector    vector.Store
	Policy    policy.Policy
	Embedder  embed.Provider
	Transport *emailtransport.Registry
}

type ToolContext struct {
	Actor    string
	ReplayID string
}

func NewService(cfg config.Config, store *store.Store, llmProvider llm.Provider, vectorStore vector.Store, policyObj policy.Policy, embedder embed.Provider, transport *emailtransport.Registry) *Service {
	return &Service{Config: cfg, Store: store, LLM: llmProvider, Vector: vectorStore, Policy: policyObj, Embedder: embedder, Transport: transport}
}

func (s *Service) withScopedStore(ctx context.Context, fn func(scopedCtx context.Context, st *store.Store, principal auth.Principal) (any, error)) (any, error) {
	if !s.Config.Cloud.Mode {
		return fn(ctx, s.Store, auth.Principal{})
	}
	principal, ok := auth.PrincipalFromContext(ctx)
	if !ok {
		return nil, errors.New("missing cloud principal")
	}
	var out any
	err := s.Store.RunAsOrg(ctx, principal.OrgID, func(scoped *store.Store) error {
		result, callErr := fn(ctx, scoped, principal)
		out = result
		return callErr
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) ensureInboxBelongsToOrg(ctx context.Context, st *store.Store, orgID string, inboxID string) error {
	if err := st.EnsureInboxBelongsToOrg(ctx, inboxID, orgID); err != nil {
		if errors.Is(err, store.ErrOwnershipMismatch) {
			return errors.New("inbox does not belong to org")
		}
		return err
	}
	return nil
}

func (s *Service) ensureThreadBelongsToOrg(ctx context.Context, st *store.Store, orgID string, threadID string) error {
	if err := st.EnsureThreadBelongsToOrg(ctx, threadID, orgID); err != nil {
		if errors.Is(err, store.ErrOwnershipMismatch) {
			return errors.New("thread does not belong to org")
		}
		return err
	}
	return nil
}

func (s *Service) ensureMessageBelongsToOrg(ctx context.Context, st *store.Store, orgID string, messageID string) error {
	if err := st.EnsureMessageBelongsToOrg(ctx, messageID, orgID); err != nil {
		if errors.Is(err, store.ErrOwnershipMismatch) {
			return errors.New("message does not belong to org")
		}
		return err
	}
	return nil
}

func (s *Service) ListThreads(ctx context.Context, inboxID string, status string, limit int) (any, error) {
	return s.withScopedStore(ctx, func(scopedCtx context.Context, st *store.Store, principal auth.Principal) (any, error) {
		if principal.OrgID != "" {
			if err := s.ensureInboxBelongsToOrg(scopedCtx, st, principal.OrgID, inboxID); err != nil {
				return nil, err
			}
		}
		threads, err := st.ListThreads(scopedCtx, inboxID, status, limit)
		if err != nil {
			return nil, err
		}
		return map[string]any{"threads": threads}, nil
	})
}

func (s *Service) GetThread(ctx context.Context, threadID string) (any, error) {
	return s.withScopedStore(ctx, func(scopedCtx context.Context, st *store.Store, principal auth.Principal) (any, error) {
		if principal.OrgID != "" {
			if err := s.ensureThreadBelongsToOrg(scopedCtx, st, principal.OrgID, threadID); err != nil {
				return nil, err
			}
		}
		thread, messages, err := st.GetThread(scopedCtx, threadID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"thread": thread, "messages": messages}, nil
	})
}

func (s *Service) SearchInbox(ctx context.Context, inboxID string, query string, topK int) (any, error) {
	return s.withScopedStore(ctx, func(scopedCtx context.Context, st *store.Store, principal auth.Principal) (any, error) {
		if principal.OrgID != "" {
			if err := s.ensureInboxBelongsToOrg(scopedCtx, st, principal.OrgID, inboxID); err != nil {
				return nil, err
			}
		}
		if s.Vector != nil && s.Embedder != nil {
			return s.searchVector(scopedCtx, inboxID, query, topK)
		}
		results, err := st.SearchInboxFTS(scopedCtx, inboxID, query, topK)
		if err != nil {
			return nil, err
		}
		return map[string]any{"results": results}, nil
	})
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
	return s.withScopedStore(ctx, func(scopedCtx context.Context, st *store.Store, principal auth.Principal) (any, error) {
		if principal.OrgID != "" {
			if err := s.ensureMessageBelongsToOrg(scopedCtx, st, principal.OrgID, messageID); err != nil {
				return nil, err
			}
		}
		msg, err := st.GetMessage(scopedCtx, messageID)
		if err != nil {
			return nil, err
		}
		classification, err := s.LLM.Classify(scopedCtx, msg.Text, nil)
		if err != nil {
			return nil, err
		}
		_ = st.UpdateThreadSignals(scopedCtx, msg.ThreadID, ptrFloat(classificationConfidenceToSentiment(classification.Sentiment)), classification.Urgency)
		return map[string]any{
			"intent":          classification.Intent,
			"urgency":         classification.Urgency,
			"sentiment":       classification.Sentiment,
			"confidence":      classification.Confidence,
			"suggested_route": "support",
		}, nil
	})
}

func (s *Service) ExtractToSchema(ctx context.Context, messageID string, schemaID string) (any, error) {
	return s.withScopedStore(ctx, func(scopedCtx context.Context, st *store.Store, principal auth.Principal) (any, error) {
		if principal.OrgID != "" {
			if err := s.ensureMessageBelongsToOrg(scopedCtx, st, principal.OrgID, messageID); err != nil {
				return nil, err
			}
		}
		msg, err := st.GetMessage(scopedCtx, messageID)
		if err != nil {
			return nil, err
		}
		schema, err := LoadSchema(schemaID)
		if err != nil {
			return nil, err
		}
		result, err := s.LLM.Extract(scopedCtx, msg.Text, schema, nil)
		if err != nil {
			return nil, err
		}
		validated, validationErrors := validateJSON(schema, result.Data)
		if !validated {
			result.ValidationErrors = validationErrors
			// One repair attempt
			repair, err := s.LLM.Extract(scopedCtx, msg.Text, schema, nil)
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
	})
}

func (s *Service) DraftReply(ctx context.Context, threadID string, goal string) (any, error) {
	return s.withScopedStore(ctx, func(scopedCtx context.Context, st *store.Store, principal auth.Principal) (any, error) {
		if principal.OrgID != "" {
			if err := s.ensureThreadBelongsToOrg(scopedCtx, st, principal.OrgID, threadID); err != nil {
				return nil, err
			}
		}
		thread, messages, err := st.GetThread(scopedCtx, threadID)
		if err != nil {
			return nil, err
		}
		contextText := buildThreadContext(thread, messages)
		draft, err := s.LLM.Draft(scopedCtx, contextText, nil, goal)
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
	})
}

func (s *Service) SendReply(ctx context.Context, threadID string, body string, bodyHTML string, needsApproval bool, idempotencyKey string) (any, error) {
	if needsApproval {
		if !s.Config.Cloud.Mode {
			if !s.Config.Security.AllowSendWithWarnings {
				return nil, errors.New("send blocked: needs human approval")
			}
		} else {
			reservation, ok := entitlements.ReservationFromContext(ctx)
			if !ok {
				return nil, errors.New("missing entitlement context")
			}
			if !entitlements.FeatureBool(reservation.Features, "email_autopilot_send_override", false) {
				return nil, errors.New("send blocked: needs human approval")
			}
		}
	}
	return s.withScopedStore(ctx, func(scopedCtx context.Context, st *store.Store, principal auth.Principal) (any, error) {
		if principal.OrgID != "" {
			if err := s.ensureThreadBelongsToOrg(scopedCtx, st, principal.OrgID, threadID); err != nil {
				return nil, err
			}
		}
		thread, messages, err := st.GetThread(scopedCtx, threadID)
		if err != nil {
			return nil, err
		}
		inboxID, _ := st.GetThreadInboxID(scopedCtx, threadID)
		if len(messages) == 0 {
			return nil, errors.New("no messages in thread")
		}
		to := messages[len(messages)-1].From.Email
		if to == "" {
			return nil, errors.New("missing recipient")
		}

		inbox, err := s.activeInboxRecord(scopedCtx, st, principal, inboxID)
		if err != nil {
			return nil, err
		}
		from := strings.TrimSpace(inbox.Address)
		if from == "" {
			from = strings.TrimSpace(s.Config.SMTP.From)
			if from == "" {
				from = "dev@local.nerve.email"
			}
		}

		if idempotencyKey == "" {
			return nil, errors.New("missing idempotency_key")
		}

		if !s.Config.Security.AllowOutbound && !isLocalDevRecipient(to) {
			return nil, errors.New("outbound disabled for non-local domains")
		}
		if len(s.Config.Security.OutboundDomainAllowlist) > 0 && !domainAllowed(to, s.Config.Security.OutboundDomainAllowlist) {
			return nil, errors.New("recipient domain not allowlisted")
		}

		provider := strings.TrimSpace(inbox.OutboundProvider)
		if provider == "" {
			provider = "smtp"
		}
		if s.Transport == nil {
			return nil, errors.New("missing transport registry")
		}
		if _, ok := s.Transport.Outbound(provider); !ok {
			return nil, fmt.Errorf("unknown outbound provider: %s", provider)
		}

		subject := "Re: " + thread.Subject
		if subject == "Re: " {
			subject = "Reply"
		}

		outboxID, err := st.EnqueueOutboxMessage(scopedCtx, store.OutboxMessage{
			OrgID:          inbox.OrgID,
			InboxID:        inboxID,
			Provider:       provider,
			IdempotencyKey: idempotencyKey,
			To:             to,
			From:           from,
			Subject:        subject,
			TextBody:       body,
			HTMLBody:       bodyHTML,
		})
		if err != nil {
			return nil, err
		}

		if _, err := st.GetMessage(scopedCtx, outboxID); err == nil {
			return map[string]any{"message_id": outboxID, "status": "queued"}, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}

		msg := store.Message{
			ID:        outboxID,
			InboxID:   inboxID,
			Direction: "outbound",
			Subject:   subject,
			Text:      body,
			HTML:      bodyHTML,
			CreatedAt: time.Now().UTC(),
			From:      store.Participant{Email: from},
			To:        []store.Participant{{Email: to}},
		}
		msg.ThreadID = thread.ID
		msgID, err := st.InsertMessage(scopedCtx, msg)
		if err != nil {
			return nil, err
		}
		return map[string]any{"message_id": msgID, "status": "queued"}, nil
	})
}

func (s *Service) ComposeEmail(ctx context.Context, inboxID, toAddress, subject, body string, bodyHTML string, idempotencyKey string) (any, error) {
	if subject == "" {
		return nil, errors.New("missing subject")
	}
	if body == "" && bodyHTML == "" {
		return nil, errors.New("missing body")
	}
	if toAddress == "" {
		return nil, errors.New("missing recipient")
	}
	if inboxID == "" {
		return nil, errors.New("missing inbox_id")
	}

	return s.withScopedStore(ctx, func(scopedCtx context.Context, st *store.Store, principal auth.Principal) (any, error) {
		if principal.OrgID != "" {
			if err := s.ensureInboxBelongsToOrg(scopedCtx, st, principal.OrgID, inboxID); err != nil {
				return nil, err
			}
		}

		inbox, err := s.activeInboxRecord(scopedCtx, st, principal, inboxID)
		if err != nil {
			return nil, err
		}
		from := strings.TrimSpace(inbox.Address)
		if from == "" {
			from = strings.TrimSpace(s.Config.SMTP.From)
			if from == "" {
				from = "dev@local.nerve.email"
			}
		}

		if idempotencyKey == "" {
			return nil, errors.New("missing idempotency_key")
		}

		if !s.Config.Security.AllowOutbound && !isLocalDevRecipient(toAddress) {
			return nil, errors.New("outbound disabled for non-local domains")
		}
		if len(s.Config.Security.OutboundDomainAllowlist) > 0 && !domainAllowed(toAddress, s.Config.Security.OutboundDomainAllowlist) {
			return nil, errors.New("recipient domain not allowlisted")
		}

		provider := strings.TrimSpace(inbox.OutboundProvider)
		if provider == "" {
			provider = "smtp"
		}
		if s.Transport == nil {
			return nil, errors.New("missing transport registry")
		}
		if _, ok := s.Transport.Outbound(provider); !ok {
			return nil, fmt.Errorf("unknown outbound provider: %s", provider)
		}

		outboxID, err := st.EnqueueOutboxMessage(scopedCtx, store.OutboxMessage{
			OrgID:          inbox.OrgID,
			InboxID:        inboxID,
			Provider:       provider,
			IdempotencyKey: idempotencyKey,
			To:             toAddress,
			From:           from,
			Subject:        subject,
			TextBody:       body,
			HTMLBody:       bodyHTML,
		})
		if err != nil {
			return nil, err
		}

		if existing, err := st.GetMessage(scopedCtx, outboxID); err == nil {
			return map[string]any{
				"thread_id":  existing.ThreadID,
				"message_id": outboxID,
				"status":     "queued",
			}, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}

		msg := store.Message{
			ID:        outboxID,
			Direction: "outbound",
			Subject:   subject,
			Text:      body,
			HTML:      bodyHTML,
			CreatedAt: time.Now().UTC(),
			From:      store.Participant{Email: from},
			To:        []store.Participant{{Email: toAddress}},
		}

		providerThreadID := "compose-" + outboxID
		threadID, msgID, err := st.InsertMessageWithThread(scopedCtx, inboxID, providerThreadID, msg)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"thread_id":  threadID,
			"message_id": msgID,
			"status":     "queued",
		}, nil
	})
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

func isLocalDevRecipient(addr string) bool {
	lower := strings.ToLower(strings.TrimSpace(addr))
	return strings.HasSuffix(lower, "@local.nerve.email") || strings.HasSuffix(lower, "@local.neuralmail")
}

func (s *Service) activeInboxRecord(ctx context.Context, st *store.Store, principal auth.Principal, inboxID string) (store.InboxRecord, error) {
	if st == nil || inboxID == "" {
		return store.InboxRecord{}, errors.New("missing inbox")
	}
	var (
		inbox store.InboxRecord
		err   error
	)
	if principal.OrgID != "" {
		inbox, err = st.GetInboxRecordByIDForOrg(ctx, principal.OrgID, inboxID)
	} else {
		inbox, err = st.GetInboxRecordByID(ctx, inboxID)
	}
	if err != nil {
		if principal.OrgID != "" && errors.Is(err, sql.ErrNoRows) {
			return store.InboxRecord{}, errors.New("inbox not found")
		}
		return store.InboxRecord{}, err
	}
	if strings.ToLower(strings.TrimSpace(inbox.Status)) != "active" {
		return store.InboxRecord{}, errors.New("inbox is not active")
	}
	return inbox, nil
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
