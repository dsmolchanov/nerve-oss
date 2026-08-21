package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"neuralmail/internal/emailaddr"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

type InboxRecord struct {
	ID          string
	OrgID       string
	OrgDomainID sql.NullString
	Address     string
	Status      string
	CreatedAt   time.Time
	ExternalRef sql.NullString

	InboundProvider           string
	OutboundProvider          string
	InboundProviderConfigRef  sql.NullString
	OutboundProviderConfigRef sql.NullString

	ForwardTo sql.NullString
}

func (s *Store) GetInboxRecordByID(ctx context.Context, inboxID string) (InboxRecord, error) {
	var rec InboxRecord
	row := s.q.QueryRowContext(ctx, `
		SELECT id, org_id, org_domain_id::text, address, status, created_at,
		       inbound_provider, outbound_provider,
		       inbound_provider_config_ref, outbound_provider_config_ref,
		       forward_to, external_ref
		FROM inboxes
		WHERE id = $1
	`, inboxID)
	if err := row.Scan(
		&rec.ID,
		&rec.OrgID,
		&rec.OrgDomainID,
		&rec.Address,
		&rec.Status,
		&rec.CreatedAt,
		&rec.InboundProvider,
		&rec.OutboundProvider,
		&rec.InboundProviderConfigRef,
		&rec.OutboundProviderConfigRef,
		&rec.ForwardTo,
		&rec.ExternalRef,
	); err != nil {
		return rec, err
	}
	return rec, nil
}

func (s *Store) GetInboxRecordByIDForOrg(ctx context.Context, orgID string, inboxID string) (InboxRecord, error) {
	var rec InboxRecord
	row := s.q.QueryRowContext(ctx, `
		SELECT id, org_id, org_domain_id::text, address, status, created_at,
		       inbound_provider, outbound_provider,
		       inbound_provider_config_ref, outbound_provider_config_ref,
		       forward_to, external_ref
		FROM inboxes
		WHERE id = $1 AND org_id = $2
	`, inboxID, orgID)
	if err := row.Scan(
		&rec.ID,
		&rec.OrgID,
		&rec.OrgDomainID,
		&rec.Address,
		&rec.Status,
		&rec.CreatedAt,
		&rec.InboundProvider,
		&rec.OutboundProvider,
		&rec.InboundProviderConfigRef,
		&rec.OutboundProviderConfigRef,
		&rec.ForwardTo,
		&rec.ExternalRef,
	); err != nil {
		return rec, err
	}
	return rec, nil
}

