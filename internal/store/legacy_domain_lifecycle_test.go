package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// migrateLegacyDomainLifecycleFixture keeps the shared lifecycle tests honest
// in both repositories without making Cloud 0009 an OSS migration. The normal
// OSS Cloud migration head remains 3; each test database applies the real Core
// schema, then installs only the Cloud-9 relations this shared Store boundary
// is permitted to query and an isolated schema-version observation.
func migrateLegacyDomainLifecycleFixture(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	cloudVersion int64,
) {
	t.Helper()
	if err := MigrateCore(ctx, db); err != nil {
		t.Fatalf("migrate lifecycle fixture Core schema: %v", err)
	}
	if cloudVersion != 8 && cloudVersion != 9 {
		t.Fatalf("unsupported lifecycle fixture Cloud version %d", cloudVersion)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE schema_migrations_cloud (
		  id          bigserial   PRIMARY KEY,
		  version_id  bigint      NOT NULL,
		  is_applied  boolean     NOT NULL,
		  tstamp      timestamptz NOT NULL DEFAULT now()
		)
	`); err != nil {
		t.Fatalf("install lifecycle fixture Cloud migration table: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO schema_migrations_cloud(version_id, is_applied) VALUES ($1, true)
	`, cloudVersion); err != nil {
		t.Fatalf("record lifecycle fixture Cloud version: %v", err)
	}
	if cloudVersion == 9 {
		if _, err := db.ExecContext(ctx, `
			CREATE TABLE agent_onboardings (
			  id uuid PRIMARY KEY
			);

			CREATE TABLE managed_mailbox_platform_domains (
			  org_domain_id     uuid        PRIMARY KEY REFERENCES org_domains(id) ON DELETE RESTRICT,
			  owner_org_id      uuid        NOT NULL REFERENCES orgs(id) ON DELETE RESTRICT,
			  canonical_domain  text        NOT NULL UNIQUE,
			  state             text        NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'disabled')),
			  validated_at      timestamptz NOT NULL,
			  disabled_at       timestamptz,
			  created_at        timestamptz NOT NULL DEFAULT now(),
			  updated_at        timestamptz NOT NULL DEFAULT now()
			);

			CREATE TABLE domain_ownership_claims (
			  canonical_domain  text        PRIMARY KEY
			                                 CHECK (canonical_domain = lower(canonical_domain)
			                                        AND canonical_domain NOT LIKE '%.'),
			  org_domain_id     uuid        NOT NULL UNIQUE REFERENCES org_domains(id) ON DELETE RESTRICT,
			  org_id            uuid        NOT NULL REFERENCES orgs(id) ON DELETE RESTRICT,
			  onboarding_id     uuid        REFERENCES agent_onboardings(id) ON DELETE RESTRICT,
			  owner_kind        text        NOT NULL CHECK (owner_kind IN ('legacy', 'autonomous')),
			  state             text        NOT NULL CHECK (state IN ('pending', 'provider_owned', 'releasing')),
			  workflow_version  bigint      NOT NULL DEFAULT 1 CHECK (workflow_version > 0),
			  lease_owner       text,
			  lease_expires_at  timestamptz,
			  claim_expires_at  timestamptz,
			  created_at        timestamptz NOT NULL DEFAULT now(),
			  updated_at        timestamptz NOT NULL DEFAULT now(),
			  CHECK ((lease_owner IS NULL AND lease_expires_at IS NULL)
			      OR (lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL)),
			  CHECK ((owner_kind = 'legacy' AND onboarding_id IS NULL)
			      OR (owner_kind = 'autonomous' AND onboarding_id IS NOT NULL))
			);

			CREATE TABLE provider_domain_quarantine (
			  id                    uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
			  canonical_domain      text        NOT NULL,
			  provider_domain_id    text        NOT NULL CHECK (btrim(provider_domain_id) <> ''),
			  local_org_domain_id   uuid        REFERENCES org_domains(id) ON DELETE RESTRICT,
			  discrepancy           text        NOT NULL
			                                     CHECK (discrepancy IN (
			                                       'provider_only', 'provider_id_mismatch', 'canonical_mismatch'
			                                     )),
			  state                 text        NOT NULL DEFAULT 'open'
			                                     CHECK (state IN ('open', 'deleted', 'adopted')),
			  inventory_sha256      text        NOT NULL CHECK (inventory_sha256 ~ '^[0-9a-f]{64}$'),
			  resolution_receipt    text,
			  bounded_details       text,
			  discovered_at         timestamptz NOT NULL DEFAULT now(),
			  resolved_at           timestamptz,
			  resolved_by           text
			);
			CREATE UNIQUE INDEX uq_provider_domain_quarantine_open
			  ON provider_domain_quarantine(canonical_domain, provider_domain_id)
			  WHERE state='open'
		`); err != nil {
			t.Fatalf("install lifecycle fixture Cloud-9 relations: %v", err)
		}
	}
	st := &Store{db: db, q: db}
	schema9, err := st.cloudSchemaSupportsM2M(ctx)
	if err != nil {
		t.Fatalf("detect lifecycle fixture Cloud version: %v", err)
	}
	if schema9 != (cloudVersion >= 9) {
		t.Fatalf("lifecycle fixture schema9=%t for Cloud version %d", schema9, cloudVersion)
	}
}

