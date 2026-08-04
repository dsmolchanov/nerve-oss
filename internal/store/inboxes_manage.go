package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"
)

type InboxRecord struct {
	ID          string
	OrgID       string
	OrgDomainID sql.NullString
	Address     string
	Status      string
	CreatedAt   time.Time

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
		       forward_to
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
		       forward_to
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
		       forward_to
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
		       forward_to
		FROM inboxes
		WHERE lower(address) = lower($1)
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
	); err != nil {
		return rec, err
	}
	return rec, nil
}

func (s *Store) CreateInboxForOrg(ctx context.Context, orgID string, address string, orgDomainID string, outboundProviders ...string) (InboxRecord, error) {
	outboundProvider := "smtp"
	if len(outboundProviders) > 0 && outboundProviders[0] != "" {
		outboundProvider = outboundProviders[0]
	}
	rec := InboxRecord{
		ID:      uuid.NewString(),
		OrgID:   orgID,
		Address: address,
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
		INSERT INTO inboxes (id, org_id, org_domain_id, address, status, outbound_provider)
		VALUES ($1, $2, $3, $4, 'active', $5)
		RETURNING created_at
	`, rec.ID, rec.OrgID, domainRef, rec.Address, outboundProvider)
	if err := row.Scan(&rec.CreatedAt); err != nil {
		return InboxRecord{}, err
	}
	return rec, nil
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
	result, err := s.q.ExecContext(ctx, `
		UPDATE inboxes
		SET status = 'disabled'
		WHERE id = $1 AND org_id = $2
	`, inboxID, orgID)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}
