package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"neuralmail/internal/domains"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

type OrgDomain struct {
	ID                string
	OrgID             string
	Domain            string
	Status            string // "pending", "verified_dns", "provisioning", "active", "failed"
	VerificationToken string
	MXVerified        bool
	SPFVerified       bool
	DKIMVerified      bool
	DMARCVerified     bool
	InboundEnabled    bool
	DKIMSelector      string
	DKIMPrivateKeyEnc sql.NullString // AES-GCM encrypted PEM
	DKIMPublicKey     sql.NullString // PEM (not secret)
	DKIMMethod        string         // "cname" or "txt"
	LastCheckAt       sql.NullTime
	VerifiedAt        sql.NullTime
	ExpiresAt         sql.NullTime
	ResendDomainID    sql.NullString
	ResendStatus      sql.NullString
	ResendDNSRecords  []byte
	CreatedAt         time.Time
	UpdatedAt         time.Time
	ExternalRef       sql.NullString

	ResendReceivingEnabled bool
	CatchAllEnabled        bool
	ForwardTo              sql.NullString // domain-level forwarding (overridden by inbox-level)
}

var ErrDomainWritesFenced = errors.New("domain writes are temporarily fenced")

// requireDomainWritesEnabled takes a row lock that makes a completed global
// fence update a drain barrier: after nerve-flags commits enabled=false, every
// older writer has finished and every newer writer fails before mutation.
func (s *Store) requireDomainWritesEnabled(ctx context.Context) error {
	if err := s.requireTx(); err != nil {
		return err
	}
	// The advisory lock makes the very first fence installation a drain
	// barrier even when no feature-flag row exists yet. All domain writers
	// take the shared side before any canonical-domain or row lock.
	if _, err := s.q.ExecContext(ctx, `SELECT pg_advisory_xact_lock_shared(hashtextextended('domain-writes-fence', 0))`); err != nil {
		return err
	}
	var fenceTablePresent bool
	if err := s.q.QueryRowContext(ctx, `SELECT to_regclass('public.org_feature_flags') IS NOT NULL`).Scan(&fenceTablePresent); err != nil {
		return err
	}
	if !fenceTablePresent {
		return nil
	}
	var enabled bool
	err := s.q.QueryRowContext(ctx, `
		SELECT enabled
		FROM org_feature_flags
		WHERE org_id IS NULL AND flag = 'domain_writes'
		FOR SHARE
	`).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !enabled {
		return ErrDomainWritesFenced
	}
	return nil
}

// CreateOrgDomain inserts a new domain registration. The domain must already be
// canonicalized (lowercase, no trailing dot). Sets expires_at = now() + 7 days for pending claims.
func (s *Store) CreateOrgDomain(ctx context.Context, orgID, domain, verificationToken, dkimSelector, dkimPrivateKeyEnc, dkimPublicKey, dkimMethod string) (string, error) {
	return s.createOrgDomain(ctx, orgID, domain, verificationToken, dkimSelector, dkimPrivateKeyEnc, dkimPublicKey, dkimMethod, "")
}

