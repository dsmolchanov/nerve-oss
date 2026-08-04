package jmap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"neuralmail/internal/config"
	"neuralmail/internal/store"
)

const (
	coreCapability          = "urn:ietf:params:jmap:core"
	mailCapability          = "urn:ietf:params:jmap:mail"
	initialEmailQueryLimit  = 50
	recoveryQueryPageSize   = 50
	recoveryQueryMaxRetries = 3
)

type JMAPMethodError struct {
	Method      string
	Type        string
	Description string
}

func (e *JMAPMethodError) Error() string {
	if e.Type != "" && e.Description != "" {
		return fmt.Sprintf("jmap method %s failed with %s: %s", e.Method, e.Type, e.Description)
	}
	if e.Type != "" {
		return fmt.Sprintf("jmap method %s failed with %s", e.Method, e.Type)
	}
	if e.Description != "" {
		return fmt.Sprintf("jmap method %s failed: %s", e.Method, e.Description)
	}
	return fmt.Sprintf("jmap method %s returned an error response", e.Method)
}

type JMAPClient struct {
	cfg             config.Config
	httpClient      *http.Client
	apiURL          string
	accountID       string
	inboxMailboxID  string
	maxObjectsInGet int
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
		state, addedIDs, err := c.emailQueryChanges(ctx, sinceState)
		if err != nil {
			if !invalidQueryCheckpoint(err) {
				return nil, sinceState, err
			}
			state, addedIDs, err = c.emailQueryAll(ctx)
			if err != nil {
				return nil, sinceState, err
			}
		}
		ids = addedIDs
		newState = state
	}
	if len(ids) == 0 {
		return nil, newState, nil
	}
	emails, err := c.emailGet(ctx, ids)
	if err != nil {
		return nil, sinceState, err
	}
	return emails, newState, nil
}