func (s *Store) ListInboxRecordsByOrg(ctx context.Context, orgID string) ([]InboxRecord, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, org_id, org_domain_id::text, address, status, created_at,
		       inbound_provider, outbound_provider,
		       inbound_provider_config_ref, outbound_provider_config_ref,
		       forward_to, external_ref
		FROM inboxes
		WHERE org_id = $1
		ORDER BY created_at DESC
	`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []InboxRecord
	for rows.Next() {
		var rec InboxRecord
		if err := rows.Scan(
			&rec.ID,
			&rec.OrgID,
			&rec.OrgDomainID,
			&rec.Address,
			&rec.Status,
			&rec.CreatedAt,
			&rec.InboundProvider,
			&rec.OutboundProvider,
			&rec.InboundProviderConfigRef,
			&rec.OutboundProviderConfigRef,
			&rec.ForwardTo,
			&rec.ExternalRef,
		); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) GetInboxByAddress(ctx context.Context, address string) (InboxRecord, error) {
	var rec InboxRecord
	row := s.q.QueryRowContext(ctx, `
		SELECT id, org_id, org_domain_id::text, address, status, created_at,
		       inbound_provider, outbound_provider,
		       inbound_provider_config_ref, outbound_provider_config_ref,
		       forward_to, external_ref
		FROM inboxes
		WHERE lower(address) = lower($1) AND status = 'active'
		ORDER BY created_at DESC
		LIMIT 1
	`, address)
	if err := row.Scan(
		&rec.ID,
		&rec.OrgID,
		&rec.OrgDomainID,
		&rec.Address,
		&rec.Status,
		&rec.CreatedAt,
		&rec.InboundProvider,
		&rec.OutboundProvider,
		&rec.InboundProviderConfigRef,
		&rec.OutboundProviderConfigRef,
		&rec.ForwardTo,
		&rec.ExternalRef,
	); err != nil {
		return rec, err
	}
	return rec, nil
}

func (s *Store) CreateInboxForOrg(ctx context.Context, orgID string, address string, orgDomainID string, outboundProviders ...string) (InboxRecord, error) {
	outboundProvider := "smtp"
	if len(outboundProviders) > 0 && strings.TrimSpace(outboundProviders[0]) != "" {
		outboundProvider = outboundProviders[0]
	}
	return s.createInboxForOrg(ctx, orgID, address, orgDomainID, outboundProvider, "")
}

func (s *Store) createInboxForOrg(ctx context.Context, orgID string, address string, orgDomainID string, outboundProvider string, externalRef string) (InboxRecord, error) {
	canonicalAddress, _, _, err := emailaddr.Canonicalize(address)
	if err != nil {
		return InboxRecord{}, err
	}
	if outboundProvider == "" {
		outboundProvider = "smtp"
	}
	rec := InboxRecord{
		ID:      uuid.NewString(),
		OrgID:   orgID,
		Address: canonicalAddress,
		Status:  "active",

		InboundProvider:  "jmap",
		OutboundProvider: outboundProvider,
	}

	var domainRef any
	if orgDomainID == "" {
		domainRef = nil
	} else {
		domainRef = orgDomainID
		rec.OrgDomainID = sql.NullString{String: orgDomainID, Valid: true}
	}

	row := s.q.QueryRowContext(ctx, `
		INSERT INTO inboxes (id, org_id, org_domain_id, address, status, outbound_provider, external_ref)
		VALUES ($1, $2, $3, $4, 'active', $5, nullif($6, ''))
		RETURNING created_at
	`, rec.ID, rec.OrgID, domainRef, rec.Address, outboundProvider, externalRef)
	if err := row.Scan(&rec.CreatedAt); err != nil {
		if isCanonicalInboxAddressConflict(err) {
			return InboxRecord{}, ErrResourceConflict
		}
		return InboxRecord{}, err
	}
	return rec, nil
}

// EnsureInboxForOrg is the idempotent provisioning path. The external_ref may
// be replayed only with the exact same org/address/domain/provider tuple.
func (s *Store) EnsureInboxForOrg(ctx context.Context, orgID, address, orgDomainID, outboundProvider, externalRef string) (InboxRecord, bool, error) {
	externalRef = strings.TrimSpace(externalRef)
	if externalRef == "" {
		return InboxRecord{}, false, errors.New("missing external_ref")
	}
	var rec InboxRecord
	created := false
	err := s.withTx(ctx, func(scoped *Store) error {
		canonicalAddress, _, canonicalDomain, err := emailaddr.Canonicalize(address)
		if err != nil {
			return err
		}
		if err := scoped.lockCanonicalDomain(ctx, canonicalDomain); err != nil {
			return err
		}
		if err := scoped.lockActiveOrgForReconciliation(ctx, orgID); err != nil {
			return err
		}
		var ensureErr error
		rec, created, ensureErr = scoped.ensureInboxForOrgLocked(
			ctx, orgID, canonicalAddress, orgDomainID, outboundProvider, externalRef,
		)
		return ensureErr
	})
	return rec, created, err
}

func (s *Store) ensureInboxForOrgLocked(ctx context.Context, orgID, address, orgDomainID, outboundProvider, externalRef string) (InboxRecord, bool, error) {
	if outboundProvider == "" {
		outboundProvider = "smtp"
	}

	rec := InboxRecord{
		ID:               uuid.NewString(),
		OrgID:            orgID,
		Address:          address,
		Status:           "active",
		ExternalRef:      sql.NullString{String: externalRef, Valid: true},
		InboundProvider:  "jmap",
		OutboundProvider: outboundProvider,
	}
	var domainRef any
	if orgDomainID != "" {
		domainRef = orgDomainID
		rec.OrgDomainID = sql.NullString{String: orgDomainID, Valid: true}
	}

	// Use the external-ref unique index as the serialization point so two
	// reconcilers cannot both pass a pre-read and make the loser surface a raw
	// unique-constraint error.
	err := s.q.QueryRowContext(ctx, `
		INSERT INTO inboxes
		  (id, org_id, org_domain_id, address, status, outbound_provider, external_ref)
		VALUES ($1, $2, $3, $4, 'active', $5, $6)
		ON CONFLICT (external_ref) WHERE external_ref IS NOT NULL DO NOTHING
		RETURNING created_at
	`, rec.ID, rec.OrgID, domainRef, rec.Address, outboundProvider, externalRef).Scan(&rec.CreatedAt)
	if err == nil {
		return rec, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		if isCanonicalInboxAddressConflict(err) {
			return InboxRecord{}, false, ErrResourceConflict
		}
		return InboxRecord{}, false, err
	}

	rec, err = s.GetInboxByExternalRef(ctx, externalRef)
	if err != nil {
		return InboxRecord{}, false, err
	}
	storedCanonical, _, _, canonicalErr := emailaddr.Canonicalize(rec.Address)
	if canonicalErr != nil || rec.OrgID != orgID || storedCanonical != address || rec.OrgDomainID.String != orgDomainID || rec.OutboundProvider != outboundProvider {
		return InboxRecord{}, false, ErrIdempotencyConflict
	}
	if rec.Address != storedCanonical {
		result, updateErr := s.q.ExecContext(ctx, `
			UPDATE inboxes
			SET address = $2
			WHERE id = $1
			  AND NOT EXISTS (
			    SELECT 1 FROM inboxes other
			    WHERE other.id <> $1
			      AND lower(btrim(other.address)) IN ($2, $2 || '.')
			  )
		`, rec.ID, storedCanonical)
		if updateErr != nil {
			if isCanonicalInboxAddressConflict(updateErr) {
				return InboxRecord{}, false, ErrResourceConflict
			}
			return InboxRecord{}, false, updateErr
		}
		updated, updateErr := result.RowsAffected()
		if updateErr != nil {
			return InboxRecord{}, false, updateErr
		}
		if updated != 1 {
			return InboxRecord{}, false, ErrResourceConflict
		}
		rec.Address = storedCanonical
	}
	return rec, false, nil
}

func isCanonicalInboxAddressConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "uq_inboxes_canonical_address"
}

func (s *Store) GetInboxByExternalRef(ctx context.Context, externalRef string) (InboxRecord, error) {
	var rec InboxRecord
	row := s.q.QueryRowContext(ctx, `
		SELECT id, org_id, org_domain_id::text, address, status, created_at,
		       inbound_provider, outbound_provider,
		       inbound_provider_config_ref, outbound_provider_config_ref,
		       forward_to, external_ref
		FROM inboxes WHERE external_ref = $1
	`, strings.TrimSpace(externalRef))
	err := row.Scan(
		&rec.ID, &rec.OrgID, &rec.OrgDomainID, &rec.Address, &rec.Status, &rec.CreatedAt,
		&rec.InboundProvider, &rec.OutboundProvider,
		&rec.InboundProviderConfigRef, &rec.OutboundProviderConfigRef,
		&rec.ForwardTo, &rec.ExternalRef,
	)
	return rec, err
}

func (s *Store) ReactivateInboxForOrg(ctx context.Context, orgID, inboxID string) (bool, error) {
	// Inbox status is evidence the outbound policy reads, so the change takes
	// the same fence as an enqueue rather than racing it.
	var changed bool
	err := s.FenceOrgPolicy(ctx, orgID, func(scoped *Store) error {
		result, err := scoped.q.ExecContext(ctx, `
			UPDATE inboxes SET status = 'active'
			WHERE id = $1 AND org_id = $2 AND status = 'disabled'
		`, inboxID, orgID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		changed = n > 0
		return err
	})
	return changed, err
}

func (s *Store) UpdateInboxOutboundProvider(ctx context.Context, inboxID string, provider string) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE inboxes SET outbound_provider = $2 WHERE id = $1
	`, inboxID, provider)
	return err
}

