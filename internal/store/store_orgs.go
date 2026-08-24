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
	if err := s.insertOrgWithAttachmentUsage(ctx, id, name); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) CreateDefaultOrg(ctx context.Context) (string, error) {
	id := uuid.NewString()
	if err := s.insertOrgWithAttachmentUsage(ctx, id, "default"); err != nil {
		return "", err
	}
	return id, nil
}

// insertOrgWithAttachmentUsage stays compatible with the additive rollout
// window: schema 0020/0021 does not have org_attachment_usage yet, while
// schema 0022+ requires every newly-created org to receive a quota row in the
// same transaction.
func (s *Store) insertOrgWithAttachmentUsage(ctx context.Context, id, name string) error {
	return s.withTx(ctx, func(scoped *Store) error {
		if _, err := scoped.q.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, $2)`, id, name); err != nil {
			return err
		}
		var usageTablePresent bool
		if err := scoped.q.QueryRowContext(ctx, `
			SELECT to_regclass('public.org_attachment_usage') IS NOT NULL
		`).Scan(&usageTablePresent); err != nil {
			return err
		}
		if !usageTablePresent {
			return nil
		}
		_, err := scoped.q.ExecContext(ctx, `
			INSERT INTO org_attachment_usage (org_id) VALUES ($1)
			ON CONFLICT (org_id) DO NOTHING
		`, id)
		return err
	})
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
	return s.ensureInbox(ctx, address)
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
