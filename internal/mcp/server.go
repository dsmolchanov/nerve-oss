package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"neuralmail/internal/config"
	"neuralmail/internal/observability"
	"neuralmail/internal/tools"
)

type Server struct {
	Config   config.Config
	Tools    *tools.Service
	mu       sync.Mutex
	sessions map[string]time.Time
}

func NewServer(cfg config.Config, toolsSvc *tools.Service) *Server {
	return &Server{Config: cfg, Tools: toolsSvc, sessions: make(map[string]time.Time)}
}

func (s *Server) HandleHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err := s.validateOrigin(r); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	var req Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	sessionID := r.Header.Get("MCP-Session-Id")
	if req.Method != "initialize" {
		if !s.isSessionValid(sessionID) {
			writeError(w, req.ID, -32000, "missing or invalid MCP-Session-Id")
			return
		}
	}
	result, err := s.dispatch(r.Context(), req)
	if err != nil {
		writeError(w, req.ID, -32000, err.Error())
		return
	}
	if req.Method == "initialize" {
		if sessionID == "" {
			sessionID = s.newSession()
		}
		w.Header().Set("MCP-Session-Id", sessionID)
	}
	w.Header().Set("MCP-Protocol-Version", s.Config.MCP.ProtocolVersion)
	w.Header().Set("Content-Type", "application/json")
	resp := Response{JSONRPC: "2.0", ID: req.ID, Result: result}
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) HandleSSEStub(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusNotImplemented)
	_, _ = w.Write([]byte("SSE not supported in MVP; use POST /mcp"))
}

func (s *Server) dispatch(ctx context.Context, req Request) (any, error) {
	switch req.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": s.Config.MCP.ProtocolVersion,
			"serverInfo": map[string]any{
				"name": "neuralmaild",
				"version": "0.1.0",
			},
			"capabilities": map[string]any{
				"tools":     true,
				"resources": true,
			},
		}, nil
	case "tools/list":
		return ListTools(), nil
	case "tools/call":
		return s.callTool(ctx, req)
	case "resources/list":
		return ListResources(), nil
	case "resources/read":
		return s.readResource(ctx, req)
	default:
		return nil, fmt.Errorf("unknown method: %s", req.Method)
	}
}

func (s *Server) callTool(ctx context.Context, req Request) (any, error) {
	var params ToolCallParams
	if err := decodeParams(req.Params, &params); err != nil {
		return nil, err
	}
	start := time.Now()
	inputsHash := hashJSON(params.Arguments)
	replayID := observability.NewReplayID()

	switch params.Name {
	case "list_threads":
		var input struct {
			InboxID string `json:"inbox_id"`
			Status  string `json:"status"`
			Limit   int    `json:"limit"`
		}
		if err := json.Unmarshal(params.Arguments, &input); err != nil {
			return nil, err
		}
		result, err := s.Tools.ListThreads(ctx, input.InboxID, input.Status, input.Limit)
		result = attachReplayID(result, replayID)
		result = attachAuditID(result, s.recordToolCall(ctx, params.Name, inputsHash, result, start, replayID))
		return result, err
	case "get_thread":
		var input struct {
			ThreadID string `json:"thread_id"`
		}
		if err := json.Unmarshal(params.Arguments, &input); err != nil {
			return nil, err
		}
		result, err := s.Tools.GetThread(ctx, input.ThreadID)
		result = attachReplayID(result, replayID)
		result = attachAuditID(result, s.recordToolCall(ctx, params.Name, inputsHash, result, start, replayID))
		return result, err
	case "search_inbox":
		var input struct {
			InboxID string `json:"inbox_id"`
			Query   string `json:"query"`
			TopK    int    `json:"top_k"`
		}
		if err := json.Unmarshal(params.Arguments, &input); err != nil {
			return nil, err
		}
		result, err := s.Tools.SearchInbox(ctx, input.InboxID, input.Query, input.TopK)
		result = attachReplayID(result, replayID)
		result = attachAuditID(result, s.recordToolCall(ctx, params.Name, inputsHash, result, start, replayID))
		return result, err
	case "triage_message":
		var input struct {
			MessageID  string `json:"message_id"`
			TaxonomyID string `json:"taxonomy_id"`
		}
		if err := json.Unmarshal(params.Arguments, &input); err != nil {
			return nil, err
		}
		result, err := s.Tools.TriageMessage(ctx, input.MessageID)
		result = attachReplayID(result, replayID)
		result = attachAuditID(result, s.recordToolCall(ctx, params.Name, inputsHash, result, start, replayID))
		return result, err
	case "extract_to_schema":
		var input struct {
			MessageID string `json:"message_id"`
			SchemaID  string `json:"schema_id"`
		}
		if err := json.Unmarshal(params.Arguments, &input); err != nil {
			return nil, err
		}
		result, err := s.Tools.ExtractToSchema(ctx, input.MessageID, input.SchemaID)
		result = attachReplayID(result, replayID)
		result = attachAuditID(result, s.recordToolCall(ctx, params.Name, inputsHash, result, start, replayID))
		return result, err
	case "draft_reply_with_policy":
		var input struct {
			ThreadID string `json:"thread_id"`
			Goal     string `json:"goal"`
		}
		if err := json.Unmarshal(params.Arguments, &input); err != nil {
			return nil, err
		}
		result, err := s.Tools.DraftReply(ctx, input.ThreadID, input.Goal)
		result = attachReplayID(result, replayID)
		result = attachAuditID(result, s.recordToolCall(ctx, params.Name, inputsHash, result, start, replayID))
		return result, err
	case "send_reply":
		var input struct {
			ThreadID       string `json:"thread_id"`
			Body           string `json:"body_or_draft_id"`
			NeedsApproval  bool   `json:"needs_human_approval"`
			IdempotencyKey string `json:"idempotency_key"`
		}
		if err := json.Unmarshal(params.Arguments, &input); err != nil {
			return nil, err
		}
		result, err := s.Tools.SendReply(ctx, input.ThreadID, input.Body, input.NeedsApproval)
		result = attachReplayID(result, replayID)
		result = attachAuditID(result, s.recordToolCall(ctx, params.Name, inputsHash, result, start, replayID))
		return result, err
	default:
		return nil, fmt.Errorf("unknown tool: %s", params.Name)
	}
}