func recordLegacyDomainLifecycleOpenQuarantine(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	canonicalDomain string,
	providerDomainID string,
	localOrgDomainID string,
	discrepancy string,
	inventorySHA256 string,
) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO provider_domain_quarantine(
		  canonical_domain, provider_domain_id, local_org_domain_id,
		  discrepancy, inventory_sha256
		)
		VALUES ($1, $2, $3::uuid, $4, $5)
	`, canonicalDomain, providerDomainID, localOrgDomainID, discrepancy, inventorySHA256); err != nil {
		t.Fatalf("record lifecycle fixture quarantine: %v", err)
	}
}

func waitForLegacyDomainLifecyclePostgresBlocker(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	blockedPID int,
	blockerPID int,
	done <-chan error,
	operation string,
) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case err := <-done:
			t.Fatalf("%s completed before PostgreSQL reported the expected blocker: %v", operation, err)
		default:
		}
		var blocked bool
		if err := db.QueryRowContext(ctx, `
			SELECT $2::integer = ANY(pg_blocking_pids($1::integer))
		`, blockedPID, blockerPID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was not blocked by backend %d", operation, blockerPID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func createLegacyDomainLifecycleFixture(
	t *testing.T,
	ctx context.Context,
	st *Store,
	name string,
	domain string,
) (string, string) {
	t.Helper()
	orgID, err := st.CreateOrg(ctx, name)
	if err != nil {
		t.Fatalf("create fixture org: %v", err)
	}
	domainID, err := st.CreateOrgDomain(ctx, orgID, domain, "ownership-token", "nerve", "", "", "cname")
	if err != nil {
		t.Fatalf("create fixture domain: %v", err)
	}
	return orgID, domainID
}

func TestLegacyDomainUnknownCreateIsPersistedBeforeIOAndNeverRecreated(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateLegacyDomainLifecycleFixture(t, ctx, db, 9)
		st := &Store{db: db, q: db}
		orgID, domainID := createLegacyDomainLifecycleFixture(t, ctx, st, "legacy-unknown-create", "Unknown.Example.")

		intent, err := st.BeginLegacyDomainProviderOperation(ctx, LegacyDomainProviderBegin{
			OrgID: orgID, OrgDomainID: domainID, Operation: LegacyDomainProviderCreate,
		})
		if err != nil {
			t.Fatalf("begin create: %v", err)
		}
		if intent.Disposition != LegacyDomainProviderCallAuthorized || intent.CanonicalDomain != "unknown.example" ||
			intent.ProviderDomainID != "" || !intent.ProviderStatusPresent || intent.ProviderStatus != "provider_unknown" ||
			intent.LeaseOwner == "" || intent.WorkflowVersion <= 1 {
			t.Fatalf("create intent=%+v", intent)
		}
		var status sql.NullString
		var claimVersion int64
		var leaseOwner sql.NullString
		if err := db.QueryRowContext(ctx, `
			SELECT d.resend_domain_status, c.workflow_version, c.lease_owner
			FROM org_domains d JOIN domain_ownership_claims c ON c.org_domain_id=d.id
			WHERE d.id=$1::uuid
		`, domainID).Scan(&status, &claimVersion, &leaseOwner); err != nil {
			t.Fatal(err)
		}
		if !status.Valid || status.String != "provider_unknown" || claimVersion != intent.WorkflowVersion ||
			!leaseOwner.Valid || leaseOwner.String != intent.LeaseOwner {
			t.Fatalf("pre-I/O fence status=%+v version=%d lease=%+v", status, claimVersion, leaseOwner)
		}

		if _, err := st.ApplyLegacyDomainProviderResult(ctx, LegacyDomainProviderResult{
			Intent: intent, Outcome: LegacyDomainProviderUnknown,
		}); err != nil {
			t.Fatalf("persist unknown create result: %v", err)
		}
		claim, err := st.GetDomainOwnershipClaim(ctx, "unknown.example")
		if err != nil {
			t.Fatal(err)
		}
		if claim.LeaseOwner.Valid || claim.LeaseExpiresAt.Valid || claim.WorkflowVersion != intent.WorkflowVersion {
			t.Fatalf("unknown result claim=%+v", claim)
		}
		retry, err := st.BeginLegacyDomainProviderOperation(ctx, LegacyDomainProviderBegin{
			OrgID: orgID, OrgDomainID: domainID, Operation: LegacyDomainProviderCreate,
		})
		if err != nil {
			t.Fatalf("classify create retry: %v", err)
		}
		if retry.Disposition != LegacyDomainProviderAwaitingProof || retry.WorkflowVersion != intent.WorkflowVersion ||
			retry.LeaseOwner != "" || !retry.ProviderAttempted {
			t.Fatalf("unknown create was made recreatable: %+v", retry)
		}
	})
}

func TestLegacyDomainUnknownResultOnlyCapturesIdentityAndPreservesReadiness(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateLegacyDomainLifecycleFixture(t, ctx, db, 9)
		st := &Store{db: db, q: db}

		pendingOrg, pendingDomain := createLegacyDomainLifecycleFixture(
			t, ctx, st, "legacy-unknown-identity", "unknown-identity.example",
		)
		createIntent, err := st.BeginLegacyDomainProviderOperation(ctx, LegacyDomainProviderBegin{
			OrgID: pendingOrg, OrgDomainID: pendingDomain, Operation: LegacyDomainProviderCreate,
		})
		if err != nil {
			t.Fatal(err)
		}
		truth := true
		captured, err := st.ApplyLegacyDomainProviderResult(ctx, LegacyDomainProviderResult{
			Intent: createIntent, Outcome: LegacyDomainProviderUnknown,
			ProviderDomainID: "rd-unknown-identity", ProviderCanonicalDomain: "unknown-identity.example",
			ProviderStatus: "verified", DNSRecords: json.RawMessage(`[{"record":"replacement"}]`),
			DomainState: "active", MXVerified: &truth, SPFVerified: &truth,
			DKIMVerified: &truth, DMARCVerified: &truth, ReceivingEnabled: &truth,
		})
		if err != nil {
			t.Fatalf("capture exact identity from unknown create: %v", err)
		}
		if captured.Domain.ResendDomainID.String != "rd-unknown-identity" ||
			captured.Domain.ResendStatus.String != "provider_unknown" || captured.Domain.Status != "pending" ||
			captured.Domain.MXVerified || captured.Domain.SPFVerified || captured.Domain.DKIMVerified ||
			captured.Domain.DMARCVerified || captured.Domain.ResendReceivingEnabled ||
			captured.Domain.InboundEnabled || len(captured.Domain.ResendDNSRecords) != 0 {
			t.Fatalf("unknown create projected non-identity evidence: %+v", captured.Domain)
		}
		claim, err := st.GetDomainOwnershipClaim(ctx, "unknown-identity.example")
		if err != nil || claim.State != "provider_owned" || claim.LeaseOwner.Valid {
			t.Fatalf("unknown exact-ID claim=%+v err=%v", claim, err)
		}

		activeOrg, activeDomain := createLegacyDomainLifecycleFixture(
			t, ctx, st, "legacy-unknown-readiness", "unknown-readiness.example",
		)
		if err := st.UpdateOrgDomainResend(ctx, activeDomain, "rd-unknown-readiness", "verified",
			[]byte(`[{"record":"original"}]`)); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateOrgDomainVerification(ctx, activeDomain, true, true, true, true, "active"); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateOrgDomainResendReceiving(ctx, activeDomain, true); err != nil {
			t.Fatal(err)
		}
		getIntent, err := st.BeginLegacyDomainProviderOperation(ctx, LegacyDomainProviderBegin{
			OrgID: activeOrg, OrgDomainID: activeDomain, Operation: LegacyDomainProviderGet,
		})
		if err != nil {
			t.Fatal(err)
		}
		falseValue := false
		preserved, err := st.ApplyLegacyDomainProviderResult(ctx, LegacyDomainProviderResult{
			Intent: getIntent, Outcome: LegacyDomainProviderUnknown,
			ProviderDomainID: "rd-unknown-readiness", ProviderCanonicalDomain: "unknown-readiness.example",
			ProviderStatus: "failed", DNSRecords: json.RawMessage(`[{"record":"replacement"}]`),
			DomainState: "failed", MXVerified: &falseValue, SPFVerified: &falseValue,
			DKIMVerified: &falseValue, DMARCVerified: &falseValue, ReceivingEnabled: &falseValue,
		})
		if err != nil {
			t.Fatalf("apply unknown active readback: %v", err)
		}
		if preserved.Domain.Status != "active" || preserved.Domain.ResendStatus.String != "provider_unknown" ||
			!preserved.Domain.MXVerified || !preserved.Domain.SPFVerified || !preserved.Domain.DKIMVerified ||
			!preserved.Domain.DMARCVerified || !preserved.Domain.ResendReceivingEnabled ||
			!preserved.Domain.InboundEnabled {
			t.Fatalf("unknown readback changed readiness: %+v", preserved.Domain)
		}
		var records []map[string]any
		if err := json.Unmarshal(preserved.Domain.ResendDNSRecords, &records); err != nil || len(records) != 1 ||
			records[0]["record"] != "original" {
			t.Fatalf("unknown readback replaced DNS evidence: records=%s err=%v", preserved.Domain.ResendDNSRecords, err)
		}
	})
}

func TestLegacyDomainDNSVerificationCannotResurrectReleaseFence(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateLegacyDomainLifecycleFixture(t, ctx, db, 9)
		st := &Store{db: db, q: db}
		orgID, domainID := createLegacyDomainLifecycleFixture(
			t, ctx, st, "legacy-dns-release-fence", "dns-release-fence.example",
		)

		release, err := st.BeginLegacyDomainProviderOperation(ctx, LegacyDomainProviderBegin{
			OrgID: orgID, OrgDomainID: domainID, Operation: LegacyDomainProviderDelete,
		})
		if err != nil || release.Disposition != LegacyDomainProviderLocalOnly ||
			release.DomainState != "failed" || release.ClaimState != "releasing" {
			t.Fatalf("begin release intent=%+v err=%v", release, err)
		}
		if err := st.UpdateOrgDomainVerification(ctx, domainID, true, true, true, true, "active"); !errors.Is(err, ErrResourceConflict) {
			t.Fatalf("stale DNS verification error=%v, want ErrResourceConflict", err)
		}
		domain, err := st.GetOrgDomainByID(ctx, domainID)
		if err != nil || domain.Status != "failed" || domain.MXVerified || domain.SPFVerified ||
			domain.DKIMVerified || domain.DMARCVerified {
			t.Fatalf("release-fenced domain resurrected: domain=%+v err=%v", domain, err)
		}
		claim, err := st.GetDomainOwnershipClaim(ctx, "dns-release-fence.example")
		if err != nil || claim.State != "releasing" || claim.WorkflowVersion != release.WorkflowVersion {
			t.Fatalf("release-fenced claim changed: claim=%+v err=%v", claim, err)
		}
	})
}

func TestLegacyDomainMalformedStoredProviderIdentityNeverAuthorizesIO(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateLegacyDomainLifecycleFixture(t, ctx, db, 9)
		st := &Store{db: db, q: db}
		orgID, domainID := createLegacyDomainLifecycleFixture(
			t, ctx, st, "legacy-malformed-provider-id", "malformed-provider-id.example",
		)
		const malformedID = " rd-unsafe "
		if _, err := db.ExecContext(ctx, `
			UPDATE org_domains
			SET resend_domain_id=$2, resend_domain_status='provider_unknown'
			WHERE id=$1::uuid
		`, domainID, malformedID); err != nil {
			t.Fatal(err)
		}

		read, err := st.BeginLegacyDomainProviderOperation(ctx, LegacyDomainProviderBegin{
			OrgID: orgID, OrgDomainID: domainID, Operation: LegacyDomainProviderGet,
		})
		if err != nil || read.Disposition != LegacyDomainProviderAwaitingProof ||
			read.ProviderDomainID != malformedID || read.LeaseOwner != "" {
			t.Fatalf("malformed read intent=%+v err=%v", read, err)
		}
		release, err := st.BeginLegacyDomainProviderOperation(ctx, LegacyDomainProviderBegin{
			OrgID: orgID, OrgDomainID: domainID, Operation: LegacyDomainProviderDelete,
		})
		if err != nil || release.Disposition != LegacyDomainProviderAwaitingProof ||
			release.ProviderDomainID != malformedID || release.LeaseOwner != "" ||
			release.DomainState != "failed" || release.ClaimState != "releasing" {
			t.Fatalf("malformed release intent=%+v err=%v", release, err)
		}
		if _, err := st.FinalizeLegacyDomainProviderAbsence(ctx, LegacyDomainProviderAbsenceReceipt{
			Intent: release, ProviderDomainID: "rd-unsafe", Proof: LegacyDomainExactIDAuthoritativeAbsence,
		}); !errors.Is(err, ErrLegacyDomainProviderAbsenceRequired) {
			t.Fatalf("normalized malformed identity absence error=%v", err)
		}
		domain, err := st.GetOrgDomainByID(ctx, domainID)
		if err != nil || !domain.ResendDomainID.Valid || domain.ResendDomainID.String != malformedID ||
			domain.Status != "failed" {
			t.Fatalf("malformed provider evidence changed: domain=%+v err=%v", domain, err)
		}
	})
}

func TestLegacyDomainProviderTypedOperationSequence(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateLegacyDomainLifecycleFixture(t, ctx, db, 9)
		st := &Store{db: db, q: db}
		orgID, domainID := createLegacyDomainLifecycleFixture(t, ctx, st, "legacy-operation-sequence", "operations.example")

		begin := func(operation LegacyDomainProviderOperation) LegacyDomainProviderIntent {
			t.Helper()
			intent, err := st.BeginLegacyDomainProviderOperation(ctx, LegacyDomainProviderBegin{
				OrgID: orgID, OrgDomainID: domainID, Operation: operation,
			})
			if err != nil || intent.Disposition != LegacyDomainProviderCallAuthorized {
				t.Fatalf("begin %s intent=%+v err=%v", operation, intent, err)
			}
			return intent
		}
		apply := func(intent LegacyDomainProviderIntent, result LegacyDomainProviderResult) OrgDomain {
			t.Helper()
			result.Intent = intent
			result.Outcome = LegacyDomainProviderObserved
			applied, err := st.ApplyLegacyDomainProviderResult(ctx, result)
			if err != nil {
				t.Fatalf("apply %s: %v", intent.Operation, err)
			}
			return applied.Domain
		}

		create := begin(LegacyDomainProviderCreate)
		created := apply(create, LegacyDomainProviderResult{
			ProviderDomainID: "rd-operations", ProviderCanonicalDomain: "operations.example",
			ProviderStatus: "pending", DNSRecords: json.RawMessage(`[]`),
		})
		if created.ResendDomainID.String != "rd-operations" || created.ResendStatus.String != "pending" {
			t.Fatalf("created provider projection=%+v", created)
		}
		replay, err := st.BeginLegacyDomainProviderOperation(ctx, LegacyDomainProviderBegin{
			OrgID: orgID, OrgDomainID: domainID, Operation: LegacyDomainProviderCreate,
		})
		if err != nil || replay.Disposition != LegacyDomainProviderMaterialized || replay.ProviderDomainID != "rd-operations" {
			t.Fatalf("materialized create replay=%+v err=%v", replay, err)
		}

		get := begin(LegacyDomainProviderGet)
		apply(get, LegacyDomainProviderResult{
			ProviderDomainID: "rd-operations", ProviderCanonicalDomain: "operations.example",
			ProviderStatus: "pending", DNSRecords: json.RawMessage(`[ {"type":"TXT","name":"send","value":"v=spf1"} ]`),
		})
		truth := true
		verify := begin(LegacyDomainProviderVerify)
		verified := apply(verify, LegacyDomainProviderResult{
			ProviderDomainID: "rd-operations", ProviderCanonicalDomain: "operations.example",
			ProviderStatus: "verified", DomainState: "verified_dns",
			MXVerified: &truth, SPFVerified: &truth, DKIMVerified: &truth,
		})
		if verified.Status != "verified_dns" || !verified.MXVerified || !verified.SPFVerified || !verified.DKIMVerified {
			t.Fatalf("verified projection=%+v", verified)
		}
		enable := begin(LegacyDomainProviderReceivingEnable)
		enabled := apply(enable, LegacyDomainProviderResult{
			ProviderDomainID: "rd-operations", ProviderStatus: "verified",
			DomainState: "active", ReceivingEnabled: &truth,
		})
		if enabled.Status != "active" || !enabled.ResendReceivingEnabled || !enabled.InboundEnabled {
			t.Fatalf("receiving-enable projection=%+v", enabled)
		}
		falseValue := false
		disable := begin(LegacyDomainProviderReceivingDisable)
		disabled := apply(disable, LegacyDomainProviderResult{
			ProviderDomainID: "rd-operations", ProviderStatus: "verified",
			ReceivingEnabled: &falseValue,
		})
		if disabled.ResendReceivingEnabled || disabled.InboundEnabled {
			t.Fatalf("receiving-disable projection=%+v", disabled)
		}
	})
}

func TestLegacyDomainObservedFailureMovesClaimToReleasing(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateLegacyDomainLifecycleFixture(t, ctx, db, 9)
		st := &Store{db: db, q: db}
		orgID, domainID := createLegacyDomainLifecycleFixture(t, ctx, st, "legacy-observed-failure", "observed-failure.example")
		if err := st.UpdateOrgDomainResend(ctx, domainID, "rd-observed-failure", "pending", []byte(`[]`)); err != nil {
			t.Fatal(err)
		}
		intent, err := st.BeginLegacyDomainProviderOperation(ctx, LegacyDomainProviderBegin{
			OrgID: orgID, OrgDomainID: domainID, Operation: LegacyDomainProviderGet,
		})
		if err != nil || intent.Disposition != LegacyDomainProviderCallAuthorized {
			t.Fatalf("begin get intent=%+v err=%v", intent, err)
		}

		applied, err := st.ApplyLegacyDomainProviderResult(ctx, LegacyDomainProviderResult{
			Intent: intent, Outcome: LegacyDomainProviderObserved,
			ProviderDomainID: "rd-observed-failure", ProviderCanonicalDomain: "observed-failure.example",
			ProviderStatus: "failed", DomainState: "failed",
		})
		if err != nil {
			t.Fatalf("apply observed failure: %v", err)
		}
		claim, err := st.GetDomainOwnershipClaim(ctx, "observed-failure.example")
		if err != nil || applied.Domain.Status != "failed" || claim.State != "releasing" ||
			claim.LeaseOwner.Valid || claim.LeaseExpiresAt.Valid || !applied.CompensatingCleanupRequired {
			t.Fatalf("observed failure domain=%+v claim=%+v applied=%+v err=%v", applied.Domain, claim, applied, err)
		}
	})
}

func TestLegacyDomainDefinitiveCreateRejectionReleasesExactIntent(t *testing.T) {
	for _, closeFirst := range []bool{false, true} {
		name := "result_first"
		if closeFirst {
			name = "close_first"
		}
		t.Run(name, func(t *testing.T) {
			withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
				migrateLegacyDomainLifecycleFixture(t, ctx, db, 9)
				st := &Store{db: db, q: db}
				domainLabel := strings.ReplaceAll(name, "_", "-")
				orgID, domainID := createLegacyDomainLifecycleFixture(
					t, ctx, st, "legacy-create-rejected-"+name, domainLabel+".create-rejected.example",
				)
				intent, err := st.BeginLegacyDomainProviderOperation(ctx, LegacyDomainProviderBegin{
					OrgID: orgID, OrgDomainID: domainID, Operation: LegacyDomainProviderCreate,
				})
				if err != nil || intent.Disposition != LegacyDomainProviderCallAuthorized {
					t.Fatalf("begin create intent=%+v err=%v", intent, err)
				}
				if _, err := st.FinalizeLegacyDomainCreateRejection(ctx, LegacyDomainCreateRejectionReceipt{
					Intent: intent, Proof: LegacyDomainExactIDAuthoritativeAbsence,
				}); !errors.Is(err, ErrLegacyDomainProviderAbsenceRequired) {
					t.Fatalf("wrong rejection proof accepted: %v", err)
				}
				if closeFirst {
					release, err := st.BeginLegacyDomainProviderOperation(ctx, LegacyDomainProviderBegin{
						OrgID: orgID, OrgDomainID: domainID, Operation: LegacyDomainProviderDelete,
					})
					if err != nil || release.Disposition != LegacyDomainProviderInFlight ||
						release.ClaimState != "releasing" || release.DomainState != "failed" ||
						release.WorkflowVersion != intent.WorkflowVersion+1 {
						t.Fatalf("close fence intent=%+v create=%+v err=%v", release, intent, err)
					}
				}
				deleted, err := st.FinalizeLegacyDomainCreateRejection(ctx, LegacyDomainCreateRejectionReceipt{
					Intent: intent, Proof: LegacyDomainCreateRejectedBeforeMaterialization,
				})
				if err != nil || !deleted {
					t.Fatalf("finalize create rejection deleted=%t err=%v", deleted, err)
				}
				if _, err := st.GetOrgDomainByIDForOrg(ctx, orgID, domainID); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("rejected domain remained: %v", err)
				}
				if _, err := st.GetDomainOwnershipClaim(ctx, intent.CanonicalDomain); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("rejected claim remained: %v", err)
				}
			})
		})
	}
}

func TestLegacyDomainLateCreateIdentityAndRejectionSurviveUnchangedLeaseExpiry(t *testing.T) {
	t.Run("exact identity is captured without readiness", func(t *testing.T) {
		withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
			migrateLegacyDomainLifecycleFixture(t, ctx, db, 9)
			st := &Store{db: db, q: db}
			orgID, domainID := createLegacyDomainLifecycleFixture(t, ctx, st, "legacy-late-create", "late-create.example")
			intent, err := st.BeginLegacyDomainProviderOperation(ctx, LegacyDomainProviderBegin{
				OrgID: orgID, OrgDomainID: domainID, Operation: LegacyDomainProviderCreate, LeaseTTL: time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(1100 * time.Millisecond)
			truth := true
			applied, err := st.ApplyLegacyDomainProviderResult(ctx, LegacyDomainProviderResult{
				Intent: intent, Outcome: LegacyDomainProviderObserved,
				ProviderDomainID: "rd-late-create", ProviderCanonicalDomain: "late-create.example",
				ProviderStatus: "verified", DomainState: "active", DNSRecords: json.RawMessage(`[ {"record":"unsafe"} ]`),
				MXVerified: &truth, SPFVerified: &truth, DKIMVerified: &truth,
				DMARCVerified: &truth, ReceivingEnabled: &truth,
			})
			if err != nil {
				t.Fatalf("capture late create: %v", err)
			}
			claim, err := st.GetDomainOwnershipClaim(ctx, intent.CanonicalDomain)
			if err != nil || !applied.StaleCreateCaptured || applied.CompensatingCleanupRequired ||
				applied.Domain.ResendDomainID.String != "rd-late-create" ||
				applied.Domain.ResendStatus.String != "provider_unknown" || applied.Domain.Status != "pending" ||
				applied.Domain.MXVerified || applied.Domain.SPFVerified || applied.Domain.DKIMVerified ||
				applied.Domain.DMARCVerified || applied.Domain.ResendReceivingEnabled || applied.Domain.InboundEnabled ||
				len(applied.Domain.ResendDNSRecords) != 0 || claim.State != "provider_owned" ||
				claim.LeaseOwner.Valid || claim.LeaseExpiresAt.Valid {
				t.Fatalf("late create applied=%+v claim=%+v err=%v", applied, claim, err)
			}
		})
	})

	t.Run("definitive rejection remains finalizable", func(t *testing.T) {
		withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
			migrateLegacyDomainLifecycleFixture(t, ctx, db, 9)
			st := &Store{db: db, q: db}
			orgID, domainID := createLegacyDomainLifecycleFixture(t, ctx, st, "legacy-late-rejection", "late-rejection.example")
			intent, err := st.BeginLegacyDomainProviderOperation(ctx, LegacyDomainProviderBegin{
				OrgID: orgID, OrgDomainID: domainID, Operation: LegacyDomainProviderCreate, LeaseTTL: time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(1100 * time.Millisecond)
			deleted, err := st.FinalizeLegacyDomainCreateRejection(ctx, LegacyDomainCreateRejectionReceipt{
				Intent: intent, Proof: LegacyDomainCreateRejectedBeforeMaterialization,
			})
			if err != nil || !deleted {
				t.Fatalf("finalize late create rejection deleted=%t err=%v", deleted, err)
			}
			if _, err := st.GetOrgDomainByIDForOrg(ctx, orgID, domainID); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("late rejected domain remained: %v", err)
			}
		})
	})
}

func TestLegacyDomainProviderApplyRollsBackOnProjectionFailureAndRejectsStaleVersion(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateLegacyDomainLifecycleFixture(t, ctx, db, 9)
		st := &Store{db: db, q: db}
		orgID, domainID := createLegacyDomainLifecycleFixture(t, ctx, st, "legacy-cas-rollback", "cas-rollback.example")
		if err := st.UpdateOrgDomainResend(ctx, domainID, "rd-cas", "pending", []byte(`[]`)); err != nil {
			t.Fatal(err)
		}
		intent, err := st.BeginLegacyDomainProviderOperation(ctx, LegacyDomainProviderBegin{
			OrgID: orgID, OrgDomainID: domainID, Operation: LegacyDomainProviderVerify,
		})
		if err != nil || intent.Disposition != LegacyDomainProviderCallAuthorized {
			t.Fatalf("begin verify intent=%+v err=%v", intent, err)
		}

		// A conflicting already-active row makes the domain projection fail after
		// the claim update. The transaction must retain the exact original lease.
		competitorID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `
			INSERT INTO org_domains(id, org_id, domain, status, verification_token)
			VALUES ($1::uuid, $2::uuid, 'cas-rollback.example', 'active', 'other-token')
		`, competitorID, orgID); err != nil {
			t.Fatalf("insert active projection conflict: %v", err)
		}
		truth := true
		_, applyErr := st.ApplyLegacyDomainProviderResult(ctx, LegacyDomainProviderResult{
			Intent: intent, Outcome: LegacyDomainProviderObserved,
			ProviderDomainID: "rd-cas", ProviderCanonicalDomain: "cas-rollback.example",
			ProviderStatus: "verified", DomainState: "active",
			MXVerified: &truth, SPFVerified: &truth, DKIMVerified: &truth,
		})
		if applyErr == nil {
			t.Fatal("conflicting active projection unexpectedly committed")
		}
		claim, err := st.GetDomainOwnershipClaim(ctx, "cas-rollback.example")
		if err != nil {
			t.Fatal(err)
		}
		domain, err := st.GetOrgDomainByIDForOrg(ctx, orgID, domainID)
		if err != nil {
			t.Fatal(err)
		}
		if claim.WorkflowVersion != intent.WorkflowVersion || !claim.LeaseOwner.Valid || claim.LeaseOwner.String != intent.LeaseOwner ||
			claim.State != intent.ClaimState || domain.Status != "pending" || domain.ResendStatus.String != "pending" ||
			domain.MXVerified || domain.SPFVerified || domain.DKIMVerified {
			t.Fatalf("failed projection partially committed claim=%+v domain=%+v", claim, domain)
		}

		if _, err := db.ExecContext(ctx, `
			UPDATE domain_ownership_claims
			SET workflow_version=workflow_version+1
			WHERE canonical_domain='cas-rollback.example'
		`); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ApplyLegacyDomainProviderResult(ctx, LegacyDomainProviderResult{
			Intent: intent, Outcome: LegacyDomainProviderObserved,
			ProviderDomainID: "rd-cas", ProviderCanonicalDomain: "cas-rollback.example",
			ProviderStatus: "verified",
		}); !errors.Is(err, ErrLegacyDomainProviderCASConflict) {
			t.Fatalf("stale workflow result error=%v", err)
		}
		domain, err = st.GetOrgDomainByIDForOrg(ctx, orgID, domainID)
		if err != nil || domain.Status != "pending" || domain.ResendStatus.String != "pending" {
			t.Fatalf("stale result changed domain=%+v err=%v", domain, err)
		}
	})
}

func TestLegacyDomainCreateReleaseRaceCapturesExactIDWithoutReactivation(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateLegacyDomainLifecycleFixture(t, ctx, db, 9)
		st := &Store{db: db, q: db}
		orgID, domainID := createLegacyDomainLifecycleFixture(t, ctx, st, "legacy-release-race", "release-race.example")

		createIntent, err := st.BeginLegacyDomainProviderOperation(ctx, LegacyDomainProviderBegin{
			OrgID: orgID, OrgDomainID: domainID, Operation: LegacyDomainProviderCreate,
		})
		if err != nil {
			t.Fatal(err)
		}
		releaseIntent, err := st.BeginLegacyDomainProviderOperation(ctx, LegacyDomainProviderBegin{
			OrgID: orgID, OrgDomainID: domainID, Operation: LegacyDomainProviderDelete,
		})
		if err != nil {
			t.Fatalf("fence create with release: %v", err)
		}
		if releaseIntent.Disposition != LegacyDomainProviderInFlight || releaseIntent.ClaimState != "releasing" ||
			releaseIntent.DomainState != "failed" || releaseIntent.WorkflowVersion != createIntent.WorkflowVersion+1 ||
			releaseIntent.LeaseOwner != createIntent.LeaseOwner {
			t.Fatalf("release fence intent=%+v create=%+v", releaseIntent, createIntent)
		}

		captured, err := st.ApplyLegacyDomainProviderResult(ctx, LegacyDomainProviderResult{
			Intent: createIntent, Outcome: LegacyDomainProviderObserved,
			ProviderDomainID: "rd-release-race", ProviderCanonicalDomain: "release-race.example",
			ProviderStatus: "pending", DNSRecords: json.RawMessage(`[ {"type":"TXT","name":"send","value":"v=spf1"} ]`),
			DomainState: "active",
		})
		if err != nil {
			t.Fatalf("capture stale create success: %v", err)
		}
		if !captured.StaleCreateCaptured || !captured.CompensatingCleanupRequired ||
			captured.Domain.Status != "failed" || captured.Domain.ResendDomainID.String != "rd-release-race" ||
			captured.Domain.ResendReceivingEnabled {
			t.Fatalf("stale capture=%+v", captured)
		}
		claim, err := st.GetDomainOwnershipClaim(ctx, "release-race.example")
		if err != nil || claim.State != "releasing" || claim.LeaseOwner.Valid ||
			claim.WorkflowVersion != releaseIntent.WorkflowVersion {
			t.Fatalf("stale capture claim=%+v err=%v", claim, err)
		}

		deleteIntent, err := st.BeginLegacyDomainProviderOperation(ctx, LegacyDomainProviderBegin{
			OrgID: orgID, OrgDomainID: domainID, Operation: LegacyDomainProviderDelete,
		})
		if err != nil || deleteIntent.Disposition != LegacyDomainProviderCallAuthorized ||
			deleteIntent.ProviderDomainID != "rd-release-race" {
			t.Fatalf("begin exact-id compensation=%+v err=%v", deleteIntent, err)
		}
		if _, err := st.FinalizeLegacyDomainProviderAbsence(ctx, LegacyDomainProviderAbsenceReceipt{
			Intent: deleteIntent, ProviderDomainID: "rd-release-race",
		}); !errors.Is(err, ErrLegacyDomainProviderAbsenceRequired) {
			t.Fatalf("DELETE acceptance without readback proof error=%v", err)
		}
		if _, err := st.GetOrgDomainByID(ctx, domainID); err != nil {
			t.Fatalf("unproven delete removed local domain: %v", err)
		}
		deleted, err := st.FinalizeLegacyDomainProviderAbsence(ctx, LegacyDomainProviderAbsenceReceipt{
			Intent: deleteIntent, ProviderDomainID: "rd-release-race",
			Proof: LegacyDomainExactIDAuthoritativeAbsence,
		})
		if err != nil || !deleted {
			t.Fatalf("finalize exact-id absence deleted=%t err=%v", deleted, err)
		}
		if _, err := st.GetOrgDomainByID(ctx, domainID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("finalized domain lookup error=%v", err)
		}
		if _, err := st.GetDomainOwnershipClaim(ctx, "release-race.example"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("finalized claim lookup error=%v", err)
		}
	})
}

func TestLegacyDomainExpiredCreateReleaseCapturesExactIDWithoutReactivation(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateLegacyDomainLifecycleFixture(t, ctx, db, 9)
		st := &Store{db: db, q: db}
		orgID, domainID := createLegacyDomainLifecycleFixture(
			t, ctx, st, "legacy-expired-create-release", "expired-create-release.example",
		)

		createIntent, err := st.BeginLegacyDomainProviderOperation(ctx, LegacyDomainProviderBegin{
			OrgID: orgID, OrgDomainID: domainID, Operation: LegacyDomainProviderCreate, LeaseTTL: time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(3 * time.Second)
		for {
			var expired bool
			if err := db.QueryRowContext(ctx, `
				SELECT lease_expires_at <= clock_timestamp()
				FROM domain_ownership_claims WHERE canonical_domain='expired-create-release.example'
			`).Scan(&expired); err != nil {
				t.Fatal(err)
			}
			if expired {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("create lease did not expire")
			}
			time.Sleep(10 * time.Millisecond)
		}

		releaseIntent, err := st.BeginLegacyDomainProviderOperation(ctx, LegacyDomainProviderBegin{
			OrgID: orgID, OrgDomainID: domainID, Operation: LegacyDomainProviderDelete,
		})
		if err != nil {
			t.Fatalf("fence expired create: %v", err)
		}
		if releaseIntent.Disposition != LegacyDomainProviderAwaitingProof || releaseIntent.ClaimState != "releasing" ||
			releaseIntent.DomainState != "failed" || releaseIntent.WorkflowVersion != createIntent.WorkflowVersion+1 ||
			releaseIntent.LeaseOwner != createIntent.LeaseOwner ||
			!releaseIntent.LeaseExpiresAt.Equal(createIntent.LeaseExpiresAt) {
			t.Fatalf("expired release fence=%+v create=%+v", releaseIntent, createIntent)
		}
		replay, err := st.BeginLegacyDomainProviderOperation(ctx, LegacyDomainProviderBegin{
			OrgID: orgID, OrgDomainID: domainID, Operation: LegacyDomainProviderDelete,
		})
		if err != nil || replay.Disposition != LegacyDomainProviderAwaitingProof ||
			replay.WorkflowVersion != releaseIntent.WorkflowVersion || replay.LeaseOwner != createIntent.LeaseOwner {
			t.Fatalf("expired release replay=%+v err=%v", replay, err)
		}

		captured, err := st.ApplyLegacyDomainProviderResult(ctx, LegacyDomainProviderResult{
			Intent: createIntent, Outcome: LegacyDomainProviderObserved,
			ProviderDomainID: "rd-expired-create", ProviderCanonicalDomain: "expired-create-release.example",
			ProviderStatus: "pending", DomainState: "active",
		})
		if err != nil {
			t.Fatalf("capture expired stale create: %v", err)
		}
		if !captured.StaleCreateCaptured || !captured.CompensatingCleanupRequired ||
			captured.Domain.Status != "failed" || captured.Domain.ResendDomainID.String != "rd-expired-create" ||
			captured.Domain.ResendReceivingEnabled || captured.Domain.InboundEnabled {
			t.Fatalf("expired stale create capture=%+v", captured)
		}
		claim, err := st.GetDomainOwnershipClaim(ctx, "expired-create-release.example")
		if err != nil || claim.State != "releasing" || claim.WorkflowVersion != releaseIntent.WorkflowVersion ||
			claim.LeaseOwner.Valid || claim.LeaseExpiresAt.Valid {
			t.Fatalf("expired stale create claim=%+v err=%v", claim, err)
		}
	})
}

func TestLegacyDomainLocalOnlyReleaseRequiresNoProviderAttempt(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateLegacyDomainLifecycleFixture(t, ctx, db, 9)
		st := &Store{db: db, q: db}
		localOrg, localDomain := createLegacyDomainLifecycleFixture(t, ctx, st, "legacy-local-only", "local-only.example")
		localIntent, err := st.BeginLegacyDomainProviderOperation(ctx, LegacyDomainProviderBegin{
			OrgID: localOrg, OrgDomainID: localDomain, Operation: LegacyDomainProviderDelete,
		})
		if err != nil || localIntent.Disposition != LegacyDomainProviderLocalOnly || localIntent.ProviderAttempted {
			t.Fatalf("begin local-only release=%+v err=%v", localIntent, err)
		}
		deleted, err := st.FinalizeLegacyDomainLocalOnlyRelease(ctx, localIntent)
		if err != nil || !deleted {
			t.Fatalf("finalize local-only release deleted=%t err=%v", deleted, err)
		}

		unknownOrg, unknownDomain := createLegacyDomainLifecycleFixture(t, ctx, st, "legacy-attempted", "attempted.example")
		createIntent, err := st.BeginLegacyDomainProviderOperation(ctx, LegacyDomainProviderBegin{
			OrgID: unknownOrg, OrgDomainID: unknownDomain, Operation: LegacyDomainProviderCreate,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.ApplyLegacyDomainProviderResult(ctx, LegacyDomainProviderResult{
			Intent: createIntent, Outcome: LegacyDomainProviderUnknown,
		}); err != nil {
			t.Fatal(err)
		}
		unknownRelease, err := st.BeginLegacyDomainProviderOperation(ctx, LegacyDomainProviderBegin{
			OrgID: unknownOrg, OrgDomainID: unknownDomain, Operation: LegacyDomainProviderDelete,
		})
		if err != nil || unknownRelease.Disposition != LegacyDomainProviderAwaitingProof ||
			!unknownRelease.ProviderAttempted {
			t.Fatalf("unknown release=%+v err=%v", unknownRelease, err)
		}
		if _, err := st.FinalizeLegacyDomainLocalOnlyRelease(ctx, unknownRelease); !errors.Is(err, ErrLegacyDomainProviderAbsenceRequired) {
			t.Fatalf("attempted create accepted local-only release: %v", err)
		}
		if _, err := st.FinalizeOrgDomainReleaseForOrg(ctx, unknownOrg, unknownDomain); !errors.Is(err, ErrLegacyDomainProviderAbsenceRequired) {
			t.Fatalf("compatibility finalizer bypassed provider proof: %v", err)
		}
		if domain, err := st.GetOrgDomainByID(ctx, unknownDomain); err != nil || domain.Status != "failed" ||
			domain.ResendStatus.String != "provider_unknown" {
			t.Fatalf("unresolved attempted domain=%+v err=%v", domain, err)
		}
	})
}

func TestLegacyDomainLocalOnlyReleaseCanBeReclaimedAfterLeaseExpiry(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateLegacyDomainLifecycleFixture(t, ctx, db, 9)
		st := &Store{db: db, q: db}
		orgID, domainID := createLegacyDomainLifecycleFixture(t, ctx, st, "legacy-local-only-reclaim", "reclaim-local-only.example")

		first, err := st.BeginLegacyDomainProviderOperation(ctx, LegacyDomainProviderBegin{
			OrgID: orgID, OrgDomainID: domainID, Operation: LegacyDomainProviderDelete,
		})
		if err != nil || first.Disposition != LegacyDomainProviderLocalOnly || first.ProviderAttempted {
			t.Fatalf("begin local-only release=%+v err=%v", first, err)
		}

		if _, err := db.ExecContext(ctx, `
			UPDATE domain_ownership_claims
			SET lease_expires_at=clock_timestamp()-interval '1 second'
			WHERE canonical_domain=$1 AND org_domain_id=$2::uuid AND org_id=$3::uuid
		`, first.CanonicalDomain, domainID, orgID); err != nil {
			t.Fatalf("expire local-only lease: %v", err)
		}

		reclaimed, err := st.BeginLegacyDomainProviderOperation(ctx, LegacyDomainProviderBegin{
			OrgID: orgID, OrgDomainID: domainID, Operation: LegacyDomainProviderDelete,
		})
		if err != nil || reclaimed.Disposition != LegacyDomainProviderLocalOnly || reclaimed.ProviderAttempted ||
			reclaimed.LeaseOwner == first.LeaseOwner || reclaimed.WorkflowVersion != first.WorkflowVersion+1 {
			t.Fatalf("reclaim local-only release=%+v first=%+v err=%v", reclaimed, first, err)
		}

		deleted, err := st.FinalizeLegacyDomainLocalOnlyRelease(ctx, reclaimed)
		if err != nil || !deleted {
			t.Fatalf("finalize reclaimed local-only release deleted=%t err=%v", deleted, err)
		}
	})
}

func TestLegacyDomainLocalOnlyLeaseRecoveryRejectsMalformedEvidence(t *testing.T) {
	validDeleteOwner := makeLegacyDomainProviderLeaseOwner(LegacyDomainProviderDelete)
	now := time.Now().UTC()
	tests := []struct {
		name   string
		domain OrgDomain
		claim  DomainOwnershipClaim
	}{
		{name: "malformed owner", claim: DomainOwnershipClaim{
			State: "releasing", LeaseOwner: sql.NullString{String: legacyDomainProviderLeasePrefix + "delete:not-a-uuid", Valid: true},
			LeaseExpiresAt: sql.NullTime{Time: now, Valid: true},
		}},
		{name: "empty owner suffix", claim: DomainOwnershipClaim{
			State: "releasing", LeaseOwner: sql.NullString{String: legacyDomainProviderLeasePrefix + "delete:", Valid: true},
			LeaseExpiresAt: sql.NullTime{Time: now, Valid: true},
		}},
		{name: "unknown operation", claim: DomainOwnershipClaim{
			State: "releasing", LeaseOwner: sql.NullString{String: legacyDomainProviderLeasePrefix + "unknown:" + uuid.NewString(), Valid: true},
			LeaseExpiresAt: sql.NullTime{Time: now, Valid: true},
		}},
		{name: "owner without expiry", claim: DomainOwnershipClaim{
			State: "releasing", LeaseOwner: sql.NullString{String: validDeleteOwner, Valid: true},
		}},
		{name: "expiry without owner", claim: DomainOwnershipClaim{
			State: "releasing", LeaseExpiresAt: sql.NullTime{Time: now, Valid: true},
		}},
		{name: "delete lease on pending claim", claim: DomainOwnershipClaim{
			State: "pending", LeaseOwner: sql.NullString{String: validDeleteOwner, Valid: true},
			LeaseExpiresAt: sql.NullTime{Time: now, Valid: true},
		}},
		{name: "delete lease beside pending domain", domain: OrgDomain{Status: "pending"}, claim: DomainOwnershipClaim{
			State: "releasing", LeaseOwner: sql.NullString{String: validDeleteOwner, Valid: true},
			LeaseExpiresAt: sql.NullTime{Time: now, Valid: true},
		}},
		{name: "zero lease expiry", domain: OrgDomain{Status: "failed"}, claim: DomainOwnershipClaim{
			State: "releasing", LeaseOwner: sql.NullString{String: validDeleteOwner, Valid: true},
			LeaseExpiresAt: sql.NullTime{Valid: true},
		}},
		{name: "blank valid provider id", domain: OrgDomain{
			Status: "failed", ResendDomainID: sql.NullString{String: " ", Valid: true},
		}, claim: DomainOwnershipClaim{
			State: "releasing", LeaseOwner: sql.NullString{String: validDeleteOwner, Valid: true},
			LeaseExpiresAt: sql.NullTime{Time: now, Valid: true},
		}},
		{name: "inbound enabled without receiving evidence", domain: OrgDomain{
			Status: "failed", InboundEnabled: true,
		}, claim: DomainOwnershipClaim{
			State: "releasing", LeaseOwner: sql.NullString{String: validDeleteOwner, Valid: true},
			LeaseExpiresAt: sql.NullTime{Time: now, Valid: true},
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			operation, known := legacyDomainProviderLeaseOperation(test.claim.LeaseOwner.String)
			if legacyDomainRecoverableLocalOnlyDeleteLease(test.domain, test.claim) ||
				!legacyDomainProviderEvidenceAttempted(test.domain, test.claim) {
				t.Fatalf("malformed evidence treated as local-only: operation=%q known=%t domain=%+v claim=%+v", operation, known, test.domain, test.claim)
			}
		})
	}
}

func TestListLegacyDomainCleanupDueFindsOnlyUnleasedFailedReleases(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateLegacyDomainLifecycleFixture(t, ctx, db, 9)
		st := &Store{db: db, q: db}
		orgID, domainID := createLegacyDomainLifecycleFixture(t, ctx, st, "legacy-expiry-cleanup", "expiry-cleanup.example")
		if err := st.UpdateOrgDomainResend(ctx, domainID, "rd-expiry-cleanup", "pending", []byte(`[]`)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `
			UPDATE org_domains SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1::uuid
		`, domainID); err != nil {
			t.Fatal(err)
		}
		if expired, err := st.ExpirePendingDomains(ctx); err != nil || expired != 1 {
			t.Fatalf("expire pending legacy domain count=%d err=%v", expired, err)
		}

		candidates, err := st.ListLegacyDomainCleanupDue(ctx, 1)
		if err != nil || len(candidates) != 1 || candidates[0].OrgID != orgID || candidates[0].OrgDomainID != domainID {
			t.Fatalf("cleanup candidates=%+v err=%v", candidates, err)
		}
		intent, err := st.BeginLegacyDomainProviderOperation(ctx, LegacyDomainProviderBegin{
			OrgID: orgID, OrgDomainID: domainID, Operation: LegacyDomainProviderDelete,
		})
		if err != nil || intent.Disposition != LegacyDomainProviderCallAuthorized {
			t.Fatalf("claim cleanup intent=%+v err=%v", intent, err)
		}
		if candidates, err := st.ListLegacyDomainCleanupDue(ctx, 100); err != nil || len(candidates) != 0 {
			t.Fatalf("live lease remained due candidates=%+v err=%v", candidates, err)
		}
		if _, err := db.ExecContext(ctx, `
			UPDATE domain_ownership_claims
			SET lease_expires_at=clock_timestamp()-interval '1 second'
			WHERE canonical_domain=$1 AND org_domain_id=$2::uuid AND org_id=$3::uuid
		`, intent.CanonicalDomain, domainID, orgID); err != nil {
			t.Fatal(err)
		}
		if candidates, err := st.ListLegacyDomainCleanupDue(ctx, 100); err != nil || len(candidates) != 1 ||
			candidates[0].OrgDomainID != domainID {
			t.Fatalf("expired lease cleanup candidates=%+v err=%v", candidates, err)
		}
	})
}

func TestListLegacyDomainCleanupDueIsNoopBeforeCloudSchemaNine(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateLegacyDomainLifecycleFixture(t, ctx, db, 8)
		st := &Store{db: db, q: db}
		candidates, err := st.ListLegacyDomainCleanupDue(ctx, 100)
		if err != nil || candidates != nil {
			t.Fatalf("schema-8 cleanup discovery=%+v err=%v", candidates, err)
		}
	})
}

func prepareAmbiguousLegacyDomainCleanup(
	t *testing.T,
	ctx context.Context,
	st *Store,
	name string,
	domain string,
) (string, string) {
	t.Helper()
	orgID, domainID := createLegacyDomainLifecycleFixture(t, ctx, st, name, domain)
	createIntent, err := st.BeginLegacyDomainProviderOperation(ctx, LegacyDomainProviderBegin{
		OrgID: orgID, OrgDomainID: domainID, Operation: LegacyDomainProviderCreate,
	})
	if err != nil {
		t.Fatalf("begin ambiguous create: %v", err)
	}
	if _, err := st.ApplyLegacyDomainProviderResult(ctx, LegacyDomainProviderResult{
		Intent: createIntent, Outcome: LegacyDomainProviderUnknown,
	}); err != nil {
		t.Fatalf("persist ambiguous create: %v", err)
	}
	release, err := st.BeginLegacyDomainProviderOperation(ctx, LegacyDomainProviderBegin{
		OrgID: orgID, OrgDomainID: domainID, Operation: LegacyDomainProviderDelete,
	})
	if err != nil || release.Disposition != LegacyDomainProviderAwaitingProof {
		t.Fatalf("fence ambiguous cleanup intent=%+v err=%v", release, err)
	}
	return orgID, domainID
}

func prepareExactIDLegacyDomainCleanup(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	st *Store,
	name string,
	domain string,
	providerID string,
) (string, string) {
	t.Helper()
	orgID, domainID := createLegacyDomainLifecycleFixture(t, ctx, st, name, domain)
	if err := st.UpdateOrgDomainResend(ctx, domainID, providerID, "pending", []byte(`[]`)); err != nil {
		t.Fatalf("persist exact provider identity: %v", err)
	}
	release, err := st.BeginLegacyDomainProviderOperation(ctx, LegacyDomainProviderBegin{
		OrgID: orgID, OrgDomainID: domainID, Operation: LegacyDomainProviderDelete,
	})
	if err != nil || release.Disposition != LegacyDomainProviderCallAuthorized {
		t.Fatalf("fence exact-id cleanup intent=%+v err=%v", release, err)
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE domain_ownership_claims
		SET lease_expires_at=clock_timestamp()-interval '1 second'
		WHERE canonical_domain=$1 AND org_domain_id=$2::uuid AND org_id=$3::uuid
	`, release.CanonicalDomain, domainID, orgID); err != nil {
		t.Fatalf("expire initial exact-id cleanup lease: %v", err)
	}
	return orgID, domainID
}

