package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"neuralmail/internal/auth"
	"neuralmail/internal/config"
	"neuralmail/internal/store"
)

func TestRequiredToolScopePreservesLegacyAndSplitsM2M(t *testing.T) {
	legacy := auth.Principal{Kind: auth.PrincipalLegacyJWT}
	m2m := auth.Principal{Kind: auth.PrincipalM2MOrg}
	for _, test := range []struct {
		principal auth.Principal
		tool      string
		want      string
	}{
		{legacy, "send_reply", "nerve:email.send"},
		{legacy, "compose_email", "nerve:email.send"},
		{m2m, "send_reply", "nerve:email.reply"},
		{m2m, "compose_email", "nerve:email.compose"},
		{m2m, "get_thread", "nerve:email.read"},
	} {
		if got := requiredToolScope(test.principal, test.tool); got != test.want {
			t.Fatalf("%s/%s scope=%q want=%q", test.principal.Kind, test.tool, got, test.want)
		}
	}
}

func TestM2MOutboundInvocationFailsClosedWithoutLivePolicy(t *testing.T) {
	cfg := config.Default()
	cfg.Cloud.Mode = true
	authService := auth.NewService(cfg, nil)
	runtime := NewServer(cfg, nil, authService, nil)
	principal := auth.Principal{
		Kind: auth.PrincipalM2MOrg, OrgID: "org-1",
		Scopes: []string{"nerve:email.compose"},
	}
	_, err := runtime.Invoker.Invoke(auth.WithPrincipal(context.Background(), principal), ToolInvocation{
		Name: "compose_email", Arguments: json.RawMessage(`{}`),
	})
	var policyErr *outboundPolicyError
	if !errors.As(err, &policyErr) || policyErr.Code != "outbound_policy_unavailable" {
		t.Fatalf("compose did not fail closed without policy: %v", err)
	}
}

func TestStoreOutboundPolicyGateUsesExplicitLiveOrgFlags(t *testing.T) {
	trueValue := true
	falseValue := false
	principal := auth.Principal{Kind: auth.PrincipalM2MOrg, OrgID: "org-1"}
	allowed := &storeOutboundPolicyGate{store: fakeOutboundPolicyStore{values: map[string]store.FeatureFlagValues{
		"autonomous_outbound_policy": {Org: &trueValue},
		"email_outbound_suspended":   {Org: &falseValue},
		"email_compose_org_enabled":  {Org: &trueValue},
	}}}
	if err := allowed.Authorize(context.Background(), principal, "send_reply", nil); err != nil {
		t.Fatalf("explicit reply policy denied: %v", err)
	}
	if err := allowed.Authorize(context.Background(), principal, "compose_email", nil); err != nil {
		t.Fatalf("explicit compose policy denied: %v", err)
	}

	denied := &storeOutboundPolicyGate{store: fakeOutboundPolicyStore{values: map[string]store.FeatureFlagValues{
		"autonomous_outbound_policy": {Org: &trueValue},
		"email_outbound_suspended":   {Org: &trueValue},
		"email_compose_org_enabled":  {Org: &trueValue},
	}}}
	if err := denied.Authorize(context.Background(), principal, "compose_email", nil); err == nil {
		t.Fatal("suspended org compose was allowed")
	}
	missing := &storeOutboundPolicyGate{store: fakeOutboundPolicyStore{values: map[string]store.FeatureFlagValues{}}}
	if err := missing.Authorize(context.Background(), principal, "send_reply", nil); err == nil {
		t.Fatal("missing explicit policy was allowed")
	}
	failing := &storeOutboundPolicyGate{store: fakeOutboundPolicyStore{err: errors.New("database unavailable")}}
	var policyErr *outboundPolicyError
	if err := failing.Authorize(context.Background(), principal, "send_reply", nil); !errors.As(err, &policyErr) || policyErr.Code != "outbound_policy_unavailable" {
		t.Fatalf("policy read error did not fail closed: %v", err)
	}
}

func TestStoreOutboundPolicyGateAllowsOnlyReadyOwnedCustomDomain(t *testing.T) {
	trueValue := true
	falseValue := false
	principal := auth.Principal{Kind: auth.PrincipalM2MOrg, OrgID: "org-1"}
	base := fakeOutboundPolicyStore{
		values: map[string]store.FeatureFlagValues{
			"autonomous_outbound_policy": {Org: &trueValue},
			"email_outbound_suspended":   {Org: &falseValue},
			"email_compose_org_enabled":  {Org: &falseValue},
		},
		inboxes: map[string]store.InboxRecord{
			"inbox-1": {ID: "inbox-1", OrgID: "org-1", Address: "agent@example.com", Status: "active", OrgDomainID: sql.NullString{String: "domain-1", Valid: true}},
		},
		domains: map[string]store.OrgDomain{
			"domain-1": {ID: "domain-1", OrgID: "org-1", Domain: "example.com", Status: "active", MXVerified: true, SPFVerified: true, DKIMVerified: true, InboundEnabled: true, ResendReceivingEnabled: true},
		},
	}
	gate := &storeOutboundPolicyGate{store: base}
	arguments := json.RawMessage(`{"inbox_id":"inbox-1"}`)
	if err := gate.Authorize(context.Background(), principal, "compose_email", arguments); err != nil {
		t.Fatalf("ready owned custom domain denied: %v", err)
	}
	if err := gate.Authorize(context.Background(), principal, "compose_email", nil); err != nil {
		t.Fatalf("catalog hid compose for ready owned custom domain: %v", err)
	}

	notReady := base
	notReady.domains = map[string]store.OrgDomain{
		"domain-1": {ID: "domain-1", OrgID: "org-1", Domain: "example.com", Status: "active", SPFVerified: true},
	}
	if err := (&storeOutboundPolicyGate{store: notReady}).Authorize(context.Background(), principal, "compose_email", arguments); err == nil {
		t.Fatal("custom domain without complete sending verification was allowed")
	}

	platformOwned := base
	platformOwned.domains = map[string]store.OrgDomain{}
	if err := (&storeOutboundPolicyGate{store: platformOwned}).Authorize(context.Background(), principal, "compose_email", arguments); err == nil {
		t.Fatal("platform-owned mailbox granted custom-domain compose")
	}
}