func (s *Store) createOrgDomain(ctx context.Context, orgID, domain, verificationToken, dkimSelector, dkimPrivateKeyEnc, dkimPublicKey, dkimMethod, externalRef string) (string, error) {
	canonicalDomain, err := domains.CanonicalizeDomain(domain)
	if err != nil {
		return "", err
	}
	id := uuid.NewString()
	err = s.withTx(ctx, func(scoped *Store) error {
		if err := scoped.requireDomainWritesEnabled(ctx); err != nil {
			return err
		}
		if err := scoped.lockCanonicalDomain(ctx, canonicalDomain); err != nil {
			return err
		}
		if err := scoped.lockActiveOrgForDomainCompatibility(ctx, orgID); err != nil {
			return err
		}
		var expiresAt time.Time
		var err error
		if strings.TrimSpace(externalRef) == "" {
			err = scoped.q.QueryRowContext(ctx, `
				INSERT INTO org_domains
				  (id, org_id, domain, verification_token, dkim_selector, dkim_private_key_enc,
				   dkim_public_key, dkim_method, expires_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now() + interval '7 days')
				RETURNING expires_at
			`, id, orgID, canonicalDomain, verificationToken, dkimSelector,
				nullIfEmpty(dkimPrivateKeyEnc), nullIfEmpty(dkimPublicKey), dkimMethod,
			).Scan(&expiresAt)
		} else {
			err = scoped.q.QueryRowContext(ctx, `
				INSERT INTO org_domains
				  (id, org_id, domain, verification_token, dkim_selector, dkim_private_key_enc,
				   dkim_public_key, dkim_method, expires_at, external_ref)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now() + interval '7 days', $9)
				RETURNING expires_at
			`, id, orgID, canonicalDomain, verificationToken, dkimSelector,
				nullIfEmpty(dkimPrivateKeyEnc), nullIfEmpty(dkimPublicKey), dkimMethod, strings.TrimSpace(externalRef),
			).Scan(&expiresAt)
		}
		if err != nil {
			return err
		}
		schema9, err := scoped.cloudSchemaSupportsM2M(ctx)
		if err != nil || !schema9 {
			return err
		}
		if err := scoped.createLegacyDomainClaim(ctx, canonicalDomain, id, orgID, &expiresAt); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return ErrResourceConflict
			}
			return err
		}
		return nil
	})
	return id, err
}

func (s *Store) EnsureOrgDomain(ctx context.Context, orgID, domain, verificationToken, dkimSelector, dkimPrivateKeyEnc, dkimPublicKey, dkimMethod, externalRef string) (OrgDomain, bool, error) {
	externalRef = strings.TrimSpace(externalRef)
	if externalRef == "" {
		return OrgDomain{}, false, errors.New("missing external_ref")
	}
	orgID = strings.TrimSpace(orgID)
	if orgID == "" {
		return OrgDomain{}, false, errors.New("missing org_id")
	}

	var rec OrgDomain
	created := false
	err := s.withTx(ctx, func(scoped *Store) error {
		if err := scoped.requireDomainWritesEnabled(ctx); err != nil {
			return err
		}
		canonicalDomain, err := domains.CanonicalizeDomain(domain)
		if err != nil {
			return err
		}
		if err := scoped.lockCanonicalDomain(ctx, canonicalDomain); err != nil {
			return err
		}
		if err := scoped.lockActiveOrgForDomainCompatibility(ctx, orgID); err != nil {
			return err
		}

		var ensureErr error
		rec, created, ensureErr = scoped.ensureOrgDomainLocked(
			ctx, orgID, domain, verificationToken, dkimSelector,
			dkimPrivateKeyEnc, dkimPublicKey, dkimMethod, externalRef,
		)
		return ensureErr
	})
	return rec, created, err
}