func (c *JMAPClient) ensureSession(ctx context.Context) error {
	if c.apiURL != "" && c.accountID != "" && c.maxObjectsInGet > 0 {
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
		APIURL          string                     `json:"apiUrl"`
		Capabilities    map[string]json.RawMessage `json:"capabilities"`
		PrimaryAccounts map[string]string          `json:"primaryAccounts"`
		Accounts        map[string]struct {
			AccountCapabilities map[string]json.RawMessage `json:"accountCapabilities"`
		} `json:"accounts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		return err
	}
	if session.APIURL == "" {
		return errors.New("missing apiUrl in session")
	}
	accountID := c.cfg.JMAP.AccountID
	if accountID != "" {
		account, ok := session.Accounts[accountID]
		if !ok {
			return fmt.Errorf("configured JMAP account %q not found in session", accountID)
		}
		if _, ok := account.AccountCapabilities[mailCapability]; !ok {
			return fmt.Errorf("configured JMAP account %q does not support mail", accountID)
		}
	} else {
		accountID = session.PrimaryAccounts[mailCapability]
		if accountID == "" {
			return errors.New("missing mail account id")
		}
	}
	coreRaw, ok := session.Capabilities[coreCapability]
	if !ok {
		return errors.New("missing JMAP core capability")
	}
	var core struct {
		MaxObjectsInGet int `json:"maxObjectsInGet"`
	}
	if err := json.Unmarshal(coreRaw, &core); err != nil {
		return fmt.Errorf("decode JMAP core capability: %w", err)
	}
	if core.MaxObjectsInGet <= 0 {
		return errors.New("invalid JMAP core maxObjectsInGet: must be positive")
	}
	c.apiURL = resolveURL(sessionURL, session.APIURL)
	c.accountID = accountID
	c.maxObjectsInGet = core.MaxObjectsInGet
	return nil
}

func (c *JMAPClient) ensureInboxMailbox(ctx context.Context) error {
	if c.inboxMailboxID != "" {
		return nil
	}
	args := map[string]any{
		"accountId":  c.accountID,
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
	filter, sort := c.emailQuerySpec()
	args := map[string]any{
		"accountId": c.accountID,
		"filter":    filter,
		"sort":      sort,
		"position":  0,
		"limit":     initialEmailQueryLimit,
	}
	resp, err := c.call(ctx, "Email/query", args)
	if err != nil {
		return "", nil, err
	}
	queryState := getString(resp, "queryState")
	if queryState == "" {
		return "", nil, errors.New("invalid Email/query response: missing queryState")
	}
	if err := requireCanCalculateChanges(resp); err != nil {
		return "", nil, err
	}
	ids, err := strictStringSlice(resp["ids"])
	if err != nil {
		return "", nil, fmt.Errorf("invalid Email/query response: %w", err)
	}
	return queryState, ids, nil
}

func (c *JMAPClient) emailQueryAll(ctx context.Context) (string, []string, error) {
	for attempt := 1; attempt <= recoveryQueryMaxRetries; attempt++ {
		queryState, ids, drifted, err := c.emailQueryAllAttempt(ctx)
		if err != nil {
			return "", nil, err
		}
		if !drifted {
			return queryState, ids, nil
		}
	}
	return "", nil, fmt.Errorf("Email/query recovery state changed during all %d attempts", recoveryQueryMaxRetries)
}

func (c *JMAPClient) emailQueryAllAttempt(ctx context.Context) (string, []string, bool, error) {
	filter, sort := c.emailQuerySpec()
	position := 0
	expectedState := ""
	expectedTotal := -1
	seen := make(map[string]struct{})
	var allIDs []string

	for {
		args := map[string]any{
			"accountId":      c.accountID,
			"filter":         filter,
			"sort":           sort,
			"position":       position,
			"limit":          recoveryQueryPageSize,
			"calculateTotal": true,
		}
		resp, err := c.call(ctx, "Email/query", args)
		if err != nil {
			return "", nil, false, err
		}
		queryState := getString(resp, "queryState")
		if queryState == "" {
			return "", nil, false, errors.New("invalid recovery Email/query response: missing queryState")
		}
		if err := requireCanCalculateChanges(resp); err != nil {
			return "", nil, false, fmt.Errorf("invalid recovery Email/query response: %w", err)
		}
		total, err := nonNegativeInt(resp, "total")
		if err != nil {
			return "", nil, false, fmt.Errorf("invalid recovery Email/query response: %w", err)
		}
		if expectedState != "" && (queryState != expectedState || total != expectedTotal) {
			return "", nil, true, nil
		}
		responsePosition, err := nonNegativeInt(resp, "position")
		if err != nil {
			return "", nil, false, fmt.Errorf("invalid recovery Email/query response: %w", err)
		}
		if responsePosition != position {
			return "", nil, false, fmt.Errorf("invalid recovery Email/query response: position %d does not match requested position %d", responsePosition, position)
		}

		if expectedState == "" {
			expectedState = queryState
			expectedTotal = total
		}

		ids, err := strictStringSlice(resp["ids"])
		if err != nil {
			return "", nil, false, fmt.Errorf("invalid recovery Email/query response: %w", err)
		}
		if len(ids) > recoveryQueryPageSize {
			return "", nil, false, fmt.Errorf("invalid recovery Email/query response: page contains %d ids, limit is %d", len(ids), recoveryQueryPageSize)
		}
		if position+len(ids) > expectedTotal {
			return "", nil, false, fmt.Errorf("invalid recovery Email/query response: page exceeds total %d", expectedTotal)
		}
		if len(ids) == 0 && position < expectedTotal {
			return "", nil, false, fmt.Errorf("invalid recovery Email/query response: empty page at position %d before total %d", position, expectedTotal)
		}
		for _, id := range ids {
			if _, duplicate := seen[id]; duplicate {
				return "", nil, false, fmt.Errorf("invalid recovery Email/query response: duplicate id %q", id)
			}
			seen[id] = struct{}{}
			allIDs = append(allIDs, id)
		}
		position += len(ids)
		if position == expectedTotal {
			return expectedState, allIDs, false, nil
		}
	}
}

func (c *JMAPClient) emailQueryChanges(ctx context.Context, sinceQueryState string) (string, []string, error) {
	filter, sort := c.emailQuerySpec()
	args := map[string]any{
		"accountId":       c.accountID,
		"filter":          filter,
		"sort":            sort,
		"sinceQueryState": sinceQueryState,
	}
	resp, err := c.call(ctx, "Email/queryChanges", args)
	if err != nil {
		return sinceQueryState, nil, err
	}
	newQueryState := getString(resp, "newQueryState")
	if newQueryState == "" {
		return sinceQueryState, nil, errors.New("invalid Email/queryChanges response: missing newQueryState")
	}
	oldQueryState := getString(resp, "oldQueryState")
	if oldQueryState != sinceQueryState {
		return sinceQueryState, nil, fmt.Errorf("invalid Email/queryChanges response: oldQueryState %q does not match requested state %q", oldQueryState, sinceQueryState)
	}
	added, ok := resp["added"]
	if !ok {
		return sinceQueryState, nil, errors.New("invalid Email/queryChanges response: missing added")
	}
	addedIDs, err := strictQueryAddedIDs(added)
	if err != nil {
		return sinceQueryState, nil, fmt.Errorf("invalid Email/queryChanges response: %w", err)
	}
	return newQueryState, addedIDs, nil
}

func invalidQueryCheckpoint(err error) bool {
	var methodErr *JMAPMethodError
	if !errors.As(err, &methodErr) {
		return false
	}
	return methodErr.Type == "cannotCalculateChanges" || methodErr.Type == "tooManyChanges"
}

func (c *JMAPClient) emailQuerySpec() (map[string]any, []map[string]any) {
	return map[string]any{
			"inMailbox": c.inboxMailboxID,
		}, []map[string]any{{
			"property":    "receivedAt",
			"isAscending": false,
		}}
}

func (c *JMAPClient) emailGet(ctx context.Context, ids []string) ([]Email, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if c.maxObjectsInGet <= 0 {
		return nil, errors.New("invalid JMAP core maxObjectsInGet: must be positive")
	}
	seenIDs := make(map[string]struct{}, len(ids))
	for index, id := range ids {
		if id == "" {
			return nil, fmt.Errorf("Email/get ids[%d] must be non-empty", index)
		}
		if _, duplicate := seenIDs[id]; duplicate {
			return nil, fmt.Errorf("Email/get ids contain duplicate %q", id)
		}
		seenIDs[id] = struct{}{}
	}
	allEmails := make([]Email, 0, len(ids))
	for start := 0; start < len(ids); start += c.maxObjectsInGet {
		end := start + c.maxObjectsInGet
		if end > len(ids) {
			end = len(ids)
		}
		emails, err := c.emailGetBatch(ctx, ids[start:end])
		if err != nil {
			return nil, err
		}
		allEmails = append(allEmails, emails...)
	}
	return allEmails, nil
}

func (c *JMAPClient) emailGetBatch(ctx context.Context, ids []string) ([]Email, error) {
	args := map[string]any{
		"accountId":           c.accountID,
		"ids":                 ids,
		"fetchTextBodyValues": true,
		"fetchHTMLBodyValues": true,
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
	requested := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		requested[id] = struct{}{}
	}
	accounted := make(map[string]struct{}, len(ids))
	emailsByID := make(map[string]Email, len(list))
	for index, item := range list {
		emailMap, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid Email/get response: list[%d] must be an object", index)
		}
		id := getString(emailMap, "id")
		if id == "" {
			return nil, fmt.Errorf("invalid Email/get response: list[%d].id must be a non-empty string", index)
		}
		if _, ok := requested[id]; !ok {
			return nil, fmt.Errorf("invalid Email/get response: unrequested id %q in list", id)
		}
		if _, duplicate := accounted[id]; duplicate {
			return nil, fmt.Errorf("invalid Email/get response: duplicate id %q", id)
		}
		accounted[id] = struct{}{}
		received := time.Now().UTC()
		if raw := getString(emailMap, "receivedAt"); raw != "" {
			if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
				received = parsed
			}
		}
		text, html := extractBodies(emailMap)
		emailsByID[id] = Email{
			ID:          id,
			ThreadID:    getString(emailMap, "threadId"),
			Subject:     getString(emailMap, "subject"),
			Text:        text,
			HTML:        html,
			From:        firstParticipant(emailMap["from"]),
			To:          parseParticipants(emailMap["to"]),
			CC:          parseParticipants(emailMap["cc"]),
			ReceivedAt:  received,
			InternetMsg: firstString(emailMap["messageId"]),
		}
	}
	if rawNotFound, present := resp["notFound"]; present && rawNotFound != nil {
		notFound, err := strictStringSlice(rawNotFound)
		if err != nil {
			return nil, fmt.Errorf("invalid Email/get response: notFound %w", err)
		}
		for _, id := range notFound {
			if _, ok := requested[id]; !ok {
				return nil, fmt.Errorf("invalid Email/get response: unrequested id %q in notFound", id)
			}
			if _, duplicate := accounted[id]; duplicate {
				return nil, fmt.Errorf("invalid Email/get response: duplicate id %q", id)
			}
			accounted[id] = struct{}{}
		}
	}
	if len(accounted) != len(requested) {
		for _, id := range ids {
			if _, ok := accounted[id]; !ok {
				return nil, fmt.Errorf("invalid Email/get response: requested id %q is missing from list and notFound", id)
			}
		}
	}
	emails := make([]Email, 0, len(emailsByID))
	for _, id := range ids {
		if email, ok := emailsByID[id]; ok {
			emails = append(emails, email)
		}
	}
	return emails, nil
}

func (c *JMAPClient) call(ctx context.Context, method string, args map[string]any) (map[string]any, error) {
	payload := map[string]any{
		"using": []string{coreCapability, mailCapability},
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
			details, _ := arr[1].(map[string]any)
			return nil, &JMAPMethodError{
				Method:      method,
				Type:        getString(details, "type"),
				Description: getString(details, "description"),
			}
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

func strictStringSlice(raw any) ([]string, error) {
	arr, ok := raw.([]any)
	if !ok {
		return nil, errors.New("ids must be an array")
	}
	ids := make([]string, 0, len(arr))
	for index, item := range arr {
		id, ok := item.(string)
		if !ok || id == "" {
			return nil, fmt.Errorf("ids[%d] must be a non-empty string", index)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func requireCanCalculateChanges(response map[string]any) error {
	canCalculate, ok := response["canCalculateChanges"].(bool)
	if !ok {
		return errors.New("missing canCalculateChanges")
	}
	if !canCalculate {
		return errors.New("canCalculateChanges is false")
	}
	return nil
}

func nonNegativeInt(response map[string]any, key string) (int, error) {
	value, ok := response[key].(float64)
	if !ok || value < 0 || math.Trunc(value) != value || value > float64(int(^uint(0)>>1)) {
		return 0, fmt.Errorf("%s must be a non-negative integer", key)
	}
	return int(value), nil
}

func strictQueryAddedIDs(raw any) ([]string, error) {
	// RFC 8620 types this as AddedItem[], but its queryChanges example uses
	// null when there are no additions. Treat that explicit form as empty.
	if raw == nil {
		return nil, nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, errors.New("added must be an array")
	}
	ids := make([]string, 0, len(arr))
	seen := make(map[string]struct{}, len(arr))
	previousIndex := -1
	for index, item := range arr {
		added, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("added[%d] must be an object", index)
		}
		id := getString(added, "id")
		if id == "" {
			return nil, fmt.Errorf("added[%d].id must be a non-empty string", index)
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("added contains duplicate id %q", id)
		}
		addedIndex, err := nonNegativeInt(added, "index")
		if err != nil {
			return nil, fmt.Errorf("added[%d].%w", index, err)
		}
		if addedIndex <= previousIndex {
			return nil, fmt.Errorf("added[%d].index %d must be greater than previous index %d", index, addedIndex, previousIndex)
		}
		previousIndex = addedIndex
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}

func firstString(raw any) string {
	values, ok := raw.([]any)
	if !ok || len(values) == 0 {
		return ""
	}
	value, _ := values[0].(string)
	return value
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
	textBody := extractBodyValues(bodyValues, email["textBody"])
	htmlBody := extractBodyValues(bodyValues, email["htmlBody"])
	return textBody, htmlBody
}

func extractBodyValues(values map[string]any, raw any) string {
	if values == nil {
		return ""
	}
	parts, ok := raw.([]any)
	if !ok {
		return ""
	}
	var body strings.Builder
	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok {
			continue
		}
		partID := getString(part, "partId")
		if partID == "" {
			continue
		}
		valueRaw, ok := values[partID].(map[string]any)
		if !ok {
			continue
		}
		body.WriteString(getString(valueRaw, "value"))
	}
	return body.String()
}
