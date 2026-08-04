package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

func (s *Store) CreateOrg(ctx context.Context, name string) (string, error) {
	id := uuid.NewString()
	if name == "" {
		name = "organization"
	}
	_, err := s.q.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, $2)`, id, name)
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) CreateDefaultOrg(ctx context.Context) (string, error) {
	id := uuid.NewString()
	_, err := s.q.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1,$2)`, id, "default")
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) EnsureDefaultOrg(ctx context.Context) (string, error) {
	row := s.q.QueryRowContext(ctx, `SELECT id FROM orgs ORDER BY created_at ASC LIMIT 1`)
	var id string
	if err := row.Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return s.CreateDefaultOrg(ctx)
		}
		return "", err
	}
	return id, nil
}

func (s *Store) EnsureDefaultInbox(ctx context.Context, address string) (string, error) {
	orgID, err := s.EnsureDefaultOrg(ctx)
	if err != nil {
		return "", err
	}
	row := s.q.QueryRowContext(ctx, `SELECT id FROM inboxes WHERE address = $1`, address)
	var id string
	if err := row.Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			id = uuid.NewString()
			_, err = s.q.ExecContext(ctx, `INSERT INTO inboxes (id, org_id, address, status) VALUES ($1,$2,$3,'active')`, id, orgID, address)
			return id, err
		}
		return "", err
	}
	_, _ = s.q.ExecContext(ctx, `UPDATE inboxes SET org_id = COALESCE(org_id, $2) WHERE id = $1`, id, orgID)
	return id, nil
}

func (s *Store) EnsureDefaults(ctx context.Context, inboxAddress string) (string, error) {
	if inboxAddress == "" {
		return "", fmt.Errorf("missing inbox address")
	}
	return s.EnsureDefaultInbox(ctx, inboxAddress)
}

func (s *Store) GetOrgMCPEndpoint(ctx context.Context, orgID string) (string, error) {
	row := s.q.QueryRowContext(ctx, `
		SELECT coalesce(mcp_endpoint, '')
		FROM orgs
		WHERE id = $1
	`, orgID)
	var endpoint string
	if err := row.Scan(&endpoint); err != nil {
		return "", err
	}
	return endpoint, nil
}

func (s *Store) SetOrgMCPEndpoint(ctx context.Context, orgID string, endpoint string) (string, error) {
	row := s.q.QueryRowContext(ctx, `
		UPDATE orgs
		SET mcp_endpoint = nullif($2, ''),
		    updated_at = now()
		WHERE id = $1
		RETURNING coalesce(mcp_endpoint, '')
	`, orgID, strings.TrimSpace(endpoint))
	var stored string
	if err := row.Scan(&stored); err != nil {
		return "", err
	}
	return stored, nil
}
