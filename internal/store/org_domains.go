package store

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/google/uuid"
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
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// CreateOrgDomain inserts a new domain registration. The domain must already be
// canonicalized (lowercase, no trailing dot). Sets expires_at = now() + 7 days for pending claims.
func (s *Store) CreateOrgDomain(ctx context.Context, orgID, domain, verificationToken, dkimSelector, dkimPrivateKeyEnc, dkimPublicKey, dkimMethod string) (string, error) {
	id := uuid.NewString()
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO org_domains (id, org_id, domain, verification_token, dkim_selector, dkim_private_key_enc, dkim_public_key, dkim_method, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now() + interval '7 days')
	`, id, orgID, strings.ToLower(domain), verificationToken, dkimSelector, nullIfEmpty(dkimPrivateKeyEnc), nullIfEmpty(dkimPublicKey), dkimMethod)
	if err != nil {
		return "", err
	}
	return id, nil
}

// GetOrgDomain retrieves a domain by its canonical domain name.
// In cloud mode with RLS, results are scoped to the current org.
func (s *Store) GetOrgDomain(ctx context.Context, domain string) (OrgDomain, error) {
	var d OrgDomain
	row := s.q.QueryRowContext(ctx, `
		SELECT id, org_id, domain, status, verification_token,
		       mx_verified, spf_verified, dkim_verified, dmarc_verified,
		       inbound_enabled, dkim_selector, dkim_private_key_enc, dkim_public_key,
		       dkim_method, last_check_at, verified_at, expires_at, created_at, updated_at
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
		       dkim_method, last_check_at, verified_at, expires_at, created_at, updated_at
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
		       dkim_method, last_check_at, verified_at, expires_at, created_at, updated_at
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
		       dkim_method, last_check_at, verified_at, expires_at, created_at, updated_at
		FROM org_domains
		WHERE org_id = $1
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
			&d.DKIMMethod, &d.LastCheckAt, &d.VerifiedAt, &d.ExpiresAt, &d.CreatedAt, &d.UpdatedAt,
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
		       dkim_method, last_check_at, verified_at, expires_at, created_at, updated_at
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
		&d.DKIMMethod, &d.LastCheckAt, &d.VerifiedAt, &d.ExpiresAt, &d.CreatedAt, &d.UpdatedAt,
	)
}

func nullIfEmpty(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