func TestLegacyDomainCleanupClaimRotatesAwaitingProofBacklogBehindExactIDWork(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateLegacyDomainLifecycleFixture(t, ctx, db, 9)
		st := &Store{db: db, q: db}

		type fixture struct {
			orgID     string
			domainID  string
			canonical string
		}
		ambiguous := make([]fixture, 0, 3)
		for index := 0; index < 3; index++ {
			canonical := fmt.Sprintf("cleanup-ambiguous-%d.example", index)
			orgID, domainID := prepareAmbiguousLegacyDomainCleanup(
				t, ctx, st, fmt.Sprintf("cleanup-ambiguous-%d", index), canonical,
			)
			ambiguous = append(ambiguous, fixture{orgID: orgID, domainID: domainID, canonical: canonical})
		}
		exactOrgID, exactDomainID := prepareExactIDLegacyDomainCleanup(
			t, ctx, db, st, "cleanup-exact-later", "cleanup-exact-later.example", "rd-cleanup-exact-later",
		)

		recordLegacyDomainLifecycleOpenQuarantine(
			t, ctx, db, ambiguous[0].canonical, "rd-quarantine-observation",
			ambiguous[0].domainID, "provider_only", strings.Repeat("a", 64),
		)
		for index, item := range ambiguous {
			if _, err := db.ExecContext(ctx, `
				UPDATE domain_ownership_claims
				SET updated_at=clock_timestamp()-make_interval(hours => $2)
				WHERE canonical_domain=$1
			`, item.canonical, 10-index); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.ExecContext(ctx, `
			UPDATE domain_ownership_claims SET updated_at=clock_timestamp()-interval '1 hour'
			WHERE canonical_domain='cleanup-exact-later.example'
		`); err != nil {
			t.Fatal(err)
		}

		firstPage, err := st.ListLegacyDomainCleanupDue(ctx, 2)
		if err != nil || len(firstPage) != 2 {
			t.Fatalf("first bounded cleanup page=%+v err=%v", firstPage, err)
		}
		if firstPage[0].CanonicalDomain != ambiguous[0].canonical || !firstPage[0].HasOpenQuarantine ||
			firstPage[1].CanonicalDomain != ambiguous[1].canonical {
			t.Fatalf("unexpected first cleanup page=%+v", firstPage)
		}
		for _, candidate := range firstPage {
			claim, claimed, err := st.ClaimLegacyDomainCleanup(ctx, candidate, time.Minute)
			if err != nil || !claimed || claim.Intent.Disposition != LegacyDomainProviderAwaitingProof ||
				claim.CleanupNotBefore.IsZero() {
				t.Fatalf("claim awaiting-proof candidate=%+v claimed=%t err=%v", claim, claimed, err)
			}
		}

		secondPage, err := st.ListLegacyDomainCleanupDue(ctx, 2)
		if err != nil || len(secondPage) != 2 || secondPage[0].CanonicalDomain != ambiguous[2].canonical ||
			secondPage[1].OrgID != exactOrgID || secondPage[1].OrgDomainID != exactDomainID {
			t.Fatalf("rotated cleanup page=%+v err=%v", secondPage, err)
		}
		ambiguousClaim, claimed, err := st.ClaimLegacyDomainCleanup(ctx, secondPage[0], time.Minute)
		if err != nil || !claimed || ambiguousClaim.Intent.Disposition != LegacyDomainProviderAwaitingProof {
			t.Fatalf("claim last ambiguous=%+v claimed=%t err=%v", ambiguousClaim, claimed, err)
		}
		exactClaim, claimed, err := st.ClaimLegacyDomainCleanup(ctx, secondPage[1], time.Minute)
		if err != nil || !claimed || exactClaim.Intent.Disposition != LegacyDomainProviderCallAuthorized ||
			exactClaim.Intent.ProviderDomainID != "rd-cleanup-exact-later" ||
			!exactClaim.CleanupNotBefore.IsZero() {
			t.Fatalf("claim later exact-id=%+v claimed=%t err=%v", exactClaim, claimed, err)
		}
	})
}