func (s *Store) ensureOrgDomainLocked(ctx context.Context, orgID, domain, verificationToken, dkimSelector, dkimPrivateKeyEnc, dkimPublicKey, dkimMethod, externalRef string) (OrgDomain, bool, error) {
	canonicalDomain, err := domains.CanonicalizeDomain(domain)
	if err != nil {
		return OrgDomain{}, false, err
	}
	schema9, err := s.cloudSchemaSupportsM2M(ctx)
	if err != nil {
		return OrgDomain{}, false, err
	}
	var conflictingActiveDomain bool
	if err := s.q.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM org_domains
			WHERE lower(domain) = $1
			  AND status IN ('verified_dns', 'provisioning', 'active')
			  AND external_ref IS DISTINCT FROM $2
		)
	`, canonicalDomain, externalRef).Scan(&conflictingActiveDomain); err != nil {
		return OrgDomain{}, false, err
	}
	if conflictingActiveDomain {
		return OrgDomain{}, false, ErrResourceConflict
	}

	// Insert first and let the unique external_ref index serialize concurrent
	// reconciler replays. A pre-read followed by an unchecked insert leaves a
	// race where the losing worker exposes a raw unique-constraint error.
	id := uuid.NewString()
	var expiresAt time.Time
	err = s.q.QueryRowContext(ctx, `
		INSERT INTO org_domains
		  (id, org_id, domain, verification_token, dkim_selector,
		   dkim_private_key_enc, dkim_public_key, dkim_method, expires_at, external_ref)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now() + interval '7 days', $9)
		ON CONFLICT (external_ref) WHERE external_ref IS NOT NULL DO NOTHING
		RETURNING id::text, expires_at
	`, id, orgID, canonicalDomain, verificationToken, dkimSelector,
		nullIfEmpty(dkimPrivateKeyEnc), nullIfEmpty(dkimPublicKey), dkimMethod, externalRef,
	).Scan(&id, &expiresAt)
	if err == nil {
		if schema9 {
			if claimErr := s.createLegacyDomainClaim(ctx, canonicalDomain, id, orgID, &expiresAt); claimErr != nil {
				var pgErr *pgconn.PgError
				if errors.As(claimErr, &pgErr) && pgErr.Code == "23505" {
					return OrgDomain{}, false, ErrResourceConflict
				}
				return OrgDomain{}, false, claimErr
			}
		}
		created, getErr := s.GetOrgDomainByIDForOrg(ctx, orgID, id)
		return created, true, getErr
	}
	if !errors.Is(err, sql.ErrNoRows) {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "idx_org_domains_verified" {
			return OrgDomain{}, false, ErrResourceConflict
		}
		return OrgDomain{}, false, err
	}

	existing, err := s.GetOrgDomainByExternalRef(ctx, externalRef)
	if err != nil {
		return OrgDomain{}, false, err
	}
	if existing.OrgID != orgID || existing.Domain != canonicalDomain || existing.DKIMMethod != dkimMethod {
		return OrgDomain{}, false, ErrIdempotencyConflict
	}
	if schema9 {
		if err := s.ensureLegacyDomainClaim(ctx, canonicalDomain, existing.ID, orgID, existing.Status, existing.ExpiresAt); err != nil {
			return OrgDomain{}, false, err
		}
	}
	return existing, false, nil
}

func (s *Store) GetOrgDomainByExternalRef(ctx context.Context, externalRef string) (OrgDomain, error) {
	var d OrgDomain
	row := s.q.QueryRowContext(ctx, `
		SELECT id, org_id, domain, status, verification_token,
		       mx_verified, spf_verified, dkim_verified, dmarc_verified,
		       inbound_enabled, dkim_selector, dkim_private_key_enc, dkim_public_key,
		       dkim_method, last_check_at, verified_at, expires_at,
		       resend_domain_id, resend_domain_status, resend_dns_records,
		       resend_receiving_enabled, catch_all_enabled, forward_to,
		       created_at, updated_at, to_jsonb(org_domains)->>'external_ref'
		FROM org_domains WHERE external_ref = $1
	`, strings.TrimSpace(externalRef))
	return d, scanOrgDomain(row, &d)
}

// GetOrgDomain retrieves a domain by its canonical domain name.
// In cloud mode with RLS, results are scoped to the current org.
func (s *Store) GetOrgDomain(ctx context.Context, domain string) (OrgDomain, error) {
	var d OrgDomain
	row := s.q.QueryRowContext(ctx, `
		SELECT id, org_id, domain, status, verification_token,
		       mx_verified, spf_verified, dkim_verified, dmarc_verified,
		       inbound_enabled, dkim_selector, dkim_private_key_enc, dkim_public_key,
		       dkim_method, last_check_at, verified_at, expires_at,
		       resend_domain_id, resend_domain_status, resend_dns_records,
		       resend_receiving_enabled, catch_all_enabled,
		       forward_to,
		       created_at, updated_at, to_jsonb(org_domains)->>'external_ref'
		FROM org_domains
		WHERE lower(domain) = lower($1)
		ORDER BY created_at DESC
		LIMIT 1
	`, domain)
	if err := scanOrgDomain(row, &d); err != nil {
		return d, err
	}
	return d, nil
}

// GetOrgDomainByID retrieves a domain by UUID.
func (s *Store) GetOrgDomainByID(ctx context.Context, id string) (OrgDomain, error) {
	var d OrgDomain
	row := s.q.QueryRowContext(ctx, `
		SELECT id, org_id, domain, status, verification_token,
		       mx_verified, spf_verified, dkim_verified, dmarc_verified,
		       inbound_enabled, dkim_selector, dkim_private_key_enc, dkim_public_key,
		       dkim_method, last_check_at, verified_at, expires_at,
		       resend_domain_id, resend_domain_status, resend_dns_records,
		       resend_receiving_enabled, catch_all_enabled,
		       forward_to,
		       created_at, updated_at, to_jsonb(org_domains)->>'external_ref'
		FROM org_domains
		WHERE id = $1
	`, id)
	if err := scanOrgDomain(row, &d); err != nil {
		return d, err
	}
	return d, nil
}

// GetOrgDomainByIDForOrg retrieves a domain by UUID, scoped to an org.
func (s *Store) GetOrgDomainByIDForOrg(ctx context.Context, orgID, id string) (OrgDomain, error) {
	var d OrgDomain
	row := s.q.QueryRowContext(ctx, `
		SELECT id, org_id, domain, status, verification_token,
		       mx_verified, spf_verified, dkim_verified, dmarc_verified,
		       inbound_enabled, dkim_selector, dkim_private_key_enc, dkim_public_key,
		       dkim_method, last_check_at, verified_at, expires_at,
		       resend_domain_id, resend_domain_status, resend_dns_records,
		       resend_receiving_enabled, catch_all_enabled,
		       forward_to,
		       created_at, updated_at, to_jsonb(org_domains)->>'external_ref'
		FROM org_domains
		WHERE id = $1 AND org_id = $2
	`, id, orgID)
	if err := scanOrgDomain(row, &d); err != nil {
		return d, err
	}
	return d, nil
}

// ListOrgDomains returns all domains for an org.
func (s *Store) ListOrgDomains(ctx context.Context, orgID string) ([]OrgDomain, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, org_id, domain, status, verification_token,
		       mx_verified, spf_verified, dkim_verified, dmarc_verified,
		       inbound_enabled, dkim_selector, dkim_private_key_enc, dkim_public_key,
		       dkim_method, last_check_at, verified_at, expires_at,
		       resend_domain_id, resend_domain_status, resend_dns_records,
		       resend_receiving_enabled, catch_all_enabled,
		       forward_to,
		       created_at, updated_at, to_jsonb(org_domains)->>'external_ref'
		FROM org_domains
		WHERE org_id = $1
		   OR EXISTS (
		     SELECT 1 FROM org_domain_grants g
		     WHERE g.org_domain_id = org_domains.id
		       AND g.grantee_org_id = $1
		       AND g.status = 'active'
		   )
		ORDER BY created_at DESC
	`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var domains []OrgDomain
	for rows.Next() {
		var d OrgDomain
		if err := rows.Scan(
			&d.ID, &d.OrgID, &d.Domain, &d.Status, &d.VerificationToken,
			&d.MXVerified, &d.SPFVerified, &d.DKIMVerified, &d.DMARCVerified,
			&d.InboundEnabled, &d.DKIMSelector, &d.DKIMPrivateKeyEnc, &d.DKIMPublicKey,
			&d.DKIMMethod, &d.LastCheckAt, &d.VerifiedAt, &d.ExpiresAt,
			&d.ResendDomainID, &d.ResendStatus, &d.ResendDNSRecords,
			&d.ResendReceivingEnabled, &d.CatchAllEnabled,
			&d.ForwardTo,
			&d.CreatedAt, &d.UpdatedAt, &d.ExternalRef,
		); err != nil {
			return nil, err
		}
		domains = append(domains, d)
	}
	return domains, rows.Err()
}

