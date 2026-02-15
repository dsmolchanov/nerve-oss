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
}

func (s *Store) ListInboxRecordsByOrg(ctx context.Context, orgID string) ([]InboxRecord, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, org_id, org_domain_id::text, address, status, created_at
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
		if err := rows.Scan(&rec.ID, &rec.OrgID, &rec.OrgDomainID, &rec.Address, &rec.Status, &rec.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) GetInboxByAddress(ctx context.Context, address string) (InboxRecord, error) {
	var rec InboxRecord
	row := s.q.QueryRowContext(ctx, `
		SELECT id, org_id, org_domain_id::text, address, status, created_at
		FROM inboxes
		WHERE lower(address) = lower($1)
		ORDER BY created_at DESC
		LIMIT 1
	`, address)
	if err := row.Scan(&rec.ID, &rec.OrgID, &rec.OrgDomainID, &rec.Address, &rec.Status, &rec.CreatedAt); err != nil {
		return rec, err
	}
	return rec, nil
}

func (s *Store) CreateInboxForOrg(ctx context.Context, orgID string, address string, orgDomainID string) (InboxRecord, error) {
	rec := InboxRecord{
		ID:      uuid.NewString(),
		OrgID:   orgID,
		Address: address,
		Status:  "active",
	}

	var domainRef any
	if orgDomainID == "" {
		domainRef = nil
	} else {
		domainRef = orgDomainID
		rec.OrgDomainID = sql.NullString{String: orgDomainID, Valid: true}
	}

	row := s.q.QueryRowContext(ctx, `
		INSERT INTO inboxes (id, org_id, org_domain_id, address, status)
		VALUES ($1, $2, $3, $4, 'active')
		RETURNING created_at
	`, rec.ID, rec.OrgID, domainRef, rec.Address)
	if err := row.Scan(&rec.CreatedAt); err != nil {
		return InboxRecord{}, err
	}
	return rec, nil
}