func TestLegacyDomainCleanupClaimFencesOpenQuarantineFromExactIDMutation(t *testing.T) {
	for _, test := range []struct {
		name                 string
		openAfterDiscovery   bool
		expectFirstClaimLost bool
	}{
		{name: "already open", openAfterDiscovery: false, expectFirstClaimLost: false},
		{name: "opens after discovery", openAfterDiscovery: true, expectFirstClaimLost: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
				migrateLegacyDomainLifecycleFixture(t, ctx, db, 9)
				st := &Store{db: db, q: db}
				orgID, domainID := prepareExactIDLegacyDomainCleanup(
					t, ctx, db, st, "cleanup-quarantine-"+strings.ReplaceAll(test.name, " ", "-"),
					"cleanup-quarantine-"+strings.ReplaceAll(test.name, " ", "-")+".example",
					"rd-cleanup-quarantine-local",
				)
				canonical := "cleanup-quarantine-" + strings.ReplaceAll(test.name, " ", "-") + ".example"

				recordQuarantine := func() {
					t.Helper()
					recordLegacyDomainLifecycleOpenQuarantine(
						t, ctx, db, canonical, "rd-cleanup-quarantine-conflict",
						domainID, "provider_id_mismatch", strings.Repeat("b", 64),
					)
				}
				if !test.openAfterDiscovery {
					recordQuarantine()
				}
				candidates, err := st.ListLegacyDomainCleanupDue(ctx, 1)
				if err != nil || len(candidates) != 1 || candidates[0].OrgID != orgID ||
					candidates[0].HasOpenQuarantine == test.openAfterDiscovery {
					t.Fatalf("quarantine discovery=%+v err=%v", candidates, err)
				}
				if test.openAfterDiscovery {
					recordQuarantine()
				}

				claim, claimed, err := st.ClaimLegacyDomainCleanup(ctx, candidates[0], time.Minute)
				if err != nil {
					t.Fatalf("claim exact-id quarantine snapshot: %v", err)
				}
				if test.expectFirstClaimLost {
					if claimed {
						t.Fatalf("stale pre-quarantine candidate authorized claim=%+v", claim)
					}
					candidates, err = st.ListLegacyDomainCleanupDue(ctx, 1)
					if err != nil || len(candidates) != 1 || !candidates[0].HasOpenQuarantine {
						t.Fatalf("post-race quarantine discovery=%+v err=%v", candidates, err)
					}
					claim, claimed, err = st.ClaimLegacyDomainCleanup(ctx, candidates[0], time.Minute)
					if err != nil {
						t.Fatalf("claim refreshed quarantine snapshot: %v", err)
					}
				}
				if !claimed || claim.Intent.Disposition != LegacyDomainProviderAwaitingProof ||
					claim.Intent.ProviderDomainID != "rd-cleanup-quarantine-local" ||
					claim.CleanupNotBefore.IsZero() {
					t.Fatalf("open quarantine authorized mutation claim=%+v claimed=%t", claim, claimed)
				}
				persisted, err := st.GetDomainOwnershipClaim(ctx, canonical)
				if err != nil || persisted.WorkflowVersion != candidates[0].WorkflowVersion ||
					!legacyDomainCleanupNullStringEqual(persisted.LeaseOwner, candidates[0].LeaseOwner) ||
					!legacyDomainCleanupNullTimeEqual(persisted.LeaseExpiresAt, candidates[0].LeaseExpiresAt) ||
					!persisted.ClaimExpiresAt.Valid || !persisted.ClaimExpiresAt.Time.Equal(claim.CleanupNotBefore) {
					t.Fatalf("quarantine scheduling mutated provider lease claim=%+v persisted=%+v err=%v", claim, persisted, err)
				}
			})
		})
	}
}

