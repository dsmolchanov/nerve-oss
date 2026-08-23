package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"neuralmail/internal/domains"

	"github.com/google/uuid"
)

// LegacyDomainProviderOperation names the external side effect/readback that a
// legacy-domain lease authorizes. The operation is encoded in lease_owner so
// Cloud 0009 can fence provider I/O without another schema transition.
type LegacyDomainProviderOperation string

const (
	LegacyDomainProviderCreate           LegacyDomainProviderOperation = "create"
	LegacyDomainProviderGet              LegacyDomainProviderOperation = "get"
	LegacyDomainProviderVerify           LegacyDomainProviderOperation = "verify"
	LegacyDomainProviderReceivingEnable  LegacyDomainProviderOperation = "receiving_enable"
	LegacyDomainProviderReceivingDisable LegacyDomainProviderOperation = "receiving_disable"
	LegacyDomainProviderDelete           LegacyDomainProviderOperation = "delete"
)

const legacyResendDomainIDMaxBytes = 256

// LegacyDomainProviderDisposition tells orchestration whether Begin authorized
// an external call. Waiting dispositions are durable successful states, not an
// invitation to retry a non-idempotent create.
type LegacyDomainProviderDisposition string

const (
	LegacyDomainProviderCallAuthorized LegacyDomainProviderDisposition = "provider_call"
	LegacyDomainProviderLocalOnly      LegacyDomainProviderDisposition = "local_only"
	LegacyDomainProviderAwaitingProof  LegacyDomainProviderDisposition = "awaiting_provider_proof"
	LegacyDomainProviderInFlight       LegacyDomainProviderDisposition = "operation_in_progress"
	LegacyDomainProviderMaterialized   LegacyDomainProviderDisposition = "already_materialized"
)

// LegacyDomainProviderOutcome is the bounded result classification accepted by
// ApplyLegacyDomainProviderResult. Provider absence is deliberately excluded:
// local deletion has a separate exact-ID receipt API below.
type LegacyDomainProviderOutcome string

const (
	LegacyDomainProviderObserved LegacyDomainProviderOutcome = "observed"
	LegacyDomainProviderUnknown  LegacyDomainProviderOutcome = "unknown"
)

// LegacyDomainAbsenceProof is intentionally a closed enum. A successful DELETE
// response is not absence proof. Exact-ID readback is required after a
// materialized provider object; the sole identity-free proof is a validation
// rejection that occurred before create materialization.
type LegacyDomainAbsenceProof string

const (
	LegacyDomainExactIDAuthoritativeAbsence         LegacyDomainAbsenceProof = "exact_id_authoritative_absence"
	LegacyDomainCreateRejectedBeforeMaterialization LegacyDomainAbsenceProof = "create_rejected_before_materialization"
)

var (
	ErrLegacyDomainProviderCASConflict     = errors.New("legacy domain provider lifecycle CAS conflict")
	ErrLegacyDomainProviderIdentityNeeded  = errors.New("legacy domain provider identity is required")
	ErrLegacyDomainProviderAbsenceRequired = errors.New("authoritative provider absence proof is required")
	ErrLegacyDomainProviderResultInvalid   = errors.New("invalid legacy domain provider result")
)

// LegacyDomainProviderBegin identifies one local domain and asks Store to
// persist a provider intent. LeaseTTL is bounded and defaults to one minute.
type LegacyDomainProviderBegin struct {
	OrgID       string
	OrgDomainID string
	Operation   LegacyDomainProviderOperation
	LeaseTTL    time.Duration
}

// LegacyDomainProviderIntent is an exact post-Begin snapshot. Callers must pass
// it back unchanged. Store checks every identity/state/version/lease member
// before applying an observation.
type LegacyDomainProviderIntent struct {
	Operation             LegacyDomainProviderOperation
	Disposition           LegacyDomainProviderDisposition
	OrgID                 string
	OrgDomainID           string
	StoredDomain          string
	CanonicalDomain       string
	DomainState           string
	DomainUpdatedAt       time.Time
	ClaimState            string
	WorkflowVersion       int64
	LeaseOwner            string
	LeaseExpiresAt        time.Time
	ProviderDomainID      string
	ProviderStatus        string
	ProviderStatusPresent bool
	ProviderAttempted     bool
}

// LegacyDomainProviderResult carries only bounded provider projections. Nil
// boolean fields and an empty DomainState preserve the current local value.
type LegacyDomainProviderResult struct {
	Intent                  LegacyDomainProviderIntent
	Outcome                 LegacyDomainProviderOutcome
	ProviderDomainID        string
	ProviderCanonicalDomain string
	ProviderStatus          string
	DNSRecords              json.RawMessage
	DomainState             string
	MXVerified              *bool
	SPFVerified             *bool
	DKIMVerified            *bool
	DMARCVerified           *bool
	ReceivingEnabled        *bool
}

type LegacyDomainProviderApplyResult struct {
	Domain                      OrgDomain
	StaleCreateCaptured         bool
	CompensatingCleanupRequired bool
}

type LegacyDomainProviderAbsenceReceipt struct {
	Intent           LegacyDomainProviderIntent
	ProviderDomainID string
	Proof            LegacyDomainAbsenceProof
}

type LegacyDomainCreateRejectionReceipt struct {
	Intent LegacyDomainProviderIntent
	Proof  LegacyDomainAbsenceProof
}

type LegacyDomainCleanupCandidate struct {
	OrgID             string
	OrgDomainID       string
	CanonicalDomain   string
	DomainUpdatedAt   time.Time
	WorkflowVersion   int64
	LeaseOwner        sql.NullString
	LeaseExpiresAt    sql.NullTime
	CleanupNotBefore  sql.NullTime
	ProviderDomainID  sql.NullString
	HasOpenQuarantine bool
}

// LegacyDomainCleanupClaim is the exact result of claiming one discovery
// candidate. Provider-backed and local-only work reuse the ordinary Delete
// intent lease. AwaitingProof work instead owns CleanupNotBefore until that
// timestamp, preserving any expired Create lease needed to capture a late
// provider result.
type LegacyDomainCleanupClaim struct {
	Intent           LegacyDomainProviderIntent
	CleanupNotBefore time.Time
}

const legacyDomainProviderLeasePrefix = "legacy-domain-provider:"

const legacyDomainCleanupMaxDelay = 15 * time.Minute

type lockedLegacyDomainProviderState struct {
	Domain OrgDomain
	Claim  DomainOwnershipClaim
	Now    time.Time
}

func validLegacyDomainProviderOperation(operation LegacyDomainProviderOperation) bool {
	switch operation {
	case LegacyDomainProviderCreate,
		LegacyDomainProviderGet,
		LegacyDomainProviderVerify,
		LegacyDomainProviderReceivingEnable,
		LegacyDomainProviderReceivingDisable,
		LegacyDomainProviderDelete:
		return true
	default:
		return false
	}
}

func normalizeLegacyDomainProviderLeaseTTL(ttl time.Duration) (time.Duration, error) {
	if ttl == 0 {
		return time.Minute, nil
	}
	if ttl < time.Second || ttl > 15*time.Minute {
		return 0, errors.New("legacy domain provider lease ttl must be between one second and fifteen minutes")
	}
	return ttl, nil
}

func makeLegacyDomainProviderLeaseOwner(operation LegacyDomainProviderOperation) string {
	return legacyDomainProviderLeasePrefix + string(operation) + ":" + uuid.NewString()
}

func legacyDomainProviderLeaseOperation(owner string) (LegacyDomainProviderOperation, bool) {
	if owner == "" || owner != strings.TrimSpace(owner) {
		return "", false
	}
	remainder := strings.TrimPrefix(owner, legacyDomainProviderLeasePrefix)
	if remainder == owner {
		return "", false
	}
	name, rawID, ok := strings.Cut(remainder, ":")
	if !ok {
		return "", false
	}
	operation := LegacyDomainProviderOperation(name)
	parsedID, err := uuid.Parse(rawID)
	if err != nil || rawID != parsedID.String() || !validLegacyDomainProviderOperation(operation) {
		return "", false
	}
	return operation, true
}

func legacyDomainProviderMaterialEvidence(domain OrgDomain) bool {
	// Valid-but-malformed persisted values are evidence too. Treating a blank
	// provider ID/status as pristine could release a claim after partial or
	// corrupt provider projection, so every present provider column fails closed.
	return domain.ResendDomainID.Valid || domain.ResendStatus.Valid ||
		len(domain.ResendDNSRecords) != 0 || domain.ResendReceivingEnabled || domain.InboundEnabled
}

func legacyDomainProviderEvidenceAttempted(domain OrgDomain, claim DomainOwnershipClaim) bool {
	return legacyDomainProviderMaterialEvidence(domain) || claim.LeaseOwner.Valid || claim.LeaseExpiresAt.Valid
}

func legacyDomainRecoverableLocalOnlyDeleteLease(domain OrgDomain, claim DomainOwnershipClaim) bool {
	operation, known := legacyDomainProviderLeaseOperation(claim.LeaseOwner.String)
	return claim.State == "releasing" && domain.Status == "failed" &&
		claim.LeaseOwner.Valid && claim.LeaseExpiresAt.Valid && !claim.LeaseExpiresAt.Time.IsZero() &&
		known && operation == LegacyDomainProviderDelete && !legacyDomainProviderMaterialEvidence(domain)
}