func (s *Server) recordToolCall(ctx context.Context, toolName string, inputsHash string, result any, start time.Time, replayID string) string {
	if s.Tools == nil || s.Tools.Store == nil {
		return ""
	}
	outputsHash := hashJSON(result)
	latency := int(time.Since(start).Milliseconds())
	modelName := ""
	promptVersion := s.Tools.Config.LLM.PromptPath
	if s.Tools.LLM != nil {
		modelName = s.Tools.LLM.Name()
	}
	toolCallID, err := s.Tools.Store.RecordToolCall(ctx, toolName, "", modelName, promptVersion, latency)
	if err != nil {
		return ""
	}
	_ = s.Tools.Store.RecordAudit(ctx, toolCallID, "mcp", inputsHash, outputsHash, replayID)
	return toolCallID
}

func hashJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:])
}

func attachReplayID(result any, replayID string) any {
	if replayID == "" {
		return result
	}
	if data, ok := result.(map[string]any); ok {
		data["replay_id"] = replayID
		return data
	}
	return result
}

func attachAuditID(result any, auditID string) any {
	if auditID == "" {
		return result
	}
	if data, ok := result.(map[string]any); ok {
		data["audit_id"] = auditID
		return data
	}
	return result
}

func (s *Server) readResource(ctx context.Context, req Request) (any, error) {
	var params ResourceReadParams
	if err := decodeParams(req.Params, &params); err != nil {
		return nil, err
	}
	switch {
	case params.URI == "email://inboxes":
		ids, err := s.Tools.Store.ListInboxes(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]any{"inbox_ids": ids}, nil
	case strings.HasPrefix(params.URI, "email://threads/"):
		threadID := strings.TrimPrefix(params.URI, "email://threads/")
		return s.Tools.GetThread(ctx, threadID)
	case strings.HasPrefix(params.URI, "email://messages/"):
		messageID := strings.TrimPrefix(params.URI, "email://messages/")
		msg, err := s.Tools.Store.GetMessage(ctx, messageID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"message": msg}, nil
	default:
		return nil, fmt.Errorf("resource not found: %s", params.URI)
	}
}

func (s *Server) validateOrigin(r *http.Request) error {
	origin := r.Header.Get("Origin")
	if s.Config.Dev.Mode {
		return nil
	}
	if origin == "" {
		if s.Config.Security.APIKey == "" {
			return errors.New("missing origin")
		}
		if r.Header.Get("X-API-Key") != s.Config.Security.APIKey {
			return errors.New("invalid api key")
		}
		return nil
	}
	if len(s.Config.MCP.AllowOrigins) == 0 {
		return nil
	}
	for _, allowed := range s.Config.MCP.AllowOrigins {
		if origin == allowed {
			return nil
		}
	}
	return errors.New("origin not allowed")
}

func (s *Server) newSession() string {
	sessionID := uuid.NewString()
	s.mu.Lock()
	s.sessions[sessionID] = time.Now().Add(24 * time.Hour)
	s.mu.Unlock()
	return sessionID
}

func (s *Server) isSessionValid(id string) bool {
	if id == "" {
		return false
	}
	s.mu.Lock()
	expiry, ok := s.sessions[id]
	s.mu.Unlock()
	if !ok {
		return false
	}
	return time.Now().Before(expiry)
}

func decodeParams(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		return errors.New("missing params")
	}
	return json.Unmarshal(raw, out)
}

func writeError(w http.ResponseWriter, id any, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	resp := Response{
		JSONRPC: "2.0",
		ID:      id,
		Error: &ResponseError{Code: code, Message: message},
	}
	_ = json.NewEncoder(w).Encode(resp)
}
