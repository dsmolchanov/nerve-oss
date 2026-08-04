package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrIdempotencyConflict = errors.New("external_ref already identifies a different resource")
	ErrResourceConflict    = errors.New("resource already exists with a different external_ref")
	ErrOrgNotEmpty         = errors.New("organization still owns resources")
)

type OrgRecord struct {
	ID          string
	Name        string
	ExternalRef sql.NullString
	CreatedAt   time.Time
	DeletedAt   sql.NullTime
}

// EnsureOrg creates an org once for a stable external reference. Replaying the
// same name/ref returns the original row; reusing the ref for different content
// is a typed conflict rather than a silent mutation.
func (s *Store) EnsureOrg(ctx context.Context, name, externalRef string) (OrgRecord, bool, error) {
	name = strings.TrimSpace(name)
	externalRef = strings.TrimSpace(externalRef)
	if name == "" {
		name = "organization"
	}
	if externalRef == "" {
		return OrgRecord{}, false, errors.New("missing external_ref")
	}

	rec := OrgRecord{ID: uuid.NewString()}
	row := s.q.QueryRowContext(ctx, `
		INSERT INTO orgs (id, name, external_ref)
		VALUES ($1, $2, $3)
		ON CONFLICT (external_ref) WHERE external_ref IS NOT NULL DO NOTHING
		RETURNING id::text, name, external_ref, created_at, deleted_at
	`, rec.ID, name, externalRef)
	if err := row.Scan(&rec.ID, &rec.Name, &rec.ExternalRef, &rec.CreatedAt, &rec.DeletedAt); err == nil {
		return rec, true, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return OrgRecord{}, false, err
	}

	rec, err := s.GetOrgByExternalRef(ctx, externalRef)
	if err != nil {
		return OrgRecord{}, false, err
	}
	if rec.Name != name || rec.DeletedAt.Valid {
		return OrgRecord{}, false, ErrIdempotencyConflict
	}
	return rec, false, nil
}

func (s *Store) GetOrgByExternalRef(ctx context.Context, externalRef string) (OrgRecord, error) {
	var rec OrgRecord
	err := s.q.QueryRowContext(ctx, `
		SELECT id::text, name, external_ref, created_at, deleted_at
		FROM orgs WHERE external_ref = $1
	`, strings.TrimSpace(externalRef)).Scan(
		&rec.ID, &rec.Name, &rec.ExternalRef, &rec.CreatedAt, &rec.DeletedAt,
	)
	return rec, err
}

func (s *Store) GetOrgByID(ctx context.Context, orgID string) (OrgRecord, error) {
	var rec OrgRecord
	err := s.q.QueryRowContext(ctx, `
		SELECT id::text, name, external_ref, created_at, deleted_at
		FROM orgs WHERE id = $1
	`, orgID).Scan(&rec.ID, &rec.Name, &rec.ExternalRef, &rec.CreatedAt, &rec.DeletedAt)
	return rec, err
}

