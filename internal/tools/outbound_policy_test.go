package tools

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"neuralmail/internal/auth"
	"neuralmail/internal/config"
	"neuralmail/internal/store"
)

type policyStoreStub struct {
	flags      map[string]*bool
	flagErr    error
	flagReads  map[string]int
	inbox      store.InboxRecord
	inboxErr   error
	inboxes    []store.InboxRecord
	domain     store.OrgDomain
	thread     store.Thread
	messages   []store.Message
	threadErr  error
	message    store.Message
	messageErr error
	locks      int
	lockErr    error
}

func (stub *policyStoreStub) LockOrgPolicy(_ context.Context, _ string) error {
	stub.locks++
	return stub.lockErr
}

func (stub *policyStoreStub) LookupFeatureFlag(
	_ context.Context, _ string, flag string,
) (store.FeatureFlagValues, error) {
	if stub.flagReads == nil {
		stub.flagReads = map[string]int{}
	}
	stub.flagReads[flag]++
	if stub.flagErr != nil {
		return store.FeatureFlagValues{}, stub.flagErr
	}
	return store.FeatureFlagValues{Org: stub.flags[flag]}, nil
}

func (stub *policyStoreStub) GetInboxRecordByIDForOrg(
	_ context.Context, _ string, _ string,
) (store.InboxRecord, error) {
	return stub.inbox, stub.inboxErr
}

func (stub *policyStoreStub) ListInboxRecordsByOrg(
	_ context.Context, _ string,
) ([]store.InboxRecord, error) {
	return stub.inboxes, nil
}

func (stub *policyStoreStub) GetOrgDomainByIDForOrg(
	_ context.Context, _ string, _ string,
) (store.OrgDomain, error) {
	return stub.domain, nil
}

func (stub *policyStoreStub) GetThread(
	_ context.Context, _ string,
) (store.Thread, []store.Message, error) {
	return stub.thread, stub.messages, stub.threadErr
}

func (stub *policyStoreStub) GetMessage(_ context.Context, _ string) (store.Message, error) {
	return stub.message, stub.messageErr
}

func boolPtr(value bool) *bool { return &value }

func allowedFlags() map[string]*bool {
	return map[string]*bool{
		"autonomous_outbound_policy": boolPtr(true),
		"email_outbound_suspended":   boolPtr(false),
		"email_compose_org_enabled":  boolPtr(true),
	}
}

func policyCode(t *testing.T, err error) string {
	t.Helper()
	var policyErr *OutboundPolicyError
	if !errors.As(err, &policyErr) {
		t.Fatalf("expected an outbound policy error, got %v", err)
	}
	return policyErr.Code
}

func TestOutboundPolicyDeniesOnMissingMalformedOrUnreadableRows(t *testing.T) {
	cases := []struct {
		name  string
		flags map[string]*bool
		err   error
		want  string
	}{
		{
			name:  "missing autonomous row",
			flags: map[string]*bool{"email_outbound_suspended": boolPtr(false)},
			want:  "autonomous_outbound_policy_denied",
		},
		{
			name: "suspended after the boundary check",
			flags: map[string]*bool{
				"autonomous_outbound_policy": boolPtr(true),
				"email_outbound_suspended":   boolPtr(true),
			},
			want: "email_outbound_suspended_denied",
		},
		{
			name:  "unreadable flag",
			flags: allowedFlags(),
			err:   errors.New("connection reset"),
			want:  "outbound_policy_unavailable",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			stub := &policyStoreStub{flags: testCase.flags, flagErr: testCase.err}
			err := EvaluateOutboundPolicy(context.Background(), stub, OutboundPolicyInput{
				Tool: "compose_email", OrgID: "org-1", InboxID: "inbox-1",
			})
			if got := policyCode(t, err); got != testCase.want {
				t.Fatalf("expected %q, got %q", testCase.want, got)
			}
		})
	}
}

func TestOutboundPolicyDeniesWithoutStoreOrOrg(t *testing.T) {
	if got := policyCode(t, EvaluateOutboundPolicy(
		context.Background(), nil, OutboundPolicyInput{Tool: "send_reply", OrgID: "org-1"},
	)); got != "outbound_policy_unavailable" {
		t.Fatalf("nil store must deny, got %q", got)
	}
	if got := policyCode(t, EvaluateOutboundPolicy(
		context.Background(), &policyStoreStub{flags: allowedFlags()}, OutboundPolicyInput{Tool: "send_reply"},
	)); got != "outbound_policy_unavailable" {
		t.Fatalf("missing org must deny, got %q", got)
	}
}

func TestOutboundPolicyDeniesReplyWithoutRealInboundTarget(t *testing.T) {
	stub := &policyStoreStub{
		flags:  allowedFlags(),
		thread: store.Thread{ID: "thread-1", InboxID: "inbox-1"},
		messages: []store.Message{
			{ID: "message-1", InboxID: "inbox-1", Direction: "outbound"},
		},
		inbox: store.InboxRecord{Status: "active"},
	}

	err := EvaluateOutboundPolicy(context.Background(), stub, OutboundPolicyInput{
		Tool: "send_reply", OrgID: "org-1", ThreadID: "thread-1",
	})

	if got := policyCode(t, err); got != "inbound_reply_policy_denied" {
		t.Fatalf("expected inbound_reply_policy_denied, got %q", got)
	}
}