func TestLegacyDomainAwaitingProofCleanupClaimConcurrentTakeoverAndDeferCAS(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateLegacyDomainLifecycleFixture(t, ctx, db, 9)
		st := &Store{db: db, q: db}
		_, domainID := prepareAmbiguousLegacyDomainCleanup(
			t, ctx, st, "cleanup-awaiting-concurrent", "cleanup-awaiting-concurrent.example",
		)
		candidates, err := st.ListLegacyDomainCleanupDue(ctx, 1)
		if err != nil || len(candidates) != 1 || candidates[0].OrgDomainID != domainID {
			t.Fatalf("initial awaiting candidate=%+v err=%v", candidates, err)
		}

		type outcome struct {
			claim   LegacyDomainCleanupClaim
			claimed bool
			err     error
		}
		start := make(chan struct{})
		outcomes := make(chan outcome, 2)
		for index := 0; index < 2; index++ {
			go func() {
				<-start
				claim, claimed, err := st.ClaimLegacyDomainCleanup(ctx, candidates[0], time.Minute)
				outcomes <- outcome{claim: claim, claimed: claimed, err: err}
			}()
		}
		close(start)
		var winner LegacyDomainCleanupClaim
		claimedCount := 0
		for index := 0; index < 2; index++ {
			result := <-outcomes
			if result.err != nil {
				t.Fatalf("concurrent awaiting claim: %v", result.err)
			}
			if result.claimed {
				winner = result.claim
				claimedCount++
			}
		}
		if claimedCount != 1 || winner.CleanupNotBefore.IsZero() {
			t.Fatalf("concurrent awaiting winners=%d claim=%+v", claimedCount, winner)
		}

		deferred, err := st.DeferLegacyDomainCleanup(ctx, winner, 2*time.Minute)
		if err != nil || !deferred.CleanupNotBefore.After(winner.CleanupNotBefore) {
			t.Fatalf("defer awaiting claim=%+v err=%v", deferred, err)
		}
		if _, err := st.DeferLegacyDomainCleanup(ctx, winner, 3*time.Minute); !errors.Is(err, ErrLegacyDomainProviderCASConflict) {
			t.Fatalf("stale defer err=%v", err)
		}
		if _, err := st.DeferLegacyDomainCleanup(ctx, deferred, legacyDomainCleanupMaxDelay+time.Second); err == nil {
			t.Fatal("unbounded cleanup deferral was accepted")
		}
		if due, err := st.ListLegacyDomainCleanupDue(ctx, 1); err != nil || len(due) != 0 {
			t.Fatalf("deferred cleanup remained due=%+v err=%v", due, err)
		}

		if _, err := db.ExecContext(ctx, `
			UPDATE domain_ownership_claims
			SET claim_expires_at=clock_timestamp()-interval '1 second'
			WHERE canonical_domain='cleanup-awaiting-concurrent.example'
		`); err != nil {
			t.Fatal(err)
		}
		takeoverCandidates, err := st.ListLegacyDomainCleanupDue(ctx, 1)
		if err != nil || len(takeoverCandidates) != 1 {
			t.Fatalf("expired scheduling claim candidates=%+v err=%v", takeoverCandidates, err)
		}
		takeover, claimed, err := st.ClaimLegacyDomainCleanup(ctx, takeoverCandidates[0], time.Minute)
		if err != nil || !claimed || !takeover.CleanupNotBefore.After(time.Now()) ||
			takeover.CleanupNotBefore.Equal(deferred.CleanupNotBefore) {
			t.Fatalf("awaiting scheduling takeover=%+v claimed=%t err=%v", takeover, claimed, err)
		}
		if _, err := st.DeferLegacyDomainCleanup(ctx, deferred, time.Minute); !errors.Is(err, ErrLegacyDomainProviderCASConflict) {
			t.Fatalf("expired owner deferred after takeover err=%v", err)
		}
	})
}