// DeleteOrgIfEmpty intentionally refuses a cascading delete. Provider-backed
// resources must be reconciled first, after which the org row can be removed
// without silently orphaning external state.
func (s *Store) DeleteOrgIfEmpty(ctx context.Context, orgID string) (bool, error) {
	deleted := false
	err := s.withTx(ctx, func(scoped *Store) error {
		if err := scoped.lockReconciliationResources(ctx, "org:"+orgID); err != nil {
			return err
		}
		result, err := scoped.q.ExecContext(ctx, `
			UPDATE orgs o SET deleted_at = now()
			WHERE o.id = $1
			  AND o.deleted_at IS NULL
			  AND NOT EXISTS (SELECT 1 FROM service_tokens t
			                  WHERE t.org_id = o.id
			                    AND t.revoked_at IS NULL
			                    AND t.expires_at > now())
			  AND NOT EXISTS (SELECT 1 FROM org_domains d WHERE d.org_id = o.id)
			  AND NOT EXISTS (SELECT 1 FROM inboxes i WHERE i.org_id = o.id)
			  AND NOT EXISTS (SELECT 1 FROM cloud_api_keys k WHERE k.org_id = o.id AND k.revoked_at IS NULL)
			  AND NOT EXISTS (SELECT 1 FROM org_webhooks w WHERE w.org_id = o.id AND w.disabled_at IS NULL)
			  AND NOT EXISTS (SELECT 1 FROM org_domain_grants g
			                  WHERE (g.owner_org_id = o.id OR g.grantee_org_id = o.id)
			                    AND g.status = 'active')
		`, orgID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		deleted = n > 0
		return nil
	})
	return deleted, err
}

// LockOrgDomainForCleanup serializes the provider deletion with all inbox and
// grant creation that references the domain. Call it inside RunAsOrg, perform
// the idempotent provider delete, then delete the row before the callback
// commits. If the provider outcome is unknown, returning the error rolls the
// transaction back and leaves a durable record that can be reconciled safely.
func (s *Store) LockOrgDomainForCleanup(ctx context.Context, orgID, domainID string) (OrgDomain, error) {
	var d OrgDomain
	row := s.q.QueryRowContext(ctx, `
		SELECT d.id, d.org_id, d.domain, d.status, d.verification_token,
		       d.mx_verified, d.spf_verified, d.dkim_verified, d.dmarc_verified,
		       d.inbound_enabled, d.dkim_selector, d.dkim_private_key_enc, d.dkim_public_key,
		       d.dkim_method, d.last_check_at, d.verified_at, d.expires_at,
		       d.resend_domain_id, d.resend_domain_status, d.resend_dns_records,
		       d.resend_receiving_enabled, d.catch_all_enabled, d.forward_to,
		       d.created_at, d.updated_at, d.external_ref
		FROM org_domains d
		WHERE d.id = $1 AND d.org_id = $2
		FOR UPDATE
	`, domainID, orgID)
	if err := scanOrgDomain(row, &d); err != nil {
		return OrgDomain{}, err
	}

	var blocked bool
	if err := s.q.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM inboxes WHERE org_domain_id = $1)
		    OR EXISTS(SELECT 1 FROM org_domain_grants WHERE org_domain_id = $1)
	`, domainID).Scan(&blocked); err != nil {
		return OrgDomain{}, err
	}
	if blocked {
		return OrgDomain{}, ErrResourceConflict
	}
	return d, nil
}

type OrgDomainGrant struct {
	ID           string
	OwnerOrgID   string
	OrgDomainID  string
	GranteeOrgID string
	ExternalRef  string
	Status       string
	CreatedAt    time.Time
	RevokedAt    sql.NullTime
}

func (s *Store) EnsureOrgDomainGrant(ctx context.Context, ownerOrgID, domainID, granteeOrgID, externalRef string) (OrgDomainGrant, bool, error) {
	ownerOrgID = strings.TrimSpace(ownerOrgID)
	domainID = strings.TrimSpace(domainID)
	granteeOrgID = strings.TrimSpace(granteeOrgID)
	externalRef = strings.TrimSpace(externalRef)
	if ownerOrgID == "" || domainID == "" || granteeOrgID == "" || externalRef == "" {
		return OrgDomainGrant{}, false, errors.New("missing domain grant field")
	}
	if ownerOrgID == granteeOrgID {
		return OrgDomainGrant{}, false, errors.New("owner and grantee must differ")
	}

	var rec OrgDomainGrant
	created := false
	err := s.withTx(ctx, func(scoped *Store) error {
		if err := scoped.lockReconciliationResources(
			ctx, "domain:"+domainID, "org:"+ownerOrgID, "org:"+granteeOrgID,
		); err != nil {
			return err
		}
		var innerErr error
		rec, created, innerErr = scoped.ensureOrgDomainGrantLocked(
			ctx, ownerOrgID, domainID, granteeOrgID, externalRef,
		)
		return innerErr
	})
	return rec, created, err
}

func (s *Store) ensureOrgDomainGrantLocked(ctx context.Context, ownerOrgID, domainID, granteeOrgID, externalRef string) (OrgDomainGrant, bool, error) {
	var domainActive bool
	if err := s.q.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM org_domains
			WHERE id = $1 AND org_id = $2 AND status = 'active'
		)
	`, domainID, ownerOrgID).Scan(&domainActive); err != nil {
		return OrgDomainGrant{}, false, err
	}
	if !domainActive {
		return OrgDomainGrant{}, false, sql.ErrNoRows
	}
	var granteeActive bool
	if err := s.q.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM orgs WHERE id = $1 AND deleted_at IS NULL)
	`, granteeOrgID).Scan(&granteeActive); err != nil {
		return OrgDomainGrant{}, false, err
	}
	if !granteeActive {
		return OrgDomainGrant{}, false, sql.ErrNoRows
	}

	rec := OrgDomainGrant{ID: uuid.NewString()}
	row := s.q.QueryRowContext(ctx, `
		INSERT INTO org_domain_grants
			(id, owner_org_id, org_domain_id, grantee_org_id, external_ref)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (external_ref) DO NOTHING
		RETURNING id::text, owner_org_id::text, org_domain_id::text,
		          grantee_org_id::text, external_ref, status, created_at, revoked_at
	`, rec.ID, ownerOrgID, domainID, granteeOrgID, externalRef)
	if err := scanOrgDomainGrant(row, &rec); err == nil {
		return rec, true, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		// A different external_ref may already own the active domain/grantee pair.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "uq_org_domain_grants_active" {
			return OrgDomainGrant{}, false, ErrResourceConflict
		}
		return OrgDomainGrant{}, false, err
	}

	rec, err := s.GetOrgDomainGrantByExternalRef(ctx, externalRef)
	if err != nil {
		return OrgDomainGrant{}, false, err
	}
	if rec.OwnerOrgID != ownerOrgID || rec.OrgDomainID != domainID || rec.GranteeOrgID != granteeOrgID || rec.Status != "active" {
		return OrgDomainGrant{}, false, ErrIdempotencyConflict
	}
	return rec, false, nil
}

// lockReconciliationResources serializes multi-resource admin operations before
// their SQL statements take snapshots. Sorting gives every caller a stable lock
// order and avoids deadlocks when owner and grantee roles are reversed.
func (s *Store) lockReconciliationResources(ctx context.Context, resources ...string) error {
	keys := append([]string(nil), resources...)
	sort.Strings(keys)
	previous := ""
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" || key == previous {
			continue
		}
		if _, err := s.q.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
			return err
		}
		previous = key
	}
	return nil
}

func (s *Store) lockActiveOrgForReconciliation(ctx context.Context, orgID string) error {
	return s.lockActiveOrgResourcesForReconciliation(ctx, orgID)
}

func (s *Store) lockActiveOrgResourcesForReconciliation(ctx context.Context, orgID string, resources ...string) error {
	resources = append(resources, "org:"+orgID)
	if err := s.lockReconciliationResources(ctx, resources...); err != nil {
		return err
	}
	var active bool
	if err := s.q.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM orgs WHERE id = $1 AND deleted_at IS NULL)
	`, orgID).Scan(&active); err != nil {
		return err
	}
	if !active {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) GetOrgDomainGrantByExternalRef(ctx context.Context, externalRef string) (OrgDomainGrant, error) {
	var rec OrgDomainGrant
	row := s.q.QueryRowContext(ctx, `
		SELECT id::text, owner_org_id::text, org_domain_id::text,
		       grantee_org_id::text, external_ref, status, created_at, revoked_at
		FROM org_domain_grants WHERE external_ref = $1
	`, strings.TrimSpace(externalRef))
	return rec, scanOrgDomainGrant(row, &rec)
}