func TestOutboundPolicyAllowsReplyToDurableInboundMessage(t *testing.T) {
	stub := &policyStoreStub{
		flags:  allowedFlags(),
		thread: store.Thread{ID: "thread-1", InboxID: "inbox-1"},
		messages: []store.Message{
			{ID: "message-1", InboxID: "inbox-1", Direction: "inbound"},
		},
		inbox: store.InboxRecord{Status: "active"},
		message: store.Message{
			ID: "message-1", ThreadID: "thread-1", InboxID: "inbox-1", Direction: "inbound",
			ReceivedEmailID: "received-1", From: store.Participant{Email: "sender@example.test"},
		},
	}

	if err := EvaluateOutboundPolicy(context.Background(), stub, OutboundPolicyInput{
		Tool: "send_reply", OrgID: "org-1", ThreadID: "thread-1",
	}); err != nil {
		t.Fatalf("expected the reply to be allowed, got %v", err)
	}
}

func TestOutboundPolicyDeniesComposeWithoutReadyOwnedDomain(t *testing.T) {
	flags := allowedFlags()
	flags["email_compose_org_enabled"] = boolPtr(false)
	stub := &policyStoreStub{
		flags:  flags,
		inbox:  store.InboxRecord{Status: "active", Address: "agent@example.test"},
		domain: store.OrgDomain{Domain: "example.test", Status: "pending"},
	}

	err := EvaluateOutboundPolicy(context.Background(), stub, OutboundPolicyInput{
		Tool: "compose_email", OrgID: "org-1", InboxID: "inbox-1",
	})

	if got := policyCode(t, err); got != "email_compose_org_enabled_denied" {
		t.Fatalf("expected email_compose_org_enabled_denied, got %q", got)
	}
}

// The enqueue transaction must ask again rather than trust the boundary answer.
func TestServiceReevaluatesPolicyForAutonomousPrincipals(t *testing.T) {
	cfg := config.Default()
	cfg.Cloud.Mode = true
	svc := &Service{Config: cfg}
	flags := allowedFlags()
	flags["email_outbound_suspended"] = boolPtr(true)
	stub := &policyStoreStub{flags: flags}

	err := svc.reevaluateOutboundPolicy(
		context.Background(), stub,
		auth.Principal{Kind: auth.PrincipalM2MOrg, OrgID: "org-1"},
		OutboundPolicyInput{Tool: "compose_email", InboxID: "inbox-1"},
	)

	if got := policyCode(t, err); got != "email_outbound_suspended_denied" {
		t.Fatalf("expected the suspension to deny the enqueue, got %q", got)
	}
	if stub.flagReads["email_outbound_suspended"] != 1 {
		t.Fatalf("expected exactly one in-transaction suspension read, got %d",
			stub.flagReads["email_outbound_suspended"])
	}
}

func TestServiceSkipsPolicyForNonAutonomousPrincipals(t *testing.T) {
	cfg := config.Default()
	cfg.Cloud.Mode = true
	svc := &Service{Config: cfg}
	stub := &policyStoreStub{flagErr: sql.ErrConnDone}

	if err := svc.reevaluateOutboundPolicy(
		context.Background(), stub,
		auth.Principal{Kind: auth.PrincipalCloudAPIKey, OrgID: "org-1"},
		OutboundPolicyInput{Tool: "send_reply", ThreadID: "thread-1"},
	); err != nil {
		t.Fatalf("legacy principals keep their existing path, got %v", err)
	}
	if len(stub.flagReads) != 0 {
		t.Fatalf("expected no policy reads for a legacy principal, got %v", stub.flagReads)
	}
}

func TestOutboundPolicyLocksTheOrgBeforeReadingFlags(t *testing.T) {
	stub := &policyStoreStub{flags: allowedFlags()}

	if err := EvaluateOutboundPolicy(context.Background(), stub, OutboundPolicyInput{
		Tool: "compose_email", OrgID: "org-1", InboxID: "inbox-1",
	}); err != nil {
		t.Fatalf("expected the send to be allowed, got %v", err)
	}

	// Without the lock a suspension could commit between the read and the
	// insert, so the lock is taken before any flag is read.
	if stub.locks != 1 {
		t.Fatalf("expected exactly one org policy lock, got %d", stub.locks)
	}
}

func TestOutboundPolicyDeniesWhenTheOrgCannotBeLocked(t *testing.T) {
	stub := &policyStoreStub{flags: allowedFlags(), lockErr: errors.New("not in a transaction")}

	err := EvaluateOutboundPolicy(context.Background(), stub, OutboundPolicyInput{
		Tool: "send_reply", OrgID: "org-1", ThreadID: "thread-1",
	})

	if got := policyCode(t, err); got != "outbound_policy_unavailable" {
		t.Fatalf("expected outbound_policy_unavailable, got %q", got)
	}
	if len(stub.flagReads) != 0 {
		t.Fatalf("expected no flag read without the lock, got %v", stub.flagReads)
	}
}

func TestLoadSchemaRejectsTraversalOutsideTheSchemaDirectory(t *testing.T) {
	for _, schemaID := range []string{
		"../../../../etc/passwd",
		"../secrets",
		"nested/schema",
		".hidden",
		"UPPER",
	} {
		if _, err := LoadSchema(schemaID); err == nil || err.Error() != "invalid schema id" {
			t.Fatalf("schema id %q must be rejected, got %v", schemaID, err)
		}
	}
}