func nullableStringValue(value sql.NullString) any {
	if !value.Valid {
		return nil
	}
	return value.String
}

func nullableTimeValue(value sql.NullTime) any {
	if !value.Valid {
		return nil
	}
	return value.Time
}

func legacyDomainProviderLeaseIsLive(claim DomainOwnershipClaim, now time.Time) bool {
	return claim.LeaseOwner.Valid && claim.LeaseExpiresAt.Valid && claim.LeaseExpiresAt.Time.After(now)
}

func (s *Store) lockLegacyDomainProviderState(
	ctx context.Context,
	orgID string,
	domainID string,
	expectedCanonical string,
) (lockedLegacyDomainProviderState, error) {
	if err := s.requireTx(); err != nil {
		return lockedLegacyDomainProviderState{}, err
	}
	if err := s.requireDomainWritesEnabled(ctx); err != nil {
		return lockedLegacyDomainProviderState{}, err
	}
	orgID = strings.TrimSpace(orgID)
	domainID = strings.TrimSpace(domainID)
	if _, err := uuid.Parse(orgID); err != nil {
		return lockedLegacyDomainProviderState{}, errors.New("invalid org id")
	}
	if _, err := uuid.Parse(domainID); err != nil {
		return lockedLegacyDomainProviderState{}, errors.New("invalid org domain id")
	}

	// Discover only the canonical advisory-lock key. The domain and claim are
	// re-read and row-locked after the required domain-writes -> canonical ->
	// org-policy prefix has been acquired.
	var discoveredDomain string
	if err := s.q.QueryRowContext(ctx, `
		SELECT domain
		FROM org_domains
		WHERE id=$1::uuid AND org_id=$2::uuid
	`, domainID, orgID).Scan(&discoveredDomain); err != nil {
		return lockedLegacyDomainProviderState{}, err
	}
	canonical, err := domains.CanonicalizeDomain(discoveredDomain)
	if err != nil {
		return lockedLegacyDomainProviderState{}, fmt.Errorf("canonicalize stored legacy domain: %w", err)
	}
	if expectedCanonical != "" {
		expectedCanonical, err = domains.CanonicalizeDomain(expectedCanonical)
		if err != nil || expectedCanonical != canonical {
			return lockedLegacyDomainProviderState{}, ErrLegacyDomainProviderCASConflict
		}
	}
	if err := s.lockCanonicalDomain(ctx, canonical); err != nil {
		return lockedLegacyDomainProviderState{}, err
	}
	if err := s.LockOrgPolicy(ctx, orgID); err != nil {
		return lockedLegacyDomainProviderState{}, err
	}
	var activeOrg bool
	if err := s.q.QueryRowContext(ctx, `
		SELECT true
		FROM orgs
		WHERE id=$1::uuid AND coalesce(to_jsonb(orgs)->>'deleted_at', '')=''
		FOR SHARE
	`, orgID).Scan(&activeOrg); err != nil {
		return lockedLegacyDomainProviderState{}, err
	}
	if !activeOrg {
		return lockedLegacyDomainProviderState{}, sql.ErrNoRows
	}

	var state lockedLegacyDomainProviderState
	if err := scanOrgDomain(s.q.QueryRowContext(ctx, `
		SELECT id, org_id, domain, status, verification_token,
		       mx_verified, spf_verified, dkim_verified, dmarc_verified,
		       inbound_enabled, dkim_selector, dkim_private_key_enc, dkim_public_key,
		       dkim_method, last_check_at, verified_at, expires_at,
		       resend_domain_id, resend_domain_status, resend_dns_records,
		       resend_receiving_enabled, catch_all_enabled, forward_to,
		       created_at, updated_at, to_jsonb(org_domains)->>'external_ref'
		FROM org_domains
		WHERE id=$1::uuid AND org_id=$2::uuid
		FOR UPDATE
	`, domainID, orgID), &state.Domain); err != nil {
		return lockedLegacyDomainProviderState{}, err
	}
	lockedCanonical, err := domains.CanonicalizeDomain(state.Domain.Domain)
	if err != nil || lockedCanonical != canonical {
		return lockedLegacyDomainProviderState{}, ErrLegacyDomainProviderCASConflict
	}
	schema9, err := s.cloudSchemaSupportsM2M(ctx)
	if err != nil {
		return lockedLegacyDomainProviderState{}, err
	}
	if !schema9 {
		return lockedLegacyDomainProviderState{}, errors.New("legacy domain provider lifecycle requires Cloud schema 9")
	}
	if err := scanDomainOwnershipClaim(s.q.QueryRowContext(ctx, `
		SELECT canonical_domain, org_domain_id::text, org_id::text, onboarding_id::text,
		       owner_kind, state, workflow_version, lease_owner, lease_expires_at,
		       claim_expires_at, created_at, updated_at
		FROM domain_ownership_claims
		WHERE canonical_domain=$1
		FOR UPDATE
	`, canonical), &state.Claim); err != nil {
		return lockedLegacyDomainProviderState{}, err
	}
	if state.Claim.OrgDomainID != domainID || state.Claim.OrgID != orgID ||
		state.Claim.OwnerKind != "legacy" || state.Claim.OnboardingID.Valid {
		return lockedLegacyDomainProviderState{}, ErrLegacyDomainProviderCASConflict
	}
	if err := s.q.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&state.Now); err != nil {
		return lockedLegacyDomainProviderState{}, err
	}
	return state, nil
}

func legacyDomainProviderIntentFromState(
	operation LegacyDomainProviderOperation,
	disposition LegacyDomainProviderDisposition,
	state lockedLegacyDomainProviderState,
) LegacyDomainProviderIntent {
	intent := LegacyDomainProviderIntent{
		Operation: operation, Disposition: disposition,
		OrgID: state.Domain.OrgID, OrgDomainID: state.Domain.ID,
		StoredDomain: state.Domain.Domain, CanonicalDomain: state.Claim.CanonicalDomain,
		DomainState: state.Domain.Status, DomainUpdatedAt: state.Domain.UpdatedAt,
		ClaimState: state.Claim.State, WorkflowVersion: state.Claim.WorkflowVersion,
		ProviderAttempted: legacyDomainProviderEvidenceAttempted(state.Domain, state.Claim),
	}
	if state.Claim.LeaseOwner.Valid {
		intent.LeaseOwner = state.Claim.LeaseOwner.String
	}
	if state.Claim.LeaseExpiresAt.Valid {
		intent.LeaseExpiresAt = state.Claim.LeaseExpiresAt.Time
	}
	if state.Domain.ResendDomainID.Valid {
		intent.ProviderDomainID = state.Domain.ResendDomainID.String
	}
	if state.Domain.ResendStatus.Valid {
		intent.ProviderStatusPresent = true
		intent.ProviderStatus = state.Domain.ResendStatus.String
	}
	return intent
}