func (s *Store) UpdateInboxesOutboundProviderByDomain(ctx context.Context, orgDomainID string, provider string) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE inboxes SET outbound_provider = $2 WHERE org_domain_id = $1
	`, orgDomainID, provider)
	return err
}

// MigrateInboxesOutboundToResend switches all inboxes still using smtp to resend.
// Called at startup when Resend is the configured provider.
func (s *Store) MigrateInboxesOutboundToResend(ctx context.Context) (int64, error) {
	result, err := s.q.ExecContext(ctx, `
		UPDATE inboxes
		SET outbound_provider = 'resend'
		WHERE outbound_provider = 'smtp'
	`)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// UpdateInboxForwardTo sets or clears the forwarding address on an inbox.
// Empty string clears forwarding.
func (s *Store) UpdateInboxForwardTo(ctx context.Context, orgID, inboxID, forwardTo string) error {
	var ft any
	if forwardTo == "" {
		ft = nil
	} else {
		ft = forwardTo
	}
	result, err := s.q.ExecContext(ctx, `
		UPDATE inboxes SET forward_to = $3 WHERE id = $1 AND org_id = $2
	`, inboxID, orgID, ft)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) DisableInboxForOrg(ctx context.Context, orgID string, inboxID string) (bool, error) {
	var changed bool
	err := s.FenceOrgPolicy(ctx, orgID, func(scoped *Store) error {
		result, err := scoped.q.ExecContext(ctx, `
			UPDATE inboxes
			SET status = 'disabled'
			WHERE id = $1 AND org_id = $2
		`, inboxID, orgID)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		changed = rows > 0
		return nil
	})
	return changed, err
}

// DeleteInboxForOrg permanently removes a bootstrap-managed inbox and its
// cascading mailbox data. Tenant-facing deletion must continue to use
// DisableInboxForOrg so accidental user deletion remains recoverable.
func (s *Store) DeleteInboxForOrg(ctx context.Context, orgID string, inboxID string) (bool, error) {
	var changed bool
	err := s.FenceOrgPolicy(ctx, orgID, func(scoped *Store) error {
		result, err := scoped.q.ExecContext(ctx, `
			DELETE FROM inboxes
			WHERE id = $1 AND org_id = $2
		`, inboxID, orgID)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		changed = rows > 0
		return nil
	})
	return changed, err
}
