package tools

import (
	"context"
	"strings"

	"neuralmail/internal/domains"
	"neuralmail/internal/emailaddr"
	"neuralmail/internal/store"
)

// OutboundPolicyError carries the exact denial reason to the protocol boundary.
type OutboundPolicyError struct {
	Code string
}

func (err *OutboundPolicyError) Error() string {
	return err.Code
}

// OutboundPolicyStore is the read surface the decision needs.
//
// Every method must execute on the caller's own executor. LookupFeatureFlag is
// deliberately the transaction-local reader rather than LookupFeatureFlagForOrg,
// which opens its own RunAsOrg transaction: called from inside the enqueue
// transaction that variant would read a different snapshot on a second pooled
// connection, leaving the race open and risking pool exhaustion under
// concurrent sends. *store.Store satisfies this interface.
type OutboundPolicyStore interface {
	LockOrgPolicy(context.Context, string) error
	LookupFeatureFlag(context.Context, string, string) (store.FeatureFlagValues, error)
	GetInboxRecordByIDForOrg(context.Context, string, string) (store.InboxRecord, error)
	ListInboxRecordsByOrg(context.Context, string) ([]store.InboxRecord, error)
	GetOrgDomainByIDForOrg(context.Context, string, string) (store.OrgDomain, error)
	GetThread(context.Context, string) (store.Thread, []store.Message, error)
	GetMessage(context.Context, string) (store.Message, error)
}

// OutboundPolicyInput identifies what is about to be sent. An empty ThreadID or
// InboxID means the caller is probing capability rather than enqueueing a
// specific message, which is how tool listing asks whether the tool is offered
// at all.
type OutboundPolicyInput struct {
	Tool     string
	OrgID    string
	ThreadID string
	InboxID  string
}

// EvaluateOutboundPolicy decides whether an autonomous org may send.
//
// Every failure denies: a missing flag row, a malformed value, or a read error
// are all treated as "not allowed" rather than "not known".
func EvaluateOutboundPolicy(
	ctx context.Context, st OutboundPolicyStore, input OutboundPolicyInput,
) error {
	if st == nil || input.OrgID == "" {
		return &OutboundPolicyError{Code: "outbound_policy_unavailable"}
	}
	// Serialize against concurrent policy writers before reading, so a
	// suspension either is already visible here or waits for this transaction.
	if err := st.LockOrgPolicy(ctx, input.OrgID); err != nil {
		return &OutboundPolicyError{Code: "outbound_policy_unavailable"}
	}
	required := []struct {
		flag string
		want bool
	}{
		{flag: "autonomous_outbound_policy", want: true},
		{flag: "email_outbound_suspended", want: false},
	}
	for _, requirement := range required {
		values, err := st.LookupFeatureFlag(ctx, input.OrgID, requirement.flag)
		if err != nil {
			return &OutboundPolicyError{Code: "outbound_policy_unavailable"}
		}
		if values.Org == nil || *values.Org != requirement.want {
			return &OutboundPolicyError{Code: requirement.flag + "_denied"}
		}
	}
	switch input.Tool {
	case "send_reply":
		if input.ThreadID == "" {
			return nil
		}
		allowed, err := hasRealInboundReplyTarget(ctx, st, input.OrgID, input.ThreadID)
		if err != nil {
			return &OutboundPolicyError{Code: "outbound_policy_unavailable"}
		}
		if !allowed {
			return &OutboundPolicyError{Code: "inbound_reply_policy_denied"}
		}
	case "compose_email":
		values, err := st.LookupFeatureFlag(ctx, input.OrgID, "email_compose_org_enabled")
		if err != nil || values.Org == nil {
			return &OutboundPolicyError{Code: "outbound_policy_unavailable"}
		}
		if *values.Org {
			return nil
		}
		allowed, err := hasReadyOwnedComposeInbox(ctx, st, input.OrgID, input.InboxID)
		if err != nil {
			return &OutboundPolicyError{Code: "outbound_policy_unavailable"}
		}
		if !allowed {
			return &OutboundPolicyError{Code: "email_compose_org_enabled_denied"}
		}
	}
	return nil
}

func hasReadyOwnedComposeInbox(
	ctx context.Context, st OutboundPolicyStore, orgID, inboxID string,
) (bool, error) {
	var inboxes []store.InboxRecord
	if inboxID == "" {
		var err error
		inboxes, err = st.ListInboxRecordsByOrg(ctx, orgID)
		if err != nil {
			return false, err
		}
	} else {
		inbox, err := st.GetInboxRecordByIDForOrg(ctx, orgID, inboxID)
		if err != nil {
			return false, err
		}
		inboxes = []store.InboxRecord{inbox}
	}

	for _, inbox := range inboxes {
		if inbox.Status != "active" || !inbox.OrgDomainID.Valid {
			continue
		}
		domain, err := st.GetOrgDomainByIDForOrg(ctx, orgID, inbox.OrgDomainID.String)
		if err != nil {
			continue
		}
		_, _, inboxDomain, addressErr := emailaddr.Canonicalize(inbox.Address)
		canonicalDomain, domainErr := domains.CanonicalizeDomain(domain.Domain)
		if domain.Status == "active" && domain.MXVerified && domain.SPFVerified && domain.DKIMVerified &&
			domain.InboundEnabled && domain.ResendReceivingEnabled &&
			addressErr == nil && domainErr == nil && inboxDomain == canonicalDomain {
			return true, nil
		}
	}
	return false, nil
}

// AutonomousComposeAllowed reports which V1 abuse-limit family applies to a
// send. It uses the same explicit paid projection and live owned-domain proof
// as compose authorization, on the caller's transaction-scoped snapshot.
func AutonomousComposeAllowed(
	ctx context.Context, st OutboundPolicyStore, orgID, inboxID string,
) (bool, error) {
	values, err := st.LookupFeatureFlag(ctx, orgID, "email_compose_org_enabled")
	if err != nil || values.Org == nil {
		return false, &OutboundPolicyError{Code: "outbound_policy_unavailable"}
	}
	if *values.Org {
		return true, nil
	}
	return hasReadyOwnedComposeInbox(ctx, st, orgID, inboxID)
}

func hasRealInboundReplyTarget(
	ctx context.Context, st OutboundPolicyStore, orgID, threadID string,
) (bool, error) {
	thread, messages, err := st.GetThread(ctx, threadID)
	if err != nil {
		return false, err
	}
	if len(messages) == 0 {
		return false, nil
	}
	inbox, err := st.GetInboxRecordByIDForOrg(ctx, orgID, thread.InboxID)
	if err != nil {
		return false, err
	}
	if inbox.Status != "active" {
		return false, nil
	}
	for index := len(messages) - 1; index >= 0; index-- {
		candidate := messages[index]
		if candidate.Direction != "inbound" || candidate.InboxID != thread.InboxID {
			continue
		}
		message, err := st.GetMessage(ctx, candidate.ID)
		if err != nil {
			return false, err
		}
		if message.ThreadID == thread.ID && message.InboxID == thread.InboxID &&
			message.Direction == "inbound" && message.ReceivedEmailID != "" &&
			strings.TrimSpace(message.From.Email) != "" {
			return true, nil
		}
	}
	return false, nil
}