func (s *Store) ListOrgDomainGrants(ctx context.Context, ownerOrgID, granteeOrgID, domainID string) ([]OrgDomainGrant, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT id::text, owner_org_id::text, org_domain_id::text,
		       grantee_org_id::text, external_ref, status, created_at, revoked_at
		FROM org_domain_grants
		WHERE (nullif($1, '') IS NULL OR owner_org_id = nullif($1, '')::uuid)
		  AND (nullif($2, '') IS NULL OR grantee_org_id = nullif($2, '')::uuid)
		  AND (nullif($3, '') IS NULL OR org_domain_id = nullif($3, '')::uuid)
		ORDER BY created_at DESC
	`, ownerOrgID, granteeOrgID, domainID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrgDomainGrant
	for rows.Next() {
		var rec OrgDomainGrant
		if err := rows.Scan(&rec.ID, &rec.OwnerOrgID, &rec.OrgDomainID, &rec.GranteeOrgID,
			&rec.ExternalRef, &rec.Status, &rec.CreatedAt, &rec.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) RevokeOrgDomainGrant(ctx context.Context, grantID string) (bool, error) {
	revoked := false
	err := s.withTx(ctx, func(scoped *Store) error {
		var cloudMode, currentOrgID string
		if err := scoped.q.QueryRowContext(ctx, `
			SELECT coalesce(current_setting('app.cloud_mode', true), ''),
			       coalesce(current_setting('app.current_org_id', true), '')
		`).Scan(&cloudMode, &currentOrgID); err != nil {
			return err
		}
		ownerConstraint := ""
		bypassed := strings.EqualFold(cloudMode, "true")
		if bypassed {
			if currentOrgID == "" {
				return errors.New("missing current org for scoped grant revoke")
			}
			ownerConstraint = currentOrgID
			if _, err := scoped.q.ExecContext(ctx, `SELECT set_config('app.cloud_mode', 'false', true)`); err != nil {
				return err
			}
		}

		// This method is the narrow privileged path for checking grantee-owned
		// inboxes while an owner-scoped RLS transaction is active. The owner
		// predicate is retained explicitly before any mutation.
		return func() (resultErr error) {
			defer func() {
				if !bypassed {
					return
				}
				_, restoreErr := scoped.q.ExecContext(ctx, `SELECT set_config('app.cloud_mode', $1, true)`, cloudMode)
				resultErr = errors.Join(resultErr, restoreErr)
			}()

			var ownerOrgID string
			err := scoped.q.QueryRowContext(ctx, `
				SELECT owner_org_id::text
				FROM org_domain_grants
				WHERE id = $1 AND status = 'active'
				  AND (nullif($2, '') IS NULL OR owner_org_id = nullif($2, '')::uuid)
				FOR UPDATE
			`, grantID, ownerConstraint).Scan(&ownerOrgID)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}

			var hasActiveInboxes bool
			if err := scoped.q.QueryRowContext(ctx, `
				SELECT EXISTS(
				  SELECT 1
				  FROM org_domain_grants g
				  JOIN inboxes i ON i.org_domain_id = g.org_domain_id
				                AND i.org_id = g.grantee_org_id
				  WHERE g.id = $1 AND g.status = 'active' AND i.status = 'active'
				)
			`, grantID).Scan(&hasActiveInboxes); err != nil {
				return err
			}
			if hasActiveInboxes {
				return ErrResourceConflict
			}
			result, err := scoped.q.ExecContext(ctx, `
				UPDATE org_domain_grants
				SET status = 'revoked', revoked_at = now()
				WHERE id = $1 AND status = 'active' AND owner_org_id = $2
			`, grantID, ownerOrgID)
			if err != nil {
				return err
			}
			updated, err := result.RowsAffected()
			if err != nil {
				return err
			}
			revoked = updated == 1
			return nil
		}()
	})
	return revoked, err
}

func scanOrgDomainGrant(row *sql.Row, rec *OrgDomainGrant) error {
	return row.Scan(&rec.ID, &rec.OwnerOrgID, &rec.OrgDomainID, &rec.GranteeOrgID,
		&rec.ExternalRef, &rec.Status, &rec.CreatedAt, &rec.RevokedAt)
}

// GetActiveOrgDomainForInboxOrg resolves either an owned active domain or a
// platform-domain grant. It is safe inside RunAsOrg because the migration's RLS
// policy permits grantee SELECT but not mutation.
func (s *Store) GetActiveOrgDomainForInboxOrg(ctx context.Context, orgID, domainID, domain string) (OrgDomain, error) {
	var d OrgDomain
	row := s.q.QueryRowContext(ctx, `
		SELECT d.id, d.org_id, d.domain, d.status, d.verification_token,
		       d.mx_verified, d.spf_verified, d.dkim_verified, d.dmarc_verified,
		       d.inbound_enabled, d.dkim_selector, d.dkim_private_key_enc, d.dkim_public_key,
		       d.dkim_method, d.last_check_at, d.verified_at, d.expires_at,
		       d.resend_domain_id, d.resend_domain_status, d.resend_dns_records,
		       d.resend_receiving_enabled, d.catch_all_enabled, d.forward_to,
		       d.created_at, d.updated_at, d.external_ref
		FROM org_domains d
		WHERE d.status = 'active'
		  AND (nullif($2, '') IS NULL OR d.id = nullif($2, '')::uuid)
		  AND ($3 = '' OR lower(d.domain) = lower($3))
		  AND (
		    d.org_id = $1
		    OR EXISTS (
		      SELECT 1 FROM org_domain_grants g
		      WHERE g.org_domain_id = d.id
		        AND g.grantee_org_id = $1
		        AND g.status = 'active'
		    )
		  )
		LIMIT 1
	`, orgID, domainID, domain)
	return d, scanOrgDomain(row, &d)
}

// ResolveReceivingInbox performs global routing before a tenant is known. The
// returned inbox owns the eventual message even when the domain is platform-owned.
func (s *Store) ResolveReceivingInbox(ctx context.Context, address string) (InboxRecord, OrgDomain, error) {
	var inbox InboxRecord
	var domain OrgDomain
	row := s.q.QueryRowContext(ctx, `
		SELECT i.id, i.org_id, i.org_domain_id::text, i.address, i.status, i.created_at,
		       i.inbound_provider, i.outbound_provider,
		       i.inbound_provider_config_ref, i.outbound_provider_config_ref, i.forward_to,
		       d.id, d.org_id, d.domain, d.status, d.verification_token,
		       d.mx_verified, d.spf_verified, d.dkim_verified, d.dmarc_verified,
		       d.inbound_enabled, d.dkim_selector, d.dkim_private_key_enc, d.dkim_public_key,
		       d.dkim_method, d.last_check_at, d.verified_at, d.expires_at,
		       d.resend_domain_id, d.resend_domain_status, d.resend_dns_records,
		       d.resend_receiving_enabled, d.catch_all_enabled, d.forward_to,
		       d.created_at, d.updated_at
		FROM inboxes i
		JOIN org_domains d ON d.id = i.org_domain_id
		WHERE lower(i.address) = lower($1)
		  AND i.status = 'active'
		  AND d.status = 'active'
		  AND d.resend_receiving_enabled = true
		LIMIT 1
	`, strings.TrimSpace(address))
	err := row.Scan(
		&inbox.ID, &inbox.OrgID, &inbox.OrgDomainID, &inbox.Address, &inbox.Status, &inbox.CreatedAt,
		&inbox.InboundProvider, &inbox.OutboundProvider,
		&inbox.InboundProviderConfigRef, &inbox.OutboundProviderConfigRef, &inbox.ForwardTo,
		&domain.ID, &domain.OrgID, &domain.Domain, &domain.Status, &domain.VerificationToken,
		&domain.MXVerified, &domain.SPFVerified, &domain.DKIMVerified, &domain.DMARCVerified,
		&domain.InboundEnabled, &domain.DKIMSelector, &domain.DKIMPrivateKeyEnc, &domain.DKIMPublicKey,
		&domain.DKIMMethod, &domain.LastCheckAt, &domain.VerifiedAt, &domain.ExpiresAt,
		&domain.ResendDomainID, &domain.ResendStatus, &domain.ResendDNSRecords,
		&domain.ResendReceivingEnabled, &domain.CatchAllEnabled, &domain.ForwardTo,
		&domain.CreatedAt, &domain.UpdatedAt,
	)
	return inbox, domain, err
}
