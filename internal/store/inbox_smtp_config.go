package store

import (
	"context"
	"time"
)

type InboxSMTPConfig struct {
	ID              string
	InboxID         string
	OrgID           string
	Host            string
	Port            int
	Username        string
	PasswordEnc     string
	RequireSTARTTLS bool
	FromName        string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// UpsertInboxSMTPConfig creates or updates the SMTP configuration for an inbox.
// It also sets outbound_provider = 'smtp' and outbound_provider_config_ref on the inbox.
func (s *Store) UpsertInboxSMTPConfig(ctx context.Context, orgID, inboxID string, cfg InboxSMTPConfig) error {
	var configID string
	row := s.q.QueryRowContext(ctx, `
		INSERT INTO inbox_smtp_configs (inbox_id, org_id, host, port, username, password_enc, require_starttls, from_name)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (inbox_id) DO UPDATE SET
			host = EXCLUDED.host,
			port = EXCLUDED.port,
			username = EXCLUDED.username,
			password_enc = EXCLUDED.password_enc,
			require_starttls = EXCLUDED.require_starttls,
			from_name = EXCLUDED.from_name,
			updated_at = now()
		RETURNING id
	`, inboxID, orgID, cfg.Host, cfg.Port, cfg.Username, cfg.PasswordEnc, cfg.RequireSTARTTLS, cfg.FromName)
	if err := row.Scan(&configID); err != nil {
		return err
	}

	// Wire the inbox to use custom SMTP via the config ref
	_, err := s.q.ExecContext(ctx, `
		UPDATE inboxes
		SET outbound_provider = 'smtp',
		    outbound_provider_config_ref = $2
		WHERE id = $1
	`, inboxID, configID)
	return err
}

// GetInboxSMTPConfig retrieves the SMTP configuration for an inbox.
func (s *Store) GetInboxSMTPConfig(ctx context.Context, orgID, inboxID string) (InboxSMTPConfig, error) {
	var cfg InboxSMTPConfig
	row := s.q.QueryRowContext(ctx, `
		SELECT id, inbox_id, org_id, host, port, username, password_enc,
		       require_starttls, coalesce(from_name, ''), created_at, updated_at
		FROM inbox_smtp_configs
		WHERE inbox_id = $1 AND org_id = $2
	`, inboxID, orgID)
	if err := row.Scan(
		&cfg.ID, &cfg.InboxID, &cfg.OrgID, &cfg.Host, &cfg.Port,
		&cfg.Username, &cfg.PasswordEnc, &cfg.RequireSTARTTLS, &cfg.FromName,
		&cfg.CreatedAt, &cfg.UpdatedAt,
	); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// GetInboxSMTPConfigByRef loads an SMTP config by its ID (the config_ref value).
func (s *Store) GetInboxSMTPConfigByRef(ctx context.Context, configID string) (InboxSMTPConfig, error) {
	var cfg InboxSMTPConfig
	row := s.q.QueryRowContext(ctx, `
		SELECT id, inbox_id, org_id, host, port, username, password_enc,
		       require_starttls, coalesce(from_name, ''), created_at, updated_at
		FROM inbox_smtp_configs
		WHERE id = $1
	`, configID)
	if err := row.Scan(
		&cfg.ID, &cfg.InboxID, &cfg.OrgID, &cfg.Host, &cfg.Port,
		&cfg.Username, &cfg.PasswordEnc, &cfg.RequireSTARTTLS, &cfg.FromName,
		&cfg.CreatedAt, &cfg.UpdatedAt,
	); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// DeleteInboxSMTPConfig removes the SMTP configuration and reverts the inbox to default provider.
func (s *Store) DeleteInboxSMTPConfig(ctx context.Context, orgID, inboxID string) error {
	_, err := s.q.ExecContext(ctx, `
		DELETE FROM inbox_smtp_configs WHERE inbox_id = $1 AND org_id = $2
	`, inboxID, orgID)
	if err != nil {
		return err
	}

	// Clear the config ref and revert to resend provider (or keep smtp for global fallback)
	_, err = s.q.ExecContext(ctx, `
		UPDATE inboxes
		SET outbound_provider_config_ref = NULL,
		    outbound_provider = CASE
		        WHEN EXISTS (SELECT 1 FROM org_domains od
		                     JOIN inboxes i ON i.org_domain_id = od.id
		                     WHERE i.id = $1 AND od.resend_domain_id IS NOT NULL)
		        THEN 'resend' ELSE outbound_provider END
		WHERE id = $1
	`, inboxID)
	return err
}