func TestStoreOutboundPolicyGateRequiresLatestRealInboundReply(t *testing.T) {
	trueValue := true
	falseValue := false
	principal := auth.Principal{Kind: auth.PrincipalM2MOrg, OrgID: "org-1"}
	base := fakeOutboundPolicyStore{
		values: map[string]store.FeatureFlagValues{
			"autonomous_outbound_policy": {Org: &trueValue},
			"email_outbound_suspended":   {Org: &falseValue},
		},
		inboxes: map[string]store.InboxRecord{
			"inbox-1": {ID: "inbox-1", OrgID: "org-1", Status: "active"},
		},
		threads: map[string]fakeThreadRecord{
			"thread-1": {
				thread:   store.Thread{ID: "thread-1", InboxID: "inbox-1"},
				messages: []store.Message{{ID: "message-1", ThreadID: "thread-1", InboxID: "inbox-1", Direction: "inbound"}},
			},
		},
		messages: map[string]store.Message{
			"message-1": {ID: "message-1", ThreadID: "thread-1", InboxID: "inbox-1", Direction: "inbound", ReceivedEmailID: "received-1", From: store.Participant{Email: "sender@example.net"}},
		},
	}
	gate := &storeOutboundPolicyGate{store: base}
	arguments := json.RawMessage(`{"thread_id":"thread-1"}`)
	if err := gate.Authorize(context.Background(), principal, "send_reply", arguments); err != nil {
		t.Fatalf("real inbound reply denied: %v", err)
	}
	if err := gate.Authorize(context.Background(), principal, "send_reply", nil); err != nil {
		t.Fatalf("catalog hid reply before a thread was selected: %v", err)
	}

	latestOutbound := base
	latestOutbound.threads = map[string]fakeThreadRecord{
		"thread-1": {
			thread: store.Thread{ID: "thread-1", InboxID: "inbox-1"},
			messages: []store.Message{
				{ID: "message-1", ThreadID: "thread-1", InboxID: "inbox-1", Direction: "inbound"},
				{ID: "message-2", ThreadID: "thread-1", InboxID: "inbox-1", Direction: "outbound"},
			},
		},
	}
	if err := (&storeOutboundPolicyGate{store: latestOutbound}).Authorize(context.Background(), principal, "send_reply", arguments); err != nil {
		t.Fatalf("reply after a newer outbound message was denied: %v", err)
	}

	notReal := base
	notReal.messages = map[string]store.Message{
		"message-1": {ID: "message-1", ThreadID: "thread-1", InboxID: "inbox-1", Direction: "inbound", From: store.Participant{Email: "sender@example.net"}},
	}
	if err := (&storeOutboundPolicyGate{store: notReal}).Authorize(context.Background(), principal, "send_reply", arguments); err == nil {
		t.Fatal("inbound row without receiving evidence was allowed")
	}
}

type fakeThreadRecord struct {
	thread   store.Thread
	messages []store.Message
}

type fakeOutboundPolicyStore struct {
	values   map[string]store.FeatureFlagValues
	inboxes  map[string]store.InboxRecord
	domains  map[string]store.OrgDomain
	threads  map[string]fakeThreadRecord
	messages map[string]store.Message
	err      error
}

func (fake fakeOutboundPolicyStore) GetInboxRecordByIDForOrg(_ context.Context, orgID, inboxID string) (store.InboxRecord, error) {
	inbox, ok := fake.inboxes[inboxID]
	if !ok || inbox.OrgID != orgID {
		return store.InboxRecord{}, sql.ErrNoRows
	}
	return inbox, nil
}

func (fake fakeOutboundPolicyStore) ListInboxRecordsByOrg(_ context.Context, orgID string) ([]store.InboxRecord, error) {
	if fake.err != nil {
		return nil, fake.err
	}
	var inboxes []store.InboxRecord
	for _, inbox := range fake.inboxes {
		if inbox.OrgID == orgID {
			inboxes = append(inboxes, inbox)
		}
	}
	return inboxes, nil
}

func (fake fakeOutboundPolicyStore) GetOrgDomainByIDForOrg(_ context.Context, orgID, domainID string) (store.OrgDomain, error) {
	domain, ok := fake.domains[domainID]
	if !ok || domain.OrgID != orgID {
		return store.OrgDomain{}, sql.ErrNoRows
	}
	return domain, nil
}

func (fake fakeOutboundPolicyStore) GetThread(_ context.Context, threadID string) (store.Thread, []store.Message, error) {
	record, ok := fake.threads[threadID]
	if !ok {
		return store.Thread{}, nil, sql.ErrNoRows
	}
	return record.thread, record.messages, nil
}

func (fake fakeOutboundPolicyStore) GetMessage(_ context.Context, messageID string) (store.Message, error) {
	message, ok := fake.messages[messageID]
	if !ok {
		return store.Message{}, sql.ErrNoRows
	}
	return message, nil
}

func (fake fakeOutboundPolicyStore) LookupFeatureFlagForOrg(_ context.Context, _ string, flag string) (store.FeatureFlagValues, error) {
	if fake.err != nil {
		return store.FeatureFlagValues{}, fake.err
	}
	return fake.values[flag], nil
}