// UpdateOrgDomainVerification updates DNS verification fields and status.
func (s *Store) UpdateOrgDomainVerification(ctx context.Context, id string, mx, spf, dkim, dmarc bool, status string) error {
	return s.withLockedLegacyDomain(ctx, id, "", func(scoped *Store, identity legacyDomainIdentity, schema9 bool) error {
		if err := scoped.requireDomainWritesEnabled(ctx); err != nil {
			return err
		}
		if _, err := scoped.q.ExecContext(ctx, `
			UPDATE org_domains
			SET mx_verified = $2, spf_verified = $3, dkim_verified = $4, dmarc_verified = $5,
			    status = $6, last_check_at = now(),
			    verified_at = CASE WHEN $6 IN ('verified_dns', 'active') THEN now() ELSE verified_at END,
			    updated_at = now()
			WHERE id = $1
		`, id, mx, spf, dkim, dmarc, status); err != nil {
			return err
		}
		if schema9 {
			return scoped.transitionLegacyDomainClaim(ctx, identity.Canonical, id, status, identity.ExpiresAt)
		}
		return nil
	})
}

// UpdateOrgDomainStatus transitions domain to a new status.
func (s *Store) UpdateOrgDomainStatus(ctx context.Context, id string, status string) error {
	return s.withLockedLegacyDomain(ctx, id, "", func(scoped *Store, identity legacyDomainIdentity, schema9 bool) error {
		if err := scoped.requireDomainWritesEnabled(ctx); err != nil {
			return err
		}
		q := `UPDATE org_domains SET status = $2, updated_at = now() WHERE id = $1`
		if status == "active" || status == "verified_dns" {
			q = `UPDATE org_domains SET status = $2, verified_at = now(), updated_at = now() WHERE id = $1`
		}
		if _, err := scoped.q.ExecContext(ctx, q, id, status); err != nil {
			return err
		}
		if schema9 {
			return scoped.transitionLegacyDomainClaim(ctx, identity.Canonical, id, status, identity.ExpiresAt)
		}
		return nil
	})
}

