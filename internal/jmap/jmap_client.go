package jmap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"neuralmail/internal/config"
	"neuralmail/internal/store"
)

const mailCapability = "urn:ietf:params:jmap:mail"

type JMAPClient struct {
	cfg           config.Config
	httpClient    *http.Client
	apiURL        string
	accountID     string
	inboxMailboxID string
}

func NewJMAPClient(cfg config.Config) (*JMAPClient, error) {
	if cfg.JMAP.URL == "" || cfg.JMAP.Username == "" || cfg.JMAP.Password == "" {
		return nil, ErrNotConfigured
	}
	return &JMAPClient{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}, nil
}

func (c *JMAPClient) Name() string { return "jmap" }

func (c *JMAPClient) FetchChanges(ctx context.Context, sinceState string) ([]Email, string, error) {
	if err := c.ensureSession(ctx); err != nil {
		return nil, sinceState, err
	}
	if err := c.ensureInboxMailbox(ctx); err != nil {
		return nil, sinceState, err
	}
	var ids []string
	var newState string
	if sinceState == "" {
		queryState, queryIDs, err := c.emailQuery(ctx)
		if err != nil {
			return nil, sinceState, err
		}
		ids = queryIDs
		newState = queryState
	} else {
		state, createdIDs, err := c.emailChanges(ctx, sinceState)
		if err != nil {
			return nil, sinceState, err
		}
		ids = createdIDs
		newState = state
	}
	if len(ids) == 0 {
		return nil, newState, nil
	}
	emails, err := c.emailGet(ctx, ids)
	if err != nil {
		return nil, newState, err
	}
	return emails, newState, nil
}

func (c *JMAPClient) ensureSession(ctx context.Context) error {
	if c.apiURL != "" && c.accountID != "" {
		return nil
	}
	sessionURL := c.cfg.JMAP.SessionURL
	if sessionURL == "" {
		parsed, err := url.Parse(c.cfg.JMAP.URL)
		if err != nil {
			return err
		}
		sessionURL = fmt.Sprintf("%s://%s/.well-known/jmap", parsed.Scheme, parsed.Host)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sessionURL, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.cfg.JMAP.Username, c.cfg.JMAP.Password)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("jmap session error: %d", resp.StatusCode)
	}

	var session struct {
		APIURL          string            `json:"apiUrl"`
		PrimaryAccounts map[string]string `json:"primaryAccounts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		return err
	}
	if session.APIURL == "" {
		return errors.New("missing apiUrl in session")
	}
	accountID := session.PrimaryAccounts[mailCapability]
	if accountID == "" {
		return errors.New("missing mail account id")
	}
	c.apiURL = resolveURL(sessionURL, session.APIURL)
	c.accountID = accountID
	return nil
}

func (c *JMAPClient) ensureInboxMailbox(ctx context.Context) error {
	if c.inboxMailboxID != "" {
		return nil
	}
	args := map[string]any{
		"accountId": c.accountID,
		"properties": []string{"id", "name", "role"},
	}
	resp, err := c.call(ctx, "Mailbox/get", args)
	if err != nil {
		return err
	}
	list, ok := resp["list"].([]any)
	if !ok {
		return errors.New("invalid mailbox list")
	}
	for _, item := range list {
		mbox, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role := getString(mbox, "role")
		name := strings.ToLower(getString(mbox, "name"))
		if role == "inbox" || name == "inbox" {
			c.inboxMailboxID = getString(mbox, "id")
			return nil
		}
	}
	return errors.New("inbox mailbox not found")
}

func (c *JMAPClient) emailQuery(ctx context.Context) (string, []string, error) {
	args := map[string]any{
		"accountId": c.accountID,
		"filter": map[string]any{
			"inMailbox": c.inboxMailboxID,
		},
		"sort": []map[string]any{{
			"property":    "receivedAt",
			"isAscending": false,
		}},
		"position": 0,
		"limit":    50,
	}
	resp, err := c.call(ctx, "Email/query", args)
	if err != nil {
		return "", nil, err
	}
	queryState := getString(resp, "queryState")
	ids := toStringSlice(resp["ids"])
	return queryState, ids, nil
}

func (c *JMAPClient) emailChanges(ctx context.Context, sinceState string) (string, []string, error) {
	args := map[string]any{
		"accountId":  c.accountID,
		"sinceState": sinceState,
		"maxChanges": 50,
	}
	resp, err := c.call(ctx, "Email/changes", args)
	if err != nil {
		return sinceState, nil, err
	}
	newState := getString(resp, "newState")
	created := toStringSlice(resp["created"])
	updated := toStringSlice(resp["updated"])
	return newState, append(created, updated...), nil
}

func (c *JMAPClient) emailGet(ctx context.Context, ids []string) ([]Email, error) {
	args := map[string]any{
		"accountId": c.accountID,
		"ids":       ids,
		"properties": []string{
			"id", "threadId", "subject", "from", "to", "cc", "receivedAt", "bodyValues", "textBody", "htmlBody", "messageId",
		},
	}
	resp, err := c.call(ctx, "Email/get", args)
	if err != nil {
		return nil, err
	}
	list, ok := resp["list"].([]any)
	if !ok {
		return nil, errors.New("invalid email list")
	}
	var emails []Email
	for _, item := range list {
		emailMap, ok := item.(map[string]any)
		if !ok {
			continue
		}
		received := time.Now().UTC()
		if raw := getString(emailMap, "receivedAt"); raw != "" {
			if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
				received = parsed
			}
		}
		text, html := extractBodies(emailMap)
		emails = append(emails, Email{
			ID:          getString(emailMap, "id"),
			ThreadID:    getString(emailMap, "threadId"),
			Subject:     getString(emailMap, "subject"),
			Text:        text,
			HTML:        html,
			From:        firstParticipant(emailMap["from"]),
			To:          parseParticipants(emailMap["to"]),
			ReceivedAt:  received,
			InternetMsg: getString(emailMap, "messageId"),
		})
	}
	return emails, nil
}

func (c *JMAPClient) call(ctx context.Context, method string, args map[string]any) (map[string]any, error) {
	payload := map[string]any{
		"using": []string{mailCapability},
		"methodCalls": []any{
			[]any{method, args, "c1"},
		},
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(c.cfg.JMAP.Username, c.cfg.JMAP.Password)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("jmap call %s failed: %d", method, resp.StatusCode)
	}

	var decoded struct {
		MethodResponses []any `json:"methodResponses"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, err
	}
	for _, raw := range decoded.MethodResponses {
		arr, ok := raw.([]any)
		if !ok || len(arr) < 2 {
			continue
		}
		name, _ := arr[0].(string)
		if name == "error" {
			return nil, errors.New("jmap error response")
		}
		if name == method {
			if argsMap, ok := arr[1].(map[string]any); ok {
				return argsMap, nil
			}
		}
	}
	return nil, errors.New("missing jmap response")
}