func TestLegacyDomainExactIDCleanupClaimConcurrentLeaseTakeover(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateLegacyDomainLifecycleFixture(t, ctx, db, 9)
		st := &Store{db: db, q: db}
		_, domainID := prepareExactIDLegacyDomainCleanup(
			t, ctx, db, st, "cleanup-exact-concurrent", "cleanup-exact-concurrent.example", "rd-cleanup-exact-concurrent",
		)
		candidates, err := st.ListLegacyDomainCleanupDue(ctx, 1)
		if err != nil || len(candidates) != 1 || candidates[0].OrgDomainID != domainID {
			t.Fatalf("initial exact-id candidate=%+v err=%v", candidates, err)
		}

		type outcome struct {
			claim   LegacyDomainCleanupClaim
			claimed bool
			err     error
		}
		start := make(chan struct{})
		outcomes := make(chan outcome, 2)
		for index := 0; index < 2; index++ {
			go func() {
				<-start
				claim, claimed, err := st.ClaimLegacyDomainCleanup(ctx, candidates[0], time.Minute)
				outcomes <- outcome{claim: claim, claimed: claimed, err: err}
			}()
		}
		close(start)
		var winner LegacyDomainCleanupClaim
		claimedCount := 0
		for index := 0; index < 2; index++ {
			result := <-outcomes
			if result.err != nil {
				t.Fatalf("concurrent exact-id claim: %v", result.err)
			}
			if result.claimed {
				winner = result.claim
				claimedCount++
			}
		}
		if claimedCount != 1 || winner.Intent.Disposition != LegacyDomainProviderCallAuthorized ||
			winner.Intent.ProviderDomainID != "rd-cleanup-exact-concurrent" {
			t.Fatalf("concurrent exact-id winners=%d claim=%+v", claimedCount, winner)
		}
		if _, err := db.ExecContext(ctx, `
			UPDATE domain_ownership_claims
			SET lease_expires_at=clock_timestamp()-interval '1 second'
			WHERE canonical_domain='cleanup-exact-concurrent.example'
		`); err != nil {
			t.Fatal(err)
		}
		takeoverCandidates, err := st.ListLegacyDomainCleanupDue(ctx, 1)
		if err != nil || len(takeoverCandidates) != 1 {
			t.Fatalf("expired exact-id lease candidates=%+v err=%v", takeoverCandidates, err)
		}
		takeover, claimed, err := st.ClaimLegacyDomainCleanup(ctx, takeoverCandidates[0], time.Minute)
		if err != nil || !claimed || takeover.Intent.Disposition != LegacyDomainProviderCallAuthorized ||
			takeover.Intent.WorkflowVersion <= winner.Intent.WorkflowVersion ||
			takeover.Intent.LeaseOwner == winner.Intent.LeaseOwner {
			t.Fatalf("exact-id lease takeover=%+v claimed=%t err=%v", takeover, claimed, err)
		}
	})
}

