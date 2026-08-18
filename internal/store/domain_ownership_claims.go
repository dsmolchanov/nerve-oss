package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"neuralmail/internal/domains"
)

type DomainOwnershipClaim struct {
	CanonicalDomain string
	OrgDomainID     string
	OrgID           string
	OnboardingID    sql.NullString
	OwnerKind       string
	State           string
	WorkflowVersion int64
	LeaseOwner      sql.NullString
	LeaseExpiresAt  sql.NullTime
	ClaimExpiresAt  sql.NullTime
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func (s *Store) cloudSchemaSupportsM2M(ctx context.Context) (bool, error) {
	var present bool
	if err := s.q.QueryRowContext(ctx, `
		SELECT to_regclass('public.schema_migrations_cloud') IS NOT NULL
	`).Scan(&present); err != nil || !present {
		return false, err
	}
	var version int64
	if err := s.q.QueryRowContext(ctx, `
		SELECT coalesce(max(version_id) FILTER (WHERE is_applied), 0)
		FROM schema_migrations_cloud
	`).Scan(&version); err != nil {
		return false, err
	}
	return version >= 9, nil
}

func (s *Store) lockCanonicalDomain(ctx context.Context, canonicalDomain string) error {
	canonicalDomain = strings.TrimSpace(canonicalDomain)
	if canonicalDomain == "" {
		return errors.New("canonical domain is required")
	}
	if !s.inTx {
		return errors.New("canonical domain lock requires an explicit transaction")
	}
	_, err := s.q.ExecContext(ctx, `
		SELECT pg_advisory_xact_lock(hashtextextended('canonical-domain:' || $1, 0))
	`, canonicalDomain)
	return err
}

func (s *Store) lockActiveOrgForDomainCompatibility(ctx context.Context, orgID string) error {
	if err := s.lockReconciliationResources(ctx, "org:"+orgID); err != nil {
		return err
	}
	var active bool
	if err := s.q.QueryRowContext(ctx, `
		SELECT EXISTS(
		  SELECT 1 FROM orgs
		  WHERE id = $1 AND coalesce(to_jsonb(orgs)->>'deleted_at', '') = ''
		)
	`, orgID).Scan(&active); err != nil {
		return err
	}
	if !active {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) createLegacyDomainClaim(ctx context.Context, canonicalDomain, domainID, orgID string, expiresAt *time.Time) error {
	state := "provider_owned"
	if expiresAt != nil {
		state = "pending"
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO domain_ownership_claims
		  (canonical_domain, org_domain_id, org_id, owner_kind, state, claim_expires_at)
		VALUES ($1, $2, $3, 'legacy', $4, $5)
	`, canonicalDomain, domainID, orgID, state, expiresAt)
	return err
}

func (s *Store) ensureLegacyDomainClaim(ctx context.Context, canonicalDomain, domainID, orgID, status string, expiresAt sql.NullTime) error {
	var claimDomainID, claimOrgID, ownerKind string
	err := s.q.QueryRowContext(ctx, `
		SELECT org_domain_id::text, org_id::text, owner_kind
		FROM domain_ownership_claims
		WHERE canonical_domain = $1
		FOR UPDATE
	`, canonicalDomain).Scan(&claimDomainID, &claimOrgID, &ownerKind)
	if errors.Is(err, sql.ErrNoRows) {
		var expiry *time.Time
		if status == "pending" && expiresAt.Valid {
			expiry = &expiresAt.Time
		}
		return s.createLegacyDomainClaim(ctx, canonicalDomain, domainID, orgID, expiry)
	}
	if err != nil {
		return err
	}
	if claimDomainID != domainID || claimOrgID != orgID || ownerKind != "legacy" {
		return ErrResourceConflict
	}
	return nil
}

func (s *Store) transitionLegacyDomainClaim(ctx context.Context, canonicalDomain, domainID, status string, expiresAt sql.NullTime) error {
	state := "provider_owned"
	var claimExpiry any
	if status == "pending" {
		state = "pending"
		if expiresAt.Valid {
			claimExpiry = expiresAt.Time
		}
	} else if status == "failed" {
		state = "releasing"
	}
	result, err := s.q.ExecContext(ctx, `
		UPDATE domain_ownership_claims
		SET state = $3, claim_expires_at = $4, workflow_version = workflow_version + 1, updated_at = now()
		WHERE canonical_domain = $1 AND org_domain_id = $2 AND owner_kind = 'legacy'
	`, canonicalDomain, domainID, state, claimExpiry)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("legacy domain %s has no matching ownership claim", canonicalDomain)
	}
	return nil
}

type legacyDomainIdentity struct {
	ID        string
	OrgID     string
	Canonical string
	Status    string
	ExpiresAt sql.NullTime
}

func (s *Store) withLockedLegacyDomain(ctx context.Context, domainID, requiredOrgID string, fn func(*Store, legacyDomainIdentity, bool) error) error {
	if fn == nil {
		return errors.New("legacy domain callback is required")
	}
	return s.withTx(ctx, func(scoped *Store) error {
		if err := scoped.requireDomainWritesEnabled(ctx); err != nil {
			return err
		}
		var storedDomain string
		query := `SELECT domain FROM org_domains WHERE id = $1`
		args := []any{domainID}
		if strings.TrimSpace(requiredOrgID) != "" {
			query += ` AND org_id = $2`
			args = append(args, requiredOrgID)
		}
		if err := scoped.q.QueryRowContext(ctx, query, args...).Scan(&storedDomain); err != nil {
			return err
		}
		canonical, err := domains.CanonicalizeDomain(storedDomain)
		if err != nil {
			return fmt.Errorf("canonicalize stored domain: %w", err)
		}
		if err := scoped.lockCanonicalDomain(ctx, canonical); err != nil {
			return err
		}
		var identity legacyDomainIdentity
		query = `
			SELECT id::text, org_id::text, domain, status, expires_at
			FROM org_domains WHERE id = $1`
		args = []any{domainID}
		if strings.TrimSpace(requiredOrgID) != "" {
			query += ` AND org_id = $2`
			args = append(args, requiredOrgID)
		}
		query += ` FOR UPDATE`
		if err := scoped.q.QueryRowContext(ctx, query, args...).Scan(
			&identity.ID, &identity.OrgID, &identity.Canonical, &identity.Status, &identity.ExpiresAt,
		); err != nil {
			return err
		}
		// Domain readiness is evidence the autonomous outbound policy reads, so
		// every writer that reaches this helper also takes the enqueue fence.
		// Without it a domain could lose readiness between an enqueue's policy
		// check and its outbox insert.
		if err := scoped.LockOrgPolicy(ctx, identity.OrgID); err != nil {
			return err
		}
		lockedCanonical, err := domains.CanonicalizeDomain(identity.Canonical)
		if err != nil {
			return fmt.Errorf("canonicalize locked domain: %w", err)
		}
		if lockedCanonical != canonical {
			return errors.New("domain identity changed while acquiring canonical lock")
		}
		identity.Canonical = lockedCanonical
		schema9, err := scoped.cloudSchemaSupportsM2M(ctx)
		if err != nil {
			return err
		}
		if schema9 {
			if err := scoped.ensureLegacyDomainClaim(ctx, identity.Canonical, identity.ID, identity.OrgID, identity.Status, identity.ExpiresAt); err != nil {
				return err
			}
		}
		return fn(scoped, identity, schema9)
	})
}

func scanDomainOwnershipClaim(row *sql.Row, claim *DomainOwnershipClaim) error {
	return row.Scan(
		&claim.CanonicalDomain, &claim.OrgDomainID, &claim.OrgID, &claim.OnboardingID,
		&claim.OwnerKind, &claim.State, &claim.WorkflowVersion, &claim.LeaseOwner,
		&claim.LeaseExpiresAt, &claim.ClaimExpiresAt, &claim.CreatedAt, &claim.UpdatedAt,
	)
}

func (s *Store) GetDomainOwnershipClaim(ctx context.Context, canonicalDomain string) (DomainOwnershipClaim, error) {
	var claim DomainOwnershipClaim
	err := scanDomainOwnershipClaim(s.q.QueryRowContext(ctx, `
		SELECT canonical_domain, org_domain_id::text, org_id::text, onboarding_id::text,
		       owner_kind, state, workflow_version, lease_owner, lease_expires_at,
		       claim_expires_at, created_at, updated_at
		FROM domain_ownership_claims WHERE canonical_domain = $1
	`, canonicalDomain), &claim)
	return claim, err
}