func (s *Store) legacyDomainHasReleaseDependents(ctx context.Context, domainID string) (bool, error) {
	var blocked bool
	err := s.q.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM inboxes WHERE org_domain_id=$1::uuid)
		    OR EXISTS(SELECT 1 FROM org_domain_grants WHERE org_domain_id=$1::uuid)
		    OR EXISTS(SELECT 1 FROM managed_mailbox_platform_domains WHERE org_domain_id=$1::uuid)
	`, domainID).Scan(&blocked)
	return blocked, err
}

// BeginLegacyDomainProviderOperation persists an exact versioned provider
// intent before any external I/O. A no-ID create writes provider_unknown before
// returning CallAuthorized, making blind recreation impossible after timeout.
func (s *Store) BeginLegacyDomainProviderOperation(
	ctx context.Context,
	request LegacyDomainProviderBegin,
) (LegacyDomainProviderIntent, error) {
	if !validLegacyDomainProviderOperation(request.Operation) {
		return LegacyDomainProviderIntent{}, errors.New("invalid legacy domain provider operation")
	}
	ttl, err := normalizeLegacyDomainProviderLeaseTTL(request.LeaseTTL)
	if err != nil {
		return LegacyDomainProviderIntent{}, err
	}
	var intent LegacyDomainProviderIntent
	err = s.withTx(ctx, func(scoped *Store) error {
		state, err := scoped.lockLegacyDomainProviderState(ctx, request.OrgID, request.OrgDomainID, "")
		if err != nil {
			return err
		}
		if request.Operation == LegacyDomainProviderDelete {
			intent, err = scoped.beginLegacyDomainDeleteLocked(ctx, state, ttl)
			return err
		}

		providerID := ""
		if state.Domain.ResendDomainID.Valid {
			providerID = state.Domain.ResendDomainID.String
		}
		leaseLive := legacyDomainProviderLeaseIsLive(state.Claim, state.Now)
		if leaseLive {
			intent = legacyDomainProviderIntentFromState(request.Operation, LegacyDomainProviderInFlight, state)
			return nil
		}
		if state.Domain.ResendDomainID.Valid &&
			!domains.IsExactProviderResourceID(providerID, legacyResendDomainIDMaxBytes) {
			intent = legacyDomainProviderIntentFromState(request.Operation, LegacyDomainProviderAwaitingProof, state)
			return nil
		}
		switch request.Operation {
		case LegacyDomainProviderCreate:
			if providerID != "" {
				if state.Claim.State == "releasing" || state.Domain.Status == "failed" {
					return ErrResourceConflict
				}
				intent = legacyDomainProviderIntentFromState(request.Operation, LegacyDomainProviderMaterialized, state)
				return nil
			}
			if state.Claim.State != "pending" || (state.Domain.Status != "pending" && state.Domain.Status != "provisioning") {
				return ErrResourceConflict
			}
			if legacyDomainProviderEvidenceAttempted(state.Domain, state.Claim) {
				intent = legacyDomainProviderIntentFromState(request.Operation, LegacyDomainProviderAwaitingProof, state)
				return nil
			}
		case LegacyDomainProviderGet:
			if providerID == "" {
				if legacyDomainProviderEvidenceAttempted(state.Domain, state.Claim) {
					intent = legacyDomainProviderIntentFromState(request.Operation, LegacyDomainProviderAwaitingProof, state)
					return nil
				}
				return ErrLegacyDomainProviderIdentityNeeded
			}
		case LegacyDomainProviderVerify, LegacyDomainProviderReceivingEnable:
			if state.Claim.State == "releasing" || state.Domain.Status == "failed" {
				return ErrResourceConflict
			}
			if providerID == "" {
				if legacyDomainProviderEvidenceAttempted(state.Domain, state.Claim) {
					intent = legacyDomainProviderIntentFromState(request.Operation, LegacyDomainProviderAwaitingProof, state)
					return nil
				}
				return ErrLegacyDomainProviderIdentityNeeded
			}
		case LegacyDomainProviderReceivingDisable:
			if providerID == "" {
				if legacyDomainProviderEvidenceAttempted(state.Domain, state.Claim) {
					intent = legacyDomainProviderIntentFromState(request.Operation, LegacyDomainProviderAwaitingProof, state)
					return nil
				}
				return ErrLegacyDomainProviderIdentityNeeded
			}
		}

		leaseOwner := makeLegacyDomainProviderLeaseOwner(request.Operation)
		previousLeaseOwner := nullableStringValue(state.Claim.LeaseOwner)
		previousLeaseExpiry := nullableTimeValue(state.Claim.LeaseExpiresAt)
		var workflowVersion int64
		var leaseExpiresAt time.Time
		if err := scoped.q.QueryRowContext(ctx, `
			UPDATE domain_ownership_claims
			SET workflow_version=workflow_version+1,
			    lease_owner=$7, lease_expires_at=clock_timestamp()+make_interval(secs => $8),
			    updated_at=now()
			WHERE canonical_domain=$1 AND org_domain_id=$2::uuid AND org_id=$3::uuid
			  AND owner_kind='legacy' AND state=$4 AND workflow_version=$5
			  AND lease_owner IS NOT DISTINCT FROM $6
			  AND lease_expires_at IS NOT DISTINCT FROM $9
			RETURNING workflow_version, lease_expires_at
		`, state.Claim.CanonicalDomain, state.Domain.ID, state.Domain.OrgID,
			state.Claim.State, state.Claim.WorkflowVersion, previousLeaseOwner,
			leaseOwner, ttl.Seconds(), previousLeaseExpiry,
		).Scan(&workflowVersion, &leaseExpiresAt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrLegacyDomainProviderCASConflict
			}
			return err
		}
		state.Claim.WorkflowVersion = workflowVersion
		state.Claim.LeaseOwner = sql.NullString{String: leaseOwner, Valid: true}
		state.Claim.LeaseExpiresAt = sql.NullTime{Time: leaseExpiresAt, Valid: true}

		if request.Operation == LegacyDomainProviderCreate {
			var updatedAt time.Time
			result := scoped.q.QueryRowContext(ctx, `
				UPDATE org_domains
				SET resend_domain_status='provider_unknown', updated_at=now()
				WHERE id=$1::uuid AND org_id=$2::uuid AND domain=$3 AND status=$4
				  AND updated_at=$5 AND resend_domain_id IS NULL
				  AND resend_domain_status IS NULL AND resend_dns_records IS NULL
				  AND resend_receiving_enabled=false
				RETURNING updated_at
			`, state.Domain.ID, state.Domain.OrgID, state.Domain.Domain,
				state.Domain.Status, state.Domain.UpdatedAt,
			)
			if err := result.Scan(&updatedAt); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrLegacyDomainProviderCASConflict
				}
				return err
			}
			state.Domain.ResendStatus = sql.NullString{String: "provider_unknown", Valid: true}
			state.Domain.UpdatedAt = updatedAt
		}
		intent = legacyDomainProviderIntentFromState(request.Operation, LegacyDomainProviderCallAuthorized, state)
		return nil
	})
	return intent, err
}

func (s *Store) beginLegacyDomainDeleteLocked(
	ctx context.Context,
	state lockedLegacyDomainProviderState,
	ttl time.Duration,
) (LegacyDomainProviderIntent, error) {
	blocked, err := s.legacyDomainHasReleaseDependents(ctx, state.Domain.ID)
	if err != nil {
		return LegacyDomainProviderIntent{}, err
	}
	if blocked {
		return LegacyDomainProviderIntent{}, ErrResourceConflict
	}
	providerID := ""
	if state.Domain.ResendDomainID.Valid {
		providerID = state.Domain.ResendDomainID.String
	}
	providerIDExact := domains.IsExactProviderResourceID(providerID, legacyResendDomainIDMaxBytes)
	leaseLive := legacyDomainProviderLeaseIsLive(state.Claim, state.Now)
	leaseOperation, knownLeaseOperation := legacyDomainProviderLeaseOperation(state.Claim.LeaseOwner.String)
	providerMaterialEvidence := legacyDomainProviderMaterialEvidence(state.Domain)
	leaseMetadataPresent := state.Claim.LeaseOwner.Valid || state.Claim.LeaseExpiresAt.Valid
	// A delete lease with no provider identity cannot authorize provider I/O.
	// Treat it as the durable local-only finalizer fence, not as evidence that a
	// provider operation happened. This lets an expired local-only finalizer be
	// reclaimed after a process crash without weakening fail-closed handling of
	// unknown or non-delete lease owners.
	localOnlyDeleteLease := legacyDomainRecoverableLocalOnlyDeleteLease(state.Domain, state.Claim)
	providerAttempted := providerMaterialEvidence || (leaseMetadataPresent && !localOnlyDeleteLease)
	// A no-ID create has no provider-native replay key. Preserve its exact
	// operation identity even after the lease deadline so a late exact-ID result
	// can still be captured for cleanup after this delete fence. Expiry revokes
	// normal result authority; it must not erase the only durable provenance for
	// a provider object that may still materialize.
	preserveCreateLease := state.Claim.LeaseOwner.Valid && state.Claim.LeaseExpiresAt.Valid &&
		knownLeaseOperation && leaseOperation == LegacyDomainProviderCreate && providerID == ""

	if state.Claim.State == "releasing" && (leaseLive || preserveCreateLease) {
		disposition := LegacyDomainProviderInFlight
		if !leaseLive {
			disposition = LegacyDomainProviderAwaitingProof
		}
		return legacyDomainProviderIntentFromState(LegacyDomainProviderDelete, disposition, state), nil
	}
	if state.Claim.State != "pending" && state.Claim.State != "provider_owned" && state.Claim.State != "releasing" {
		return LegacyDomainProviderIntent{}, ErrResourceConflict
	}

	disposition := LegacyDomainProviderAwaitingProof
	leaseOwner := ""
	var leaseExpiry any
	if preserveCreateLease {
		leaseOwner = state.Claim.LeaseOwner.String
		leaseExpiry = state.Claim.LeaseExpiresAt.Time
		if leaseLive {
			disposition = LegacyDomainProviderInFlight
		} else {
			disposition = LegacyDomainProviderAwaitingProof
		}
	} else if providerIDExact {
		leaseOwner = makeLegacyDomainProviderLeaseOwner(LegacyDomainProviderDelete)
		leaseExpiry = state.Now.Add(ttl)
		disposition = LegacyDomainProviderCallAuthorized
	} else if !providerAttempted {
		leaseOwner = makeLegacyDomainProviderLeaseOwner(LegacyDomainProviderDelete)
		leaseExpiry = state.Now.Add(ttl)
		disposition = LegacyDomainProviderLocalOnly
	}

	// A repeated unresolved delete is a read-only durable waiting result.
	if state.Claim.State == "releasing" && disposition == LegacyDomainProviderAwaitingProof {
		return legacyDomainProviderIntentFromState(LegacyDomainProviderDelete, disposition, state), nil
	}

	previousLeaseOwner := nullableStringValue(state.Claim.LeaseOwner)
	previousLeaseExpiry := nullableTimeValue(state.Claim.LeaseExpiresAt)
	var workflowVersion int64
	var returnedLeaseOwner sql.NullString
	var returnedLeaseExpiry sql.NullTime
	if err := s.q.QueryRowContext(ctx, `
		UPDATE domain_ownership_claims
		SET state='releasing', workflow_version=workflow_version+1,
		    lease_owner=nullif($7, ''), lease_expires_at=$8,
		    claim_expires_at=NULL, updated_at=now()
		WHERE canonical_domain=$1 AND org_domain_id=$2::uuid AND org_id=$3::uuid
		  AND owner_kind='legacy' AND state=$4 AND workflow_version=$5
		  AND lease_owner IS NOT DISTINCT FROM $6
		  AND lease_expires_at IS NOT DISTINCT FROM $9
		RETURNING workflow_version, lease_owner, lease_expires_at
	`, state.Claim.CanonicalDomain, state.Domain.ID, state.Domain.OrgID,
		state.Claim.State, state.Claim.WorkflowVersion, previousLeaseOwner,
		leaseOwner, leaseExpiry, previousLeaseExpiry,
	).Scan(&workflowVersion, &returnedLeaseOwner, &returnedLeaseExpiry); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return LegacyDomainProviderIntent{}, ErrLegacyDomainProviderCASConflict
		}
		return LegacyDomainProviderIntent{}, err
	}
	var updatedAt time.Time
	if err := s.q.QueryRowContext(ctx, `
		UPDATE org_domains
		SET status='failed', resend_receiving_enabled=false,
		    inbound_enabled=false, updated_at=now()
		WHERE id=$1::uuid AND org_id=$2::uuid AND domain=$3
		  AND status=$4 AND updated_at=$5
		  AND resend_domain_id IS NOT DISTINCT FROM $6
		  AND resend_domain_status IS NOT DISTINCT FROM $7
		RETURNING updated_at
	`, state.Domain.ID, state.Domain.OrgID, state.Domain.Domain,
		state.Domain.Status, state.Domain.UpdatedAt,
		nullableStringValue(state.Domain.ResendDomainID), nullableStringValue(state.Domain.ResendStatus),
	).Scan(&updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return LegacyDomainProviderIntent{}, ErrLegacyDomainProviderCASConflict
		}
		return LegacyDomainProviderIntent{}, err
	}
	state.Claim.State = "releasing"
	state.Claim.WorkflowVersion = workflowVersion
	state.Claim.LeaseOwner = returnedLeaseOwner
	state.Claim.LeaseExpiresAt = returnedLeaseExpiry
	state.Claim.ClaimExpiresAt = sql.NullTime{}
	state.Domain.Status = "failed"
	state.Domain.ResendReceivingEnabled = false
	state.Domain.InboundEnabled = false
	state.Domain.UpdatedAt = updatedAt
	intent := legacyDomainProviderIntentFromState(LegacyDomainProviderDelete, disposition, state)
	if disposition == LegacyDomainProviderLocalOnly {
		// The lease fences a local-only finalizer; it does not mean an external
		// provider operation was attempted.
		intent.ProviderAttempted = false
	}
	return intent, nil
}

func validateLegacyDomainProviderResult(result LegacyDomainProviderResult) error {
	if result.Intent.Disposition != LegacyDomainProviderCallAuthorized ||
		!validLegacyDomainProviderOperation(result.Intent.Operation) ||
		result.Intent.LeaseOwner == "" || result.Intent.LeaseExpiresAt.IsZero() {
		return ErrLegacyDomainProviderResultInvalid
	}
	if result.Outcome != LegacyDomainProviderObserved && result.Outcome != LegacyDomainProviderUnknown {
		return ErrLegacyDomainProviderResultInvalid
	}
	if (result.ProviderDomainID != "" &&
		!domains.IsExactProviderResourceID(result.ProviderDomainID, legacyResendDomainIDMaxBytes)) ||
		(result.Intent.ProviderDomainID != "" &&
			!domains.IsExactProviderResourceID(result.Intent.ProviderDomainID, legacyResendDomainIDMaxBytes)) ||
		len(result.ProviderStatus) > 256 || len(result.DNSRecords) > 64*1024 {
		return ErrLegacyDomainProviderResultInvalid
	}
	if len(result.DNSRecords) != 0 {
		var records []json.RawMessage
		if err := json.Unmarshal(result.DNSRecords, &records); err != nil {
			return fmt.Errorf("%w: dns records must be a JSON array", ErrLegacyDomainProviderResultInvalid)
		}
	}
	switch result.DomainState {
	case "", "pending", "verified_dns", "provisioning", "active", "failed":
	default:
		return ErrLegacyDomainProviderResultInvalid
	}
	return nil
}

func legacyDomainProviderIntentMatches(
	intent LegacyDomainProviderIntent,
	state lockedLegacyDomainProviderState,
) bool {
	if intent.OrgID != state.Domain.OrgID || intent.OrgDomainID != state.Domain.ID ||
		intent.StoredDomain != state.Domain.Domain || intent.CanonicalDomain != state.Claim.CanonicalDomain ||
		intent.DomainState != state.Domain.Status || !intent.DomainUpdatedAt.Equal(state.Domain.UpdatedAt) ||
		intent.ClaimState != state.Claim.State || intent.WorkflowVersion != state.Claim.WorkflowVersion ||
		!state.Claim.LeaseOwner.Valid || intent.LeaseOwner != state.Claim.LeaseOwner.String ||
		!state.Claim.LeaseExpiresAt.Valid || !intent.LeaseExpiresAt.Equal(state.Claim.LeaseExpiresAt.Time) {
		return false
	}
	providerID := ""
	if state.Domain.ResendDomainID.Valid {
		providerID = state.Domain.ResendDomainID.String
	}
	if intent.ProviderDomainID != providerID || intent.ProviderStatusPresent != state.Domain.ResendStatus.Valid {
		return false
	}
	return !intent.ProviderStatusPresent || intent.ProviderStatus == state.Domain.ResendStatus.String
}

func validateLegacyDomainProviderResultIdentity(result LegacyDomainProviderResult) (string, error) {
	providerID := result.ProviderDomainID
	intentID := result.Intent.ProviderDomainID
	if result.Intent.Operation == LegacyDomainProviderCreate {
		if result.Outcome == LegacyDomainProviderObserved && providerID == "" {
			return "", ErrLegacyDomainProviderResultInvalid
		}
		if intentID != "" && providerID != "" && intentID != providerID {
			return "", ErrLegacyDomainProviderResultInvalid
		}
	} else {
		if intentID == "" || providerID == "" || intentID != providerID {
			return "", ErrLegacyDomainProviderResultInvalid
		}
	}
	if result.ProviderCanonicalDomain != "" {
		canonical, err := domains.CanonicalizeDomain(result.ProviderCanonicalDomain)
		if err != nil || canonical != result.Intent.CanonicalDomain {
			return "", ErrLegacyDomainProviderResultInvalid
		}
	} else if result.Intent.Operation == LegacyDomainProviderCreate && providerID != "" {
		return "", ErrLegacyDomainProviderResultInvalid
	}
	return providerID, nil
}

// ApplyLegacyDomainProviderResult applies an observation only to the exact
// intent snapshot. The sole stale-result exception captures a materialized
// exact-ID create after a one-version release fence; it keeps the claim
// releasing and the domain failed so compensating cleanup is mandatory.
func (s *Store) ApplyLegacyDomainProviderResult(
	ctx context.Context,
	result LegacyDomainProviderResult,
) (LegacyDomainProviderApplyResult, error) {
	if err := validateLegacyDomainProviderResult(result); err != nil {
		return LegacyDomainProviderApplyResult{}, err
	}
	providerID, err := validateLegacyDomainProviderResultIdentity(result)
	if err != nil {
		return LegacyDomainProviderApplyResult{}, err
	}
	var applied LegacyDomainProviderApplyResult
	err = s.withTx(ctx, func(scoped *Store) error {
		state, err := scoped.lockLegacyDomainProviderState(
			ctx, result.Intent.OrgID, result.Intent.OrgDomainID, result.Intent.CanonicalDomain,
		)
		if err != nil {
			return err
		}
		if !legacyDomainProviderIntentMatches(result.Intent, state) ||
			!legacyDomainProviderLeaseIsLive(state.Claim, state.Now) {
			if result.Intent.Operation == LegacyDomainProviderCreate && providerID != "" {
				return scoped.captureStaleLegacyDomainCreateLocked(ctx, state, result, providerID, &applied)
			}
			return ErrLegacyDomainProviderCASConflict
		}
		return scoped.applyLegacyDomainProviderResultLocked(ctx, state, result, providerID, &applied)
	})
	return applied, err
}

func (s *Store) applyLegacyDomainProviderResultLocked(
	ctx context.Context,
	state lockedLegacyDomainProviderState,
	result LegacyDomainProviderResult,
	providerID string,
	applied *LegacyDomainProviderApplyResult,
) error {
	newProviderID := state.Domain.ResendDomainID.String
	if providerID != "" {
		newProviderID = providerID
	}
	newProviderStatus := state.Domain.ResendStatus.String
	if result.Outcome == LegacyDomainProviderUnknown {
		newProviderStatus = "provider_unknown"
	} else if strings.TrimSpace(result.ProviderStatus) != "" {
		newProviderStatus = strings.TrimSpace(result.ProviderStatus)
	}
	newDomainState := state.Domain.Status
	if result.Outcome == LegacyDomainProviderObserved && result.DomainState != "" {
		newDomainState = result.DomainState
	}
	newClaimState := state.Claim.State
	if newProviderID != "" && newClaimState != "releasing" {
		newClaimState = "provider_owned"
	}
	newReceiving := state.Domain.ResendReceivingEnabled
	if result.Outcome == LegacyDomainProviderObserved && result.ReceivingEnabled != nil {
		newReceiving = *result.ReceivingEnabled
	}
	if state.Claim.State == "releasing" || state.Domain.Status == "failed" ||
		newDomainState == "failed" || result.Intent.Operation == LegacyDomainProviderDelete {
		newClaimState = "releasing"
		newDomainState = "failed"
		newReceiving = false
	}

	claimResult, err := s.q.ExecContext(ctx, `
		UPDATE domain_ownership_claims
		SET state=$7, lease_owner=NULL, lease_expires_at=NULL,
		    claim_expires_at=CASE WHEN $7 IN ('provider_owned','releasing') THEN NULL ELSE claim_expires_at END,
		    updated_at=now()
		WHERE canonical_domain=$1 AND org_domain_id=$2::uuid AND org_id=$3::uuid
		  AND owner_kind='legacy' AND state=$4 AND workflow_version=$5
		  AND lease_owner=$6 AND lease_expires_at=$8
		  AND lease_expires_at > clock_timestamp()
	`, state.Claim.CanonicalDomain, state.Domain.ID, state.Domain.OrgID,
		state.Claim.State, state.Claim.WorkflowVersion, state.Claim.LeaseOwner.String,
		newClaimState, state.Claim.LeaseExpiresAt.Time,
	)
	if err != nil {
		return err
	}
	if n, err := claimResult.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return err
		}
		return ErrLegacyDomainProviderCASConflict
	}

	dnsRecords := any(nil)
	if result.Outcome == LegacyDomainProviderObserved && len(result.DNSRecords) != 0 {
		dnsRecords = []byte(result.DNSRecords)
	}
	mxVerified := result.MXVerified
	spfVerified := result.SPFVerified
	dkimVerified := result.DKIMVerified
	dmarcVerified := result.DMARCVerified
	if result.Outcome == LegacyDomainProviderUnknown {
		// Unknown transport outcomes carry no authoritative readiness evidence.
		// The only monotonic projection they may add is an exact provider ID;
		// provider_unknown records that the operation still needs readback.
		mxVerified = nil
		spfVerified = nil
		dkimVerified = nil
		dmarcVerified = nil
	}
	var updated OrgDomain
	err = scanOrgDomain(s.q.QueryRowContext(ctx, `
		UPDATE org_domains
		SET resend_domain_id=nullif($8, ''), resend_domain_status=nullif($9, ''),
		    resend_dns_records=coalesce($10::jsonb, resend_dns_records),
		    status=$11,
		    mx_verified=coalesce($12, mx_verified),
		    spf_verified=coalesce($13, spf_verified),
		    dkim_verified=coalesce($14, dkim_verified),
		    dmarc_verified=coalesce($15, dmarc_verified),
		    resend_receiving_enabled=$16, inbound_enabled=$16,
		    verified_at=CASE WHEN $17 AND $11 IN ('verified_dns','active') THEN coalesce(verified_at, now()) ELSE verified_at END,
		    last_check_at=CASE WHEN $12 IS NOT NULL OR $13 IS NOT NULL OR $14 IS NOT NULL OR $15 IS NOT NULL THEN now() ELSE last_check_at END,
		    updated_at=now()
		WHERE id=$1::uuid AND org_id=$2::uuid AND domain=$3 AND status=$4
		  AND updated_at=$5
		  AND resend_domain_id IS NOT DISTINCT FROM $6
		  AND resend_domain_status IS NOT DISTINCT FROM $7
		RETURNING id, org_id, domain, status, verification_token,
		          mx_verified, spf_verified, dkim_verified, dmarc_verified,
		          inbound_enabled, dkim_selector, dkim_private_key_enc, dkim_public_key,
		          dkim_method, last_check_at, verified_at, expires_at,
		          resend_domain_id, resend_domain_status, resend_dns_records,
		          resend_receiving_enabled, catch_all_enabled, forward_to,
		          created_at, updated_at, to_jsonb(org_domains)->>'external_ref'
	`, state.Domain.ID, state.Domain.OrgID, state.Domain.Domain, state.Domain.Status,
		state.Domain.UpdatedAt, nullableStringValue(state.Domain.ResendDomainID),
		nullableStringValue(state.Domain.ResendStatus), newProviderID, newProviderStatus,
		dnsRecords, newDomainState, mxVerified, spfVerified,
		dkimVerified, dmarcVerified, newReceiving,
		result.Outcome == LegacyDomainProviderObserved,
	), &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLegacyDomainProviderCASConflict
	}
	if err != nil {
		return err
	}
	applied.Domain = updated
	applied.CompensatingCleanupRequired = newClaimState == "releasing" && newProviderID != ""
	return nil
}

func (s *Store) captureStaleLegacyDomainCreateLocked(
	ctx context.Context,
	state lockedLegacyDomainProviderState,
	result LegacyDomainProviderResult,
	providerID string,
	applied *LegacyDomainProviderApplyResult,
) error {
	intent := result.Intent
	if legacyDomainProviderIntentMatches(intent, state) &&
		!legacyDomainProviderLeaseIsLive(state.Claim, state.Now) {
		claimResult, err := s.q.ExecContext(ctx, `
			UPDATE domain_ownership_claims
			SET state='provider_owned', lease_owner=NULL, lease_expires_at=NULL,
			    claim_expires_at=NULL, updated_at=now()
			WHERE canonical_domain=$1 AND org_domain_id=$2::uuid AND org_id=$3::uuid
			  AND owner_kind='legacy' AND state=$4 AND workflow_version=$5
			  AND lease_owner=$6 AND lease_expires_at=$7
		`, state.Claim.CanonicalDomain, state.Domain.ID, state.Domain.OrgID,
			state.Claim.State, state.Claim.WorkflowVersion,
			state.Claim.LeaseOwner.String, state.Claim.LeaseExpiresAt.Time,
		)
		if err != nil {
			return err
		}
		if n, err := claimResult.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return err
			}
			return ErrLegacyDomainProviderCASConflict
		}
		var updated OrgDomain
		err = scanOrgDomain(s.q.QueryRowContext(ctx, `
			UPDATE org_domains
			SET resend_domain_id=$6, resend_domain_status='provider_unknown', updated_at=now()
			WHERE id=$1::uuid AND org_id=$2::uuid AND domain=$3 AND status=$4
			  AND updated_at=$5 AND resend_domain_id IS NULL
			  AND resend_domain_status='provider_unknown'
			  AND resend_dns_records IS NULL AND resend_receiving_enabled=false AND inbound_enabled=false
			RETURNING id, org_id, domain, status, verification_token,
			          mx_verified, spf_verified, dkim_verified, dmarc_verified,
			          inbound_enabled, dkim_selector, dkim_private_key_enc, dkim_public_key,
			          dkim_method, last_check_at, verified_at, expires_at,
			          resend_domain_id, resend_domain_status, resend_dns_records,
			          resend_receiving_enabled, catch_all_enabled, forward_to,
			          created_at, updated_at, to_jsonb(org_domains)->>'external_ref'
		`, state.Domain.ID, state.Domain.OrgID, state.Domain.Domain, state.Domain.Status,
			state.Domain.UpdatedAt, providerID,
		), &updated)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLegacyDomainProviderCASConflict
		}
		if err != nil {
			return err
		}
		applied.Domain = updated
		applied.StaleCreateCaptured = true
		return nil
	}
	if state.Claim.State != "releasing" || state.Domain.Status != "failed" ||
		state.Claim.WorkflowVersion != intent.WorkflowVersion+1 ||
		!state.Claim.LeaseOwner.Valid || state.Claim.LeaseOwner.String != intent.LeaseOwner ||
		!state.Claim.LeaseExpiresAt.Valid || !state.Claim.LeaseExpiresAt.Time.Equal(intent.LeaseExpiresAt) ||
		state.Domain.ResendDomainID.Valid || !state.Domain.ResendStatus.Valid ||
		state.Domain.ResendStatus.String != "provider_unknown" {
		return ErrLegacyDomainProviderCASConflict
	}
	providerStatus := "provider_unknown"
	if result.Outcome == LegacyDomainProviderObserved && strings.TrimSpace(result.ProviderStatus) != "" {
		providerStatus = strings.TrimSpace(result.ProviderStatus)
	}
	if providerStatus == "" {
		providerStatus = "provider_unknown"
	}
	dnsRecords := any(nil)
	if result.Outcome == LegacyDomainProviderObserved && len(result.DNSRecords) != 0 {
		dnsRecords = []byte(result.DNSRecords)
	}

	claimResult, err := s.q.ExecContext(ctx, `
		UPDATE domain_ownership_claims
		SET lease_owner=NULL, lease_expires_at=NULL, updated_at=now()
		WHERE canonical_domain=$1 AND org_domain_id=$2::uuid AND org_id=$3::uuid
		  AND owner_kind='legacy' AND state='releasing' AND workflow_version=$4
		  AND lease_owner=$5 AND lease_expires_at=$6
	`, state.Claim.CanonicalDomain, state.Domain.ID, state.Domain.OrgID,
		state.Claim.WorkflowVersion, state.Claim.LeaseOwner.String, state.Claim.LeaseExpiresAt.Time,
	)
	if err != nil {
		return err
	}
	if n, err := claimResult.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return err
		}
		return ErrLegacyDomainProviderCASConflict
	}
	var updated OrgDomain
	err = scanOrgDomain(s.q.QueryRowContext(ctx, `
		UPDATE org_domains
		SET resend_domain_id=$6, resend_domain_status=$7,
		    resend_dns_records=coalesce($8::jsonb, resend_dns_records),
		    resend_receiving_enabled=false, inbound_enabled=false,
		    status='failed', updated_at=now()
		WHERE id=$1::uuid AND org_id=$2::uuid AND domain=$3
		  AND status='failed' AND updated_at=$4
		  AND resend_domain_id IS NULL AND resend_domain_status=$5
		RETURNING id, org_id, domain, status, verification_token,
		          mx_verified, spf_verified, dkim_verified, dmarc_verified,
		          inbound_enabled, dkim_selector, dkim_private_key_enc, dkim_public_key,
		          dkim_method, last_check_at, verified_at, expires_at,
		          resend_domain_id, resend_domain_status, resend_dns_records,
		          resend_receiving_enabled, catch_all_enabled, forward_to,
		          created_at, updated_at, to_jsonb(org_domains)->>'external_ref'
	`, state.Domain.ID, state.Domain.OrgID, state.Domain.Domain,
		state.Domain.UpdatedAt, "provider_unknown", providerID, providerStatus, dnsRecords,
	), &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLegacyDomainProviderCASConflict
	}
	if err != nil {
		return err
	}
	applied.Domain = updated
	applied.StaleCreateCaptured = true
	applied.CompensatingCleanupRequired = true
	return nil
}

func validateLegacyDomainCreateRejectionReceipt(receipt LegacyDomainCreateRejectionReceipt) error {
	intent := receipt.Intent
	if receipt.Proof != LegacyDomainCreateRejectedBeforeMaterialization ||
		intent.Operation != LegacyDomainProviderCreate ||
		intent.Disposition != LegacyDomainProviderCallAuthorized ||
		intent.ClaimState != "pending" ||
		(intent.DomainState != "pending" && intent.DomainState != "provisioning") ||
		intent.LeaseOwner == "" || intent.LeaseExpiresAt.IsZero() ||
		intent.ProviderDomainID != "" || !intent.ProviderStatusPresent ||
		intent.ProviderStatus != "provider_unknown" {
		return ErrLegacyDomainProviderAbsenceRequired
	}
	return nil
}

func legacyDomainCreateRejectionMatchesCloseFence(
	intent LegacyDomainProviderIntent,
	state lockedLegacyDomainProviderState,
) bool {
	return intent.OrgID == state.Domain.OrgID && intent.OrgDomainID == state.Domain.ID &&
		intent.StoredDomain == state.Domain.Domain && intent.CanonicalDomain == state.Claim.CanonicalDomain &&
		state.Domain.Status == "failed" && state.Claim.State == "releasing" &&
		state.Claim.WorkflowVersion == intent.WorkflowVersion+1 &&
		state.Claim.LeaseOwner.Valid && state.Claim.LeaseOwner.String == intent.LeaseOwner &&
		state.Claim.LeaseExpiresAt.Valid && state.Claim.LeaseExpiresAt.Time.Equal(intent.LeaseExpiresAt)
}

// FinalizeLegacyDomainCreateRejection releases the local row only when the
// exact POST /domains request was authoritatively rejected before provider
// materialization. The same receipt remains valid if a concurrent delete won
// after Begin(Create): that path preserves the exact create lease while moving
// the row to failed/releasing, so the proof cannot be lost to the close race.
func (s *Store) FinalizeLegacyDomainCreateRejection(
	ctx context.Context,
	receipt LegacyDomainCreateRejectionReceipt,
) (bool, error) {
	if err := validateLegacyDomainCreateRejectionReceipt(receipt); err != nil {
		return false, err
	}
	deleted := false
	err := s.withTx(ctx, func(scoped *Store) error {
		state, err := scoped.lockLegacyDomainProviderState(
			ctx, receipt.Intent.OrgID, receipt.Intent.OrgDomainID, receipt.Intent.CanonicalDomain,
		)
		if err != nil {
			return err
		}
		// The rejection receipt proves the exact create never materialized. Its
		// authority survives the deadline only while the persisted version,
		// owner, expiry, and full intent snapshot remain unchanged; any takeover
		// or close fence changes that snapshot and follows the +1 branch below.
		normalMatch := legacyDomainProviderIntentMatches(receipt.Intent, state)
		if !normalMatch && !legacyDomainCreateRejectionMatchesCloseFence(receipt.Intent, state) {
			return ErrLegacyDomainProviderCASConflict
		}
		if state.Domain.ResendDomainID.Valid || !state.Domain.ResendStatus.Valid ||
			state.Domain.ResendStatus.String != "provider_unknown" || len(state.Domain.ResendDNSRecords) != 0 ||
			state.Domain.ResendReceivingEnabled || state.Domain.InboundEnabled {
			return ErrLegacyDomainProviderAbsenceRequired
		}
		blocked, err := scoped.legacyDomainHasReleaseDependents(ctx, state.Domain.ID)
		if err != nil {
			return err
		}
		if blocked {
			return ErrResourceConflict
		}
		claimResult, err := scoped.q.ExecContext(ctx, `
			DELETE FROM domain_ownership_claims
			WHERE canonical_domain=$1 AND org_domain_id=$2::uuid AND org_id=$3::uuid
			  AND owner_kind='legacy' AND state=$4 AND workflow_version=$5
			  AND lease_owner=$6 AND lease_expires_at=$7
		`, state.Claim.CanonicalDomain, state.Domain.ID, state.Domain.OrgID,
			state.Claim.State, state.Claim.WorkflowVersion,
			state.Claim.LeaseOwner.String, state.Claim.LeaseExpiresAt.Time,
		)
		if err != nil {
			return err
		}
		if n, err := claimResult.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return err
			}
			return ErrLegacyDomainProviderCASConflict
		}
		domainResult, err := scoped.q.ExecContext(ctx, `
			DELETE FROM org_domains
			WHERE id=$1::uuid AND org_id=$2::uuid AND domain=$3
			  AND status=$4 AND updated_at=$5
			  AND resend_domain_id IS NULL AND resend_domain_status='provider_unknown'
			  AND resend_dns_records IS NULL AND resend_receiving_enabled=false AND inbound_enabled=false
		`, state.Domain.ID, state.Domain.OrgID, state.Domain.Domain, state.Domain.Status, state.Domain.UpdatedAt)
		if err != nil {
			return err
		}
		n, err := domainResult.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrLegacyDomainProviderCASConflict
		}
		deleted = true
		return nil
	})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return deleted, err
}

func validateLegacyDomainProviderDeleteIntent(intent LegacyDomainProviderIntent) error {
	if intent.Operation != LegacyDomainProviderDelete || intent.ClaimState != "releasing" ||
		intent.DomainState != "failed" || intent.LeaseOwner == "" || intent.LeaseExpiresAt.IsZero() {
		return ErrLegacyDomainProviderResultInvalid
	}
	return nil
}

// FinalizeLegacyDomainProviderAbsence deletes the local legacy row/claim only
// after authoritative absence readback of the exact ID named by the delete
// intent. DELETE 2xx, canonical-name inventory, or an empty ID is rejected.
func (s *Store) FinalizeLegacyDomainProviderAbsence(
	ctx context.Context,
	receipt LegacyDomainProviderAbsenceReceipt,
) (bool, error) {
	if err := validateLegacyDomainProviderDeleteIntent(receipt.Intent); err != nil ||
		receipt.Intent.Disposition != LegacyDomainProviderCallAuthorized ||
		receipt.Proof != LegacyDomainExactIDAuthoritativeAbsence ||
		!domains.IsExactProviderResourceID(receipt.ProviderDomainID, legacyResendDomainIDMaxBytes) ||
		receipt.ProviderDomainID != receipt.Intent.ProviderDomainID {
		return false, ErrLegacyDomainProviderAbsenceRequired
	}
	deleted := false
	err := s.withTx(ctx, func(scoped *Store) error {
		state, err := scoped.lockLegacyDomainProviderState(
			ctx, receipt.Intent.OrgID, receipt.Intent.OrgDomainID, receipt.Intent.CanonicalDomain,
		)
		if err != nil {
			return err
		}
		if !legacyDomainProviderIntentMatches(receipt.Intent, state) ||
			!legacyDomainProviderLeaseIsLive(state.Claim, state.Now) ||
			state.Domain.ResendDomainID.String != receipt.ProviderDomainID {
			return ErrLegacyDomainProviderCASConflict
		}
		blocked, err := scoped.legacyDomainHasReleaseDependents(ctx, state.Domain.ID)
		if err != nil {
			return err
		}
		if blocked {
			return ErrResourceConflict
		}
		claimResult, err := scoped.q.ExecContext(ctx, `
			DELETE FROM domain_ownership_claims
			WHERE canonical_domain=$1 AND org_domain_id=$2::uuid AND org_id=$3::uuid
			  AND owner_kind='legacy' AND state='releasing' AND workflow_version=$4
			  AND lease_owner=$5 AND lease_expires_at=$6
			  AND lease_expires_at > clock_timestamp()
		`, state.Claim.CanonicalDomain, state.Domain.ID, state.Domain.OrgID,
			state.Claim.WorkflowVersion, state.Claim.LeaseOwner.String, state.Claim.LeaseExpiresAt.Time,
		)
		if err != nil {
			return err
		}
		if n, err := claimResult.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return err
			}
			return ErrLegacyDomainProviderCASConflict
		}
		domainResult, err := scoped.q.ExecContext(ctx, `
			DELETE FROM org_domains
			WHERE id=$1::uuid AND org_id=$2::uuid AND domain=$3
			  AND status='failed' AND updated_at=$4
			  AND resend_domain_id=$5
			  AND resend_domain_status IS NOT DISTINCT FROM $6
		`, state.Domain.ID, state.Domain.OrgID, state.Domain.Domain,
			state.Domain.UpdatedAt, receipt.ProviderDomainID, nullableStringValue(state.Domain.ResendStatus),
		)
		if err != nil {
			return err
		}
		n, err := domainResult.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrLegacyDomainProviderCASConflict
		}
		deleted = true
		return nil
	})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return deleted, err
}

// FinalizeLegacyDomainLocalOnlyRelease is the sole no-provider-proof exception.
// It is available only when BeginDelete observed a pristine row and persisted a
// local-only lease. Any Resend identity/status/record/receiving evidence or a
// prior provider lease makes this path fail closed.
func (s *Store) FinalizeLegacyDomainLocalOnlyRelease(
	ctx context.Context,
	intent LegacyDomainProviderIntent,
) (bool, error) {
	if err := validateLegacyDomainProviderDeleteIntent(intent); err != nil ||
		intent.Disposition != LegacyDomainProviderLocalOnly || intent.ProviderAttempted ||
		intent.ProviderDomainID != "" || intent.ProviderStatusPresent {
		return false, ErrLegacyDomainProviderAbsenceRequired
	}
	deleted := false
	err := s.withTx(ctx, func(scoped *Store) error {
		state, err := scoped.lockLegacyDomainProviderState(ctx, intent.OrgID, intent.OrgDomainID, intent.CanonicalDomain)
		if err != nil {
			return err
		}
		if !legacyDomainProviderIntentMatches(intent, state) ||
			!legacyDomainProviderLeaseIsLive(state.Claim, state.Now) ||
			state.Domain.ResendDomainID.Valid || state.Domain.ResendStatus.Valid ||
			len(state.Domain.ResendDNSRecords) != 0 || state.Domain.ResendReceivingEnabled ||
			state.Domain.InboundEnabled {
			return ErrLegacyDomainProviderAbsenceRequired
		}
		blocked, err := scoped.legacyDomainHasReleaseDependents(ctx, state.Domain.ID)
		if err != nil {
			return err
		}
		if blocked {
			return ErrResourceConflict
		}
		claimResult, err := scoped.q.ExecContext(ctx, `
			DELETE FROM domain_ownership_claims
			WHERE canonical_domain=$1 AND org_domain_id=$2::uuid AND org_id=$3::uuid
			  AND owner_kind='legacy' AND state='releasing' AND workflow_version=$4
			  AND lease_owner=$5 AND lease_expires_at=$6
			  AND lease_expires_at > clock_timestamp()
		`, state.Claim.CanonicalDomain, state.Domain.ID, state.Domain.OrgID,
			state.Claim.WorkflowVersion, state.Claim.LeaseOwner.String, state.Claim.LeaseExpiresAt.Time,
		)
		if err != nil {
			return err
		}
		if n, err := claimResult.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return err
			}
			return ErrLegacyDomainProviderCASConflict
		}
		domainResult, err := scoped.q.ExecContext(ctx, `
			DELETE FROM org_domains
			WHERE id=$1::uuid AND org_id=$2::uuid AND domain=$3
			  AND status='failed' AND updated_at=$4
			  AND resend_domain_id IS NULL AND resend_domain_status IS NULL
			  AND resend_dns_records IS NULL AND resend_receiving_enabled=false AND inbound_enabled=false
		`, state.Domain.ID, state.Domain.OrgID, state.Domain.Domain, state.Domain.UpdatedAt)
		if err != nil {
			return err
		}
		n, err := domainResult.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrLegacyDomainProviderCASConflict
		}
		deleted = true
		return nil
	})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return deleted, err
}

func legacyDomainCleanupNullStringEqual(left, right sql.NullString) bool {
	return left.Valid == right.Valid && (!left.Valid || left.String == right.String)
}

func legacyDomainCleanupNullTimeEqual(left, right sql.NullTime) bool {
	return left.Valid == right.Valid && (!left.Valid || left.Time.Equal(right.Time))
}

func validateLegacyDomainCleanupCandidate(candidate LegacyDomainCleanupCandidate) error {
	if _, err := uuid.Parse(strings.TrimSpace(candidate.OrgID)); err != nil {
		return errors.New("invalid cleanup candidate org id")
	}
	if _, err := uuid.Parse(strings.TrimSpace(candidate.OrgDomainID)); err != nil {
		return errors.New("invalid cleanup candidate org domain id")
	}
	canonical, err := domains.CanonicalizeDomain(candidate.CanonicalDomain)
	if err != nil || canonical != candidate.CanonicalDomain {
		return errors.New("invalid cleanup candidate canonical domain")
	}
	if candidate.DomainUpdatedAt.IsZero() || candidate.WorkflowVersion <= 0 ||
		candidate.LeaseOwner.Valid != candidate.LeaseExpiresAt.Valid ||
		(candidate.LeaseExpiresAt.Valid && candidate.LeaseExpiresAt.Time.IsZero()) ||
		(candidate.CleanupNotBefore.Valid && candidate.CleanupNotBefore.Time.IsZero()) {
		return errors.New("invalid cleanup candidate snapshot")
	}
	return nil
}

func legacyDomainCleanupCandidateMatches(
	candidate LegacyDomainCleanupCandidate,
	state lockedLegacyDomainProviderState,
	hasOpenQuarantine bool,
) bool {
	return candidate.OrgID == state.Domain.OrgID && candidate.OrgDomainID == state.Domain.ID &&
		candidate.CanonicalDomain == state.Claim.CanonicalDomain &&
		candidate.DomainUpdatedAt.Equal(state.Domain.UpdatedAt) &&
		candidate.WorkflowVersion == state.Claim.WorkflowVersion &&
		legacyDomainCleanupNullStringEqual(candidate.LeaseOwner, state.Claim.LeaseOwner) &&
		legacyDomainCleanupNullTimeEqual(candidate.LeaseExpiresAt, state.Claim.LeaseExpiresAt) &&
		legacyDomainCleanupNullTimeEqual(candidate.CleanupNotBefore, state.Claim.ClaimExpiresAt) &&
		legacyDomainCleanupNullStringEqual(candidate.ProviderDomainID, state.Domain.ResendDomainID) &&
		candidate.HasOpenQuarantine == hasOpenQuarantine
}

func (s *Store) legacyDomainHasOpenQuarantine(ctx context.Context, canonicalDomain string) (bool, error) {
	var open bool
	err := s.q.QueryRowContext(ctx, `
		SELECT EXISTS(
		  SELECT 1 FROM provider_domain_quarantine
		  WHERE canonical_domain=$1 AND state='open'
		)
	`, canonicalDomain).Scan(&open)
	return open, err
}

func legacyDomainCleanupStateIsDue(state lockedLegacyDomainProviderState) bool {
	return state.Claim.OwnerKind == "legacy" && state.Claim.State == "releasing" &&
		state.Domain.Status == "failed" && !legacyDomainProviderLeaseIsLive(state.Claim, state.Now) &&
		(!state.Claim.ClaimExpiresAt.Valid || !state.Claim.ClaimExpiresAt.Time.After(state.Now))
}

func legacyDomainCleanupIntentMatches(
	intent LegacyDomainProviderIntent,
	state lockedLegacyDomainProviderState,
) bool {
	if intent.Operation != LegacyDomainProviderDelete ||
		intent.OrgID != state.Domain.OrgID || intent.OrgDomainID != state.Domain.ID ||
		intent.StoredDomain != state.Domain.Domain || intent.CanonicalDomain != state.Claim.CanonicalDomain ||
		intent.DomainState != state.Domain.Status || !intent.DomainUpdatedAt.Equal(state.Domain.UpdatedAt) ||
		intent.ClaimState != state.Claim.State || intent.WorkflowVersion != state.Claim.WorkflowVersion {
		return false
	}
	if intent.LeaseOwner == "" {
		if state.Claim.LeaseOwner.Valid || state.Claim.LeaseExpiresAt.Valid || !intent.LeaseExpiresAt.IsZero() {
			return false
		}
	} else if !state.Claim.LeaseOwner.Valid || state.Claim.LeaseOwner.String != intent.LeaseOwner ||
		!state.Claim.LeaseExpiresAt.Valid || !state.Claim.LeaseExpiresAt.Time.Equal(intent.LeaseExpiresAt) {
		return false
	}
	providerID := ""
	if state.Domain.ResendDomainID.Valid {
		providerID = state.Domain.ResendDomainID.String
	}
	if intent.ProviderDomainID != providerID || intent.ProviderStatusPresent != state.Domain.ResendStatus.Valid {
		return false
	}
	return !intent.ProviderStatusPresent || intent.ProviderStatus == state.Domain.ResendStatus.String
}

func validateLegacyDomainCleanupDelay(delay time.Duration) error {
	if delay < time.Second || delay > legacyDomainCleanupMaxDelay {
		return fmt.Errorf("legacy domain cleanup delay must be between one second and %s", legacyDomainCleanupMaxDelay)
	}
	return nil
}

// ListLegacyDomainCleanupDue returns a bounded, stable ordering of actionable
// failed/releasing legacy domains. claim_expires_at is otherwise unused once a
// legacy claim is releasing, so schema 9 safely carries the cleanup not-before
// timestamp there. A claimed AwaitingProof/quarantine row therefore rotates
// behind later exact-ID work instead of occupying the first bounded page.
// Discovery is read-only; ClaimLegacyDomainCleanup must win the exact snapshot
// before the caller performs provider or local cleanup work.
func (s *Store) ListLegacyDomainCleanupDue(ctx context.Context, limit int) ([]LegacyDomainCleanupCandidate, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	schema9, err := s.cloudSchemaSupportsM2M(ctx)
	if err != nil || !schema9 {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT d.org_id::text, d.id::text, c.canonical_domain, d.updated_at,
		       c.workflow_version, c.lease_owner, c.lease_expires_at,
		       c.claim_expires_at, d.resend_domain_id,
		       EXISTS(
		         SELECT 1 FROM provider_domain_quarantine quarantine
		         WHERE quarantine.canonical_domain=c.canonical_domain
		           AND quarantine.state='open'
		       )
		FROM org_domains d
		JOIN domain_ownership_claims c
		  ON c.org_domain_id=d.id AND c.org_id=d.org_id
		WHERE c.owner_kind='legacy' AND c.state='releasing' AND d.status='failed'
		  AND (c.lease_expires_at IS NULL OR c.lease_expires_at <= clock_timestamp())
		  AND (c.claim_expires_at IS NULL OR c.claim_expires_at <= clock_timestamp())
		  AND NOT EXISTS(SELECT 1 FROM inboxes WHERE org_domain_id=d.id)
		  AND NOT EXISTS(SELECT 1 FROM org_domain_grants WHERE org_domain_id=d.id)
		  AND NOT EXISTS(SELECT 1 FROM managed_mailbox_platform_domains WHERE org_domain_id=d.id)
		ORDER BY coalesce(c.claim_expires_at, c.updated_at), c.updated_at,
		         c.canonical_domain, d.id
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	candidates := make([]LegacyDomainCleanupCandidate, 0)
	for rows.Next() {
		var candidate LegacyDomainCleanupCandidate
		if err := rows.Scan(
			&candidate.OrgID, &candidate.OrgDomainID, &candidate.CanonicalDomain,
			&candidate.DomainUpdatedAt, &candidate.WorkflowVersion,
			&candidate.LeaseOwner, &candidate.LeaseExpiresAt,
			&candidate.CleanupNotBefore, &candidate.ProviderDomainID,
			&candidate.HasOpenQuarantine,
		); err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

// ClaimLegacyDomainCleanup claims one exact discovery snapshot. Exact-ID and
// pristine local-only rows acquire the normal Delete intent lease. An
// unresolved no-ID row cannot safely replace a preserved Create lease, so it
// acquires a short exact-CAS scheduling claim in claim_expires_at and returns
// AwaitingProof without authorizing provider mutation.
func (s *Store) ClaimLegacyDomainCleanup(
	ctx context.Context,
	candidate LegacyDomainCleanupCandidate,
	ttl time.Duration,
) (LegacyDomainCleanupClaim, bool, error) {
	var claim LegacyDomainCleanupClaim
	if err := validateLegacyDomainCleanupCandidate(candidate); err != nil {
		return claim, false, err
	}
	ttl, err := normalizeLegacyDomainProviderLeaseTTL(ttl)
	if err != nil {
		return claim, false, err
	}
	claimed := false
	err = s.withTx(ctx, func(scoped *Store) error {
		state, err := scoped.lockLegacyDomainProviderState(
			ctx, candidate.OrgID, candidate.OrgDomainID, candidate.CanonicalDomain,
		)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		hasOpenQuarantine, err := scoped.legacyDomainHasOpenQuarantine(ctx, state.Claim.CanonicalDomain)
		if err != nil {
			return err
		}
		if !legacyDomainCleanupCandidateMatches(candidate, state, hasOpenQuarantine) ||
			!legacyDomainCleanupStateIsDue(state) {
			return nil
		}
		blocked, err := scoped.legacyDomainHasReleaseDependents(ctx, state.Domain.ID)
		if err != nil {
			return err
		}
		if blocked {
			return nil
		}

		var intent LegacyDomainProviderIntent
		if hasOpenQuarantine {
			// Quarantine is explicit conflicting inventory authority. Even an
			// otherwise exact local provider ID cannot authorize mutation until
			// every open observation for the canonical name is resolved.
			intent = legacyDomainProviderIntentFromState(
				LegacyDomainProviderDelete, LegacyDomainProviderAwaitingProof, state,
			)
		} else {
			intent, err = scoped.beginLegacyDomainDeleteLocked(ctx, state, ttl)
			if err != nil {
				return err
			}
		}
		claim.Intent = intent
		if intent.Disposition == LegacyDomainProviderAwaitingProof {
			var notBefore time.Time
			err := scoped.q.QueryRowContext(ctx, `
				UPDATE domain_ownership_claims
				SET claim_expires_at=clock_timestamp()+$8::interval, updated_at=now()
				WHERE canonical_domain=$1 AND org_domain_id=$2::uuid AND org_id=$3::uuid
				  AND owner_kind='legacy' AND state='releasing' AND workflow_version=$4
				  AND lease_owner IS NOT DISTINCT FROM $5
				  AND lease_expires_at IS NOT DISTINCT FROM $6
				  AND claim_expires_at IS NOT DISTINCT FROM $7
				  AND (lease_expires_at IS NULL OR lease_expires_at <= clock_timestamp())
				  AND (claim_expires_at IS NULL OR claim_expires_at <= clock_timestamp())
				RETURNING claim_expires_at
			`, state.Claim.CanonicalDomain, state.Domain.ID, state.Domain.OrgID,
				state.Claim.WorkflowVersion, nullableStringValue(state.Claim.LeaseOwner),
				nullableTimeValue(state.Claim.LeaseExpiresAt), nullableTimeValue(state.Claim.ClaimExpiresAt),
				ttl.String(),
			).Scan(&notBefore)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrLegacyDomainProviderCASConflict
			}
			if err != nil {
				return err
			}
			claim.CleanupNotBefore = notBefore
		}
		claimed = true
		return nil
	})
	return claim, claimed, err
}

// DeferLegacyDomainCleanup moves a live AwaitingProof scheduling claim to a
// bounded later time. It does not change workflow_version or either provider
// lease field, so an exact late Create result retains its original capture
// authority. The previous not-before timestamp is the scheduling CAS token.
func (s *Store) DeferLegacyDomainCleanup(
	ctx context.Context,
	claim LegacyDomainCleanupClaim,
	delay time.Duration,
) (LegacyDomainCleanupClaim, error) {
	if claim.Intent.Disposition != LegacyDomainProviderAwaitingProof ||
		claim.CleanupNotBefore.IsZero() ||
		claim.Intent.ClaimState != "releasing" || claim.Intent.DomainState != "failed" {
		return LegacyDomainCleanupClaim{}, ErrLegacyDomainProviderResultInvalid
	}
	if err := validateLegacyDomainCleanupDelay(delay); err != nil {
		return LegacyDomainCleanupClaim{}, err
	}
	updated := claim
	err := s.withTx(ctx, func(scoped *Store) error {
		state, err := scoped.lockLegacyDomainProviderState(
			ctx, claim.Intent.OrgID, claim.Intent.OrgDomainID, claim.Intent.CanonicalDomain,
		)
		if err != nil {
			return err
		}
		if !legacyDomainCleanupIntentMatches(claim.Intent, state) ||
			!state.Claim.ClaimExpiresAt.Valid ||
			!state.Claim.ClaimExpiresAt.Time.Equal(claim.CleanupNotBefore) ||
			!state.Claim.ClaimExpiresAt.Time.After(state.Now) {
			return ErrLegacyDomainProviderCASConflict
		}
		if !state.Now.Add(delay).After(state.Claim.ClaimExpiresAt.Time) {
			return errors.New("legacy domain cleanup deferral must extend the live claim")
		}
		var notBefore time.Time
		err = scoped.q.QueryRowContext(ctx, `
			UPDATE domain_ownership_claims
			SET claim_expires_at=clock_timestamp()+$8::interval, updated_at=now()
			WHERE canonical_domain=$1 AND org_domain_id=$2::uuid AND org_id=$3::uuid
			  AND owner_kind='legacy' AND state='releasing' AND workflow_version=$4
			  AND lease_owner IS NOT DISTINCT FROM $5
			  AND lease_expires_at IS NOT DISTINCT FROM $6
			  AND claim_expires_at=$7 AND claim_expires_at > clock_timestamp()
			RETURNING claim_expires_at
		`, state.Claim.CanonicalDomain, state.Domain.ID, state.Domain.OrgID,
			state.Claim.WorkflowVersion, nullableStringValue(state.Claim.LeaseOwner),
			nullableTimeValue(state.Claim.LeaseExpiresAt), claim.CleanupNotBefore,
			delay.String(),
		).Scan(&notBefore)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLegacyDomainProviderCASConflict
		}
		if err != nil {
			return err
		}
		updated.CleanupNotBefore = notBefore
		return nil
	})
	return updated, err
}