func resolveURL(base string, target string) string {
	if strings.HasPrefix(target, "http") {
		return target
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return target
	}
	ref, err := url.Parse(target)
	if err != nil {
		return target
	}
	return baseURL.ResolveReference(ref).String()
}

func getString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	val, ok := m[key]
	if !ok {
		return ""
	}
	if s, ok := val.(string); ok {
		return s
	}
	return ""
}

func toStringSlice(raw any) []string {
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func parseParticipants(raw any) []store.Participant {
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	var participants []store.Participant
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		participants = append(participants, store.Participant{
			Name:  getString(m, "name"),
			Email: getString(m, "email"),
		})
	}
	return participants
}

func firstParticipant(raw any) store.Participant {
	participants := parseParticipants(raw)
	if len(participants) == 0 {
		return store.Participant{}
	}
	return participants[0]
}

func extractBodies(email map[string]any) (string, string) {
	bodyValues, _ := email["bodyValues"].(map[string]any)
	textBody := extractBodyValue(bodyValues, email["textBody"])
	htmlBody := extractBodyValue(bodyValues, email["htmlBody"])
	return textBody, htmlBody
}

func extractBodyValue(values map[string]any, raw any) string {
	if values == nil {
		return ""
	}
	parts, ok := raw.([]any)
	if !ok || len(parts) == 0 {
		return ""
	}
	part, ok := parts[0].(map[string]any)
	if !ok {
		return ""
	}
	partID := getString(part, "partId")
	if partID == "" {
		return ""
	}
	valueRaw, ok := values[partID].(map[string]any)
	if !ok {
		return ""
	}
	return getString(valueRaw, "value")
}