func TestLegacyDomainCleanupSchedulingPreservesLateCreateCaptureAndBoundsExactIDRetry(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateLegacyDomainLifecycleFixture(t, ctx, db, 9)
		st := &Store{db: db, q: db}
		orgID, domainID := createLegacyDomainLifecycleFixture(
			t, ctx, st, "cleanup-late-create", "cleanup-late-create.example",
		)
		createIntent, err := st.BeginLegacyDomainProviderOperation(ctx, LegacyDomainProviderBegin{
			OrgID: orgID, OrgDomainID: domainID, Operation: LegacyDomainProviderCreate, LeaseTTL: time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(1100 * time.Millisecond)
		release, err := st.BeginLegacyDomainProviderOperation(ctx, LegacyDomainProviderBegin{
			OrgID: orgID, OrgDomainID: domainID, Operation: LegacyDomainProviderDelete,
		})
		if err != nil || release.Disposition != LegacyDomainProviderAwaitingProof ||
			release.LeaseOwner != createIntent.LeaseOwner ||
			release.WorkflowVersion != createIntent.WorkflowVersion+1 {
			t.Fatalf("late-create release fence=%+v create=%+v err=%v", release, createIntent, err)
		}
		candidates, err := st.ListLegacyDomainCleanupDue(ctx, 1)
		if err != nil || len(candidates) != 1 {
			t.Fatalf("late-create cleanup candidate=%+v err=%v", candidates, err)
		}
		cleanupClaim, claimed, err := st.ClaimLegacyDomainCleanup(ctx, candidates[0], time.Minute)
		if err != nil || !claimed || cleanupClaim.Intent.Disposition != LegacyDomainProviderAwaitingProof ||
			cleanupClaim.Intent.LeaseOwner != createIntent.LeaseOwner || cleanupClaim.CleanupNotBefore.IsZero() {
			t.Fatalf("late-create scheduling claim=%+v claimed=%t err=%v", cleanupClaim, claimed, err)
		}
		cleanupClaim, err = st.DeferLegacyDomainCleanup(ctx, cleanupClaim, 2*time.Minute)
		if err != nil {
			t.Fatalf("defer late-create proof lookup: %v", err)
		}

		applied, err := st.ApplyLegacyDomainProviderResult(ctx, LegacyDomainProviderResult{
			Intent: createIntent, Outcome: LegacyDomainProviderObserved,
			ProviderDomainID:        "rd-cleanup-late-create",
			ProviderCanonicalDomain: "cleanup-late-create.example",
			ProviderStatus:          "pending",
		})
		if err != nil || !applied.StaleCreateCaptured || !applied.CompensatingCleanupRequired ||
			applied.Domain.Status != "failed" || applied.Domain.ResendDomainID.String != "rd-cleanup-late-create" {
			t.Fatalf("late create after scheduling applied=%+v err=%v", applied, err)
		}
		claim, err := st.GetDomainOwnershipClaim(ctx, "cleanup-late-create.example")
		if err != nil || claim.State != "releasing" || claim.LeaseOwner.Valid ||
			!claim.ClaimExpiresAt.Valid || !claim.ClaimExpiresAt.Time.Equal(cleanupClaim.CleanupNotBefore) {
			t.Fatalf("late-create scheduling fence claim=%+v err=%v", claim, err)
		}
		if due, err := st.ListLegacyDomainCleanupDue(ctx, 1); err != nil || len(due) != 0 {
			t.Fatalf("captured late identity bypassed bounded retry=%+v err=%v", due, err)
		}
		if _, err := db.ExecContext(ctx, `
			UPDATE domain_ownership_claims
			SET claim_expires_at=clock_timestamp()-interval '1 second'
			WHERE canonical_domain='cleanup-late-create.example'
		`); err != nil {
			t.Fatal(err)
		}
		due, err := st.ListLegacyDomainCleanupDue(ctx, 1)
		if err != nil || len(due) != 1 || !due[0].ProviderDomainID.Valid ||
			due[0].ProviderDomainID.String != "rd-cleanup-late-create" {
			t.Fatalf("captured exact identity did not become due=%+v err=%v", due, err)
		}
		exactCleanup, claimed, err := st.ClaimLegacyDomainCleanup(ctx, due[0], time.Minute)
		if err != nil || !claimed || exactCleanup.Intent.Disposition != LegacyDomainProviderCallAuthorized ||
			exactCleanup.Intent.ProviderDomainID != "rd-cleanup-late-create" {
			t.Fatalf("captured exact identity cleanup=%+v claimed=%t err=%v", exactCleanup, claimed, err)
		}
	})
}

