package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

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

// CreateOrgDomain inserts a new domain registration. The domain must already be
// canonicalized (lowercase, no trailing dot). Sets expires_at = now() + 7 days for pending claims.
func (s *Store) CreateOrgDomain(ctx context.Context, orgID, domain, verificationToken, dkimSelector, dkimPrivateKeyEnc, dkimPublicKey, dkimMethod string) (string, error) {
	return s.createOrgDomain(ctx, orgID, domain, verificationToken, dkimSelector, dkimPrivateKeyEnc, dkimPublicKey, dkimMethod, "")
}

func (s *Store) createOrgDomain(ctx context.Context, orgID, domain, verificationToken, dkimSelector, dkimPrivateKeyEnc, dkimPublicKey, dkimMethod, externalRef string) (string, error) {
	id := uuid.NewString()
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO org_domains (id, org_id, domain, verification_token, dkim_selector, dkim_private_key_enc, dkim_public_key, dkim_method, expires_at, external_ref)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now() + interval '7 days', nullif($9, ''))
	`, id, orgID, strings.ToLower(domain), verificationToken, dkimSelector, nullIfEmpty(dkimPrivateKeyEnc), nullIfEmpty(dkimPublicKey), dkimMethod, externalRef)
	if err != nil {
		return "", err
	}
	return id, nil
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
		canonicalDomain := strings.ToLower(strings.TrimSpace(domain))
		if err := scoped.lockActiveOrgResourcesForReconciliation(ctx, orgID, "domain:"+canonicalDomain); err != nil {
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
	canonicalDomain := strings.ToLower(strings.TrimSpace(domain))
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
	err := s.q.QueryRowContext(ctx, `
		INSERT INTO org_domains
		  (id, org_id, domain, verification_token, dkim_selector,
		   dkim_private_key_enc, dkim_public_key, dkim_method, expires_at, external_ref)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now() + interval '7 days', $9)
		ON CONFLICT (external_ref) WHERE external_ref IS NOT NULL DO NOTHING
		RETURNING id::text
	`, id, orgID, canonicalDomain, verificationToken, dkimSelector,
		nullIfEmpty(dkimPrivateKeyEnc), nullIfEmpty(dkimPublicKey), dkimMethod, externalRef,
	).Scan(&id)
	if err == nil {
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
	if existing.OrgID != orgID || !strings.EqualFold(existing.Domain, domain) || existing.DKIMMethod != dkimMethod {
		return OrgDomain{}, false, ErrIdempotencyConflict
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
		       created_at, updated_at, external_ref
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
		       created_at, updated_at, external_ref
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
		       created_at, updated_at, external_ref
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
		       created_at, updated_at, external_ref
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
		       created_at, updated_at, external_ref
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
	_, err := s.q.ExecContext(ctx, `
		UPDATE org_domains
		SET mx_verified = $2, spf_verified = $3, dkim_verified = $4, dmarc_verified = $5,
		    status = $6, last_check_at = now(),
		    verified_at = CASE WHEN $6 IN ('verified_dns', 'active') THEN now() ELSE verified_at END,
		    updated_at = now()
		WHERE id = $1
	`, id, mx, spf, dkim, dmarc, status)
	return err
}

// UpdateOrgDomainStatus transitions domain to a new status.
func (s *Store) UpdateOrgDomainStatus(ctx context.Context, id string, status string) error {
	q := `UPDATE org_domains SET status = $2, updated_at = now() WHERE id = $1`
	if status == "active" || status == "verified_dns" {
		q = `UPDATE org_domains SET status = $2, verified_at = now(), updated_at = now() WHERE id = $1`
	}
	_, err := s.q.ExecContext(ctx, q, id, status)
	return err
}

// DeleteOrgDomain removes a domain registration.
func (s *Store) DeleteOrgDomain(ctx context.Context, id string) error {
	_, err := s.q.ExecContext(ctx, `DELETE FROM org_domains WHERE id = $1`, id)
	return err
}

// DeleteOrgDomainForOrg removes a domain registration, scoped to an org.
// Returns true if a row was deleted.
func (s *Store) DeleteOrgDomainForOrg(ctx context.Context, orgID, id string) (bool, error) {
	result, err := s.q.ExecContext(ctx, `DELETE FROM org_domains WHERE id = $1 AND org_id = $2`, id, orgID)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
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
		       created_at, updated_at, external_ref
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
	result, err := s.q.ExecContext(ctx, `DELETE FROM org_domains WHERE status = 'pending' AND expires_at < now()`)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
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
	_, err := s.q.ExecContext(ctx, `
		UPDATE org_domains
		SET resend_domain_id = nullif($2, ''),
		    resend_domain_status = nullif($3, ''),
		    resend_dns_records = coalesce($4::jsonb, resend_dns_records),
		    updated_at = now()
		WHERE id = $1
	`, id, resendDomainID, resendStatus, dnsRecords)
	return err
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
		       created_at, updated_at, external_ref
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
	result, err := s.q.ExecContext(ctx, `
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
	result, err := s.q.ExecContext(ctx, `
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
}

// UpdateOrgDomainResendReceiving sets the receiving flags on a domain.
func (s *Store) UpdateOrgDomainResendReceiving(ctx context.Context, domainID string, enabled bool) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE org_domains
		SET resend_receiving_enabled = $1, inbound_enabled = $1, updated_at = now()
		WHERE id = $2
	`, enabled, domainID)
	return err
}