// DeleteOrgDomain removes a domain registration.
func (s *Store) DeleteOrgDomain(ctx context.Context, id string) error {
	return s.withLockedLegacyDomain(ctx, id, "", func(scoped *Store, identity legacyDomainIdentity, schema9 bool) error {
		if err := scoped.requireDomainWritesEnabled(ctx); err != nil {
			return err
		}
		if !schema9 {
			_, err := scoped.q.ExecContext(ctx, `DELETE FROM org_domains WHERE id = $1`, id)
			return err
		}
		if err := scoped.transitionLegacyDomainClaim(ctx, identity.Canonical, id, "failed", identity.ExpiresAt); err != nil {
			return err
		}
		_, err := scoped.q.ExecContext(ctx, `UPDATE org_domains SET status = 'failed', updated_at = now() WHERE id = $1`, id)
		return err
	})
}

// DeleteOrgDomainForOrg removes a domain registration, scoped to an org.
// Returns true if a row was deleted.
func (s *Store) DeleteOrgDomainForOrg(ctx context.Context, orgID, id string) (bool, error) {
	_, deleted, err := s.BeginOrgDomainReleaseForOrg(ctx, orgID, id)
	return deleted, err
}

// BeginOrgDomainReleaseForOrg returns the locked provider identity at the same
// linearization point that starts release. On Cloud schema 8 it preserves the
// legacy immediate delete. On schema 9 it retains the Core row and ownership
// claim in releasing state until FinalizeOrgDomainReleaseForOrg is called.
func (s *Store) BeginOrgDomainReleaseForOrg(ctx context.Context, orgID, id string) (OrgDomain, bool, error) {
	var domain OrgDomain
	releaseStarted := false
	err := s.withLockedLegacyDomain(ctx, id, orgID, func(scoped *Store, identity legacyDomainIdentity, schema9 bool) error {
		if err := scoped.requireDomainWritesEnabled(ctx); err != nil {
			return err
		}
		var err error
		domain, err = scoped.GetOrgDomainByIDForOrg(ctx, orgID, id)
		if err != nil {
			return err
		}
		var blocked bool
		if err := scoped.q.QueryRowContext(ctx, `
			SELECT EXISTS(SELECT 1 FROM inboxes WHERE org_domain_id = $1)
			    OR EXISTS(SELECT 1 FROM org_domain_grants WHERE org_domain_id = $1)
		`, id).Scan(&blocked); err != nil {
			return err
		}
		if schema9 && !blocked {
			if err := scoped.q.QueryRowContext(ctx, `
				SELECT EXISTS(
				  SELECT 1 FROM managed_mailbox_platform_domains
				  WHERE org_domain_id = $1
				)
			`, id).Scan(&blocked); err != nil {
				return err
			}
		}
		if blocked {
			return ErrResourceConflict
		}
		if !schema9 {
			result, err := scoped.q.ExecContext(ctx, `DELETE FROM org_domains WHERE id = $1 AND org_id = $2`, id, orgID)
			if err != nil {
				return err
			}
			n, err := result.RowsAffected()
			releaseStarted = n > 0
			return err
		}
		if err := scoped.transitionLegacyDomainClaim(ctx, identity.Canonical, id, "failed", identity.ExpiresAt); err != nil {
			return err
		}
		result, err := scoped.q.ExecContext(ctx, `
			UPDATE org_domains SET status = 'failed', updated_at = now()
			WHERE id = $1 AND org_id = $2
		`, id, orgID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		releaseStarted = n > 0
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return OrgDomain{}, false, nil
	}
	return domain, releaseStarted, err
}

// FinalizeOrgDomainReleaseForOrg removes a legacy claim and its Core row only
// after the caller has obtained a definitive provider-absent result. Unknown
// provider outcomes must use DeleteOrgDomainForOrg's retained releasing state
// instead so a retry/reconciler still has durable ownership provenance.
func (s *Store) FinalizeOrgDomainReleaseForOrg(ctx context.Context, orgID, id string) (bool, error) {
	deleted := false
	err := s.withLockedLegacyDomain(ctx, id, orgID, func(scoped *Store, identity legacyDomainIdentity, schema9 bool) error {
		if !schema9 {
			result, err := scoped.q.ExecContext(ctx, `DELETE FROM org_domains WHERE id = $1 AND org_id = $2`, id, orgID)
			if err != nil {
				return err
			}
			n, err := result.RowsAffected()
			deleted = n == 1
			return err
		}
		result, err := scoped.q.ExecContext(ctx, `
			DELETE FROM domain_ownership_claims
			WHERE canonical_domain = $1 AND org_domain_id = $2
			  AND org_id = $3 AND owner_kind = 'legacy'
		`, identity.Canonical, id, orgID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return errors.New("legacy domain release has no matching ownership claim")
		}
		result, err = scoped.q.ExecContext(ctx, `DELETE FROM org_domains WHERE id = $1 AND org_id = $2`, id, orgID)
		if err != nil {
			return err
		}
		n, err = result.RowsAffected()
		deleted = n == 1
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return deleted, err
}

// GetOrgDomainForSending retrieves the active domain + encrypted DKIM key for a given email address domain.
// Only returns domains with status='active'.
func (s *Store) GetOrgDomainForSending(ctx context.Context, domain string) (OrgDomain, error) {
	var d OrgDomain
	row := s.q.QueryRowContext(ctx, `
		SELECT id, org_id, domain, status, verification_token,
		       mx_verified, spf_verified, dkim_verified, dmarc_verified,
		       inbound_enabled, dkim_selector, dkim_private_key_enc, dkim_public_key,
		       dkim_method, last_check_at, verified_at, expires_at,
		       resend_domain_id, resend_domain_status, resend_dns_records,
		       resend_receiving_enabled, catch_all_enabled,
		       forward_to,
		       created_at, updated_at, to_jsonb(org_domains)->>'external_ref'
		FROM org_domains
		WHERE lower(domain) = lower($1) AND status = 'active'
		LIMIT 1
	`, domain)
	if err := scanOrgDomain(row, &d); err != nil {
		return d, err
	}
	return d, nil
}

// CountDomainsByOrg returns the number of non-expired domains for an org.
func (s *Store) CountDomainsByOrg(ctx context.Context, orgID string) (int, error) {
	row := s.q.QueryRowContext(ctx, `
		SELECT count(*)
		FROM org_domains
		WHERE org_id = $1
		  AND (status != 'pending' OR expires_at > now())
	`, orgID)
	var count int
	if err := row.Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// ExpirePendingDomains deletes pending domains past their expires_at.
// Returns the number of deleted rows.
func (s *Store) ExpirePendingDomains(ctx context.Context) (int, error) {
	transitioned := 0
	err := s.withTx(ctx, func(scoped *Store) error {
		if err := scoped.requireDomainWritesEnabled(ctx); err != nil {
			return err
		}
		var err error
		transitioned, err = scoped.expirePendingDomainsLocked(ctx)
		return err
	})
	return transitioned, err
}

func (s *Store) expirePendingDomainsLocked(ctx context.Context) (int, error) {
	schema9, err := s.cloudSchemaSupportsM2M(ctx)
	if err != nil {
		return 0, err
	}
	if !schema9 {
		result, err := s.q.ExecContext(ctx, `DELETE FROM org_domains WHERE status = 'pending' AND expires_at < now()`)
		if err != nil {
			return 0, err
		}
		n, err := result.RowsAffected()
		return int(n), err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT id::text
		FROM org_domains
		WHERE status = 'pending' AND expires_at < now()
		  AND coalesce(external_ref, '') NOT LIKE 'm2m-onboarding:%'
		ORDER BY id
	`)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	transitioned := 0
	for _, id := range ids {
		err := s.withLockedLegacyDomain(ctx, id, "", func(scoped *Store, identity legacyDomainIdentity, _ bool) error {
			if identity.Status != "pending" || !identity.ExpiresAt.Valid || !identity.ExpiresAt.Time.Before(time.Now()) {
				return nil
			}
			if err := scoped.transitionLegacyDomainClaim(ctx, identity.Canonical, id, "failed", identity.ExpiresAt); err != nil {
				return err
			}
			result, err := scoped.q.ExecContext(ctx, `
				UPDATE org_domains SET status = 'failed', updated_at = now()
				WHERE id = $1 AND status = 'pending' AND expires_at < now()
			`, id)
			if err != nil {
				return err
			}
			n, err := result.RowsAffected()
			transitioned += int(n)
			return err
		})
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return transitioned, err
		}
	}
	return transitioned, nil
}

func scanOrgDomain(row *sql.Row, d *OrgDomain) error {
	return row.Scan(
		&d.ID, &d.OrgID, &d.Domain, &d.Status, &d.VerificationToken,
		&d.MXVerified, &d.SPFVerified, &d.DKIMVerified, &d.DMARCVerified,
		&d.InboundEnabled, &d.DKIMSelector, &d.DKIMPrivateKeyEnc, &d.DKIMPublicKey,
		&d.DKIMMethod, &d.LastCheckAt, &d.VerifiedAt, &d.ExpiresAt,
		&d.ResendDomainID, &d.ResendStatus, &d.ResendDNSRecords,
		&d.ResendReceivingEnabled, &d.CatchAllEnabled,
		&d.ForwardTo,
		&d.CreatedAt, &d.UpdatedAt, &d.ExternalRef,
	)
}

func nullIfEmpty(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func (s *Store) UpdateOrgDomainResend(ctx context.Context, id, resendDomainID, resendStatus string, dnsRecords []byte) error {
	return s.withLockedLegacyDomain(ctx, id, "", func(scoped *Store, _ legacyDomainIdentity, _ bool) error {
		if err := scoped.requireDomainWritesEnabled(ctx); err != nil {
			return err
		}
		_, err := scoped.q.ExecContext(ctx, `
			UPDATE org_domains
			SET resend_domain_id = nullif($2, ''),
			    resend_domain_status = nullif($3, ''),
			    resend_dns_records = coalesce($4::jsonb, resend_dns_records),
			    updated_at = now()
			WHERE id = $1
		`, id, resendDomainID, resendStatus, dnsRecords)
		return err
	})
}

// GetReceivingOrgDomainByDomain finds an active domain record with receiving enabled.
// This does NOT use RLS (called before we know the org).
// Only routes if domain is active AND receiving is enabled.
func (s *Store) GetReceivingOrgDomainByDomain(ctx context.Context, domain string) (*OrgDomain, error) {
	var d OrgDomain
	row := s.q.QueryRowContext(ctx, `
		SELECT id, org_id, domain, status, verification_token,
		       mx_verified, spf_verified, dkim_verified, dmarc_verified,
		       inbound_enabled, dkim_selector, dkim_private_key_enc, dkim_public_key,
		       dkim_method, last_check_at, verified_at, expires_at,
		       resend_domain_id, resend_domain_status, resend_dns_records,
		       resend_receiving_enabled, catch_all_enabled,
		       forward_to,
		       created_at, updated_at, to_jsonb(org_domains)->>'external_ref'
		FROM org_domains
		WHERE lower(domain) = lower($1)
		  AND status = 'active'
		  AND resend_receiving_enabled = true
		LIMIT 1
	`, domain)
	if err := scanOrgDomain(row, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// UpdateOrgDomainCatchAll toggles catch-all on a domain.
// Only allowed on domains with resend_receiving_enabled = true and status = 'active'.
func (s *Store) UpdateOrgDomainCatchAll(ctx context.Context, orgID, domainID string, enabled bool) error {
	return s.withLockedLegacyDomain(ctx, domainID, orgID, func(scoped *Store, _ legacyDomainIdentity, _ bool) error {
		if err := scoped.requireDomainWritesEnabled(ctx); err != nil {
			return err
		}
		result, err := scoped.q.ExecContext(ctx, `
			UPDATE org_domains
			SET catch_all_enabled = $3, updated_at = now()
			WHERE id = $1 AND org_id = $2
			  AND resend_receiving_enabled = true
			  AND status = 'active'
		`, domainID, orgID, enabled)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("domain not found, not active, or receiving not enabled")
		}
		return nil
	})
}

// UpdateOrgDomainForwardTo sets or clears domain-level forwarding.
// Only allowed on active domains with receiving enabled.
func (s *Store) UpdateOrgDomainForwardTo(ctx context.Context, orgID, domainID, forwardTo string) error {
	var ft any
	if forwardTo == "" {
		ft = nil
	} else {
		ft = forwardTo
	}
	return s.withLockedLegacyDomain(ctx, domainID, orgID, func(scoped *Store, _ legacyDomainIdentity, _ bool) error {
		if err := scoped.requireDomainWritesEnabled(ctx); err != nil {
			return err
		}
		result, err := scoped.q.ExecContext(ctx, `
			UPDATE org_domains
			SET forward_to = $3, updated_at = now()
			WHERE id = $1 AND org_id = $2
			  AND resend_receiving_enabled = true
			  AND status = 'active'
		`, domainID, orgID, ft)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("domain not found, not active, or receiving not enabled")
		}
		return nil
	})
}

// UpdateOrgDomainResendReceiving sets the receiving flags on a domain.
func (s *Store) UpdateOrgDomainResendReceiving(ctx context.Context, domainID string, enabled bool) error {
	return s.withLockedLegacyDomain(ctx, domainID, "", func(scoped *Store, _ legacyDomainIdentity, _ bool) error {
		if err := scoped.requireDomainWritesEnabled(ctx); err != nil {
			return err
		}
		_, err := scoped.q.ExecContext(ctx, `
			UPDATE org_domains
			SET resend_receiving_enabled = $1, inbound_enabled = $1, updated_at = now()
			WHERE id = $2
		`, enabled, domainID)
		return err
	})
}