func TestLegacyDomainCompatibilityWriterLocksOrgPolicyBeforeDomainAndClaimRows(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateLegacyDomainLifecycleFixture(t, ctx, db, 9)
		st := &Store{db: db, q: db}
		orgID, domainID := createLegacyDomainLifecycleFixture(
			t, ctx, st, "legacy-compat-lock-order", "compat-lock-order.example",
		)

		blocker, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback()
		if _, err := blocker.ExecContext(ctx,
			`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "org-policy:"+orgID); err != nil {
			t.Fatal(err)
		}
		var blockerPID int
		if err := blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
			t.Fatal(err)
		}

		writerTx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer writerTx.Rollback()
		var writerPID int
		if err := writerTx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&writerPID); err != nil {
			t.Fatal(err)
		}
		writer := &Store{db: db, q: writerTx, inTx: true}
		done := make(chan error, 1)
		go func() {
			done <- writer.UpdateOrgDomainStatus(ctx, domainID, "verified_dns")
		}()
		waitForLegacyDomainLifecyclePostgresBlocker(
			t, ctx, db, writerPID, blockerPID, done, "legacy compatibility writer",
		)

		probe, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		var lockedID string
		if err := probe.QueryRowContext(ctx, `
			SELECT id::text FROM org_domains WHERE id=$1::uuid FOR UPDATE NOWAIT
		`, domainID).Scan(&lockedID); err != nil {
			probe.Rollback()
			t.Fatalf("compatibility writer locked domain before org policy: %v", err)
		}
		var lockedCanonical string
		if err := probe.QueryRowContext(ctx, `
			SELECT canonical_domain FROM domain_ownership_claims
			WHERE canonical_domain='compat-lock-order.example' FOR UPDATE NOWAIT
		`).Scan(&lockedCanonical); err != nil {
			probe.Rollback()
			t.Fatalf("compatibility writer locked claim before org policy: %v", err)
		}
		if err := probe.Rollback(); err != nil {
			t.Fatal(err)
		}

		if err := blocker.Rollback(); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("compatibility writer: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("compatibility writer did not resume after org-policy release")
		}
		if err := writerTx.Commit(); err != nil {
			t.Fatal(err)
		}
		domain, err := st.GetOrgDomainByIDForOrg(ctx, orgID, domainID)
		if err != nil || domain.Status != "verified_dns" {
			t.Fatalf("compatibility writer domain=%+v err=%v", domain, err)
		}
	})
}
