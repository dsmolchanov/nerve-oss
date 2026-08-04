package store

import "context"

// SeedMissingOrgAttachmentUsage repairs orgs created by pre-0022 writers or
// by an interrupted rollout. It is a schema-window no-op before 0022 so the
// dual-reader binary remains healthy throughout the additive migration steps.
func (s *Store) SeedMissingOrgAttachmentUsage(ctx context.Context) (int, error) {
	var usageTablePresent bool
	if err := s.q.QueryRowContext(ctx, `
		SELECT to_regclass('public.org_attachment_usage') IS NOT NULL
	`).Scan(&usageTablePresent); err != nil {
		return 0, err
	}
	if !usageTablePresent {
		return 0, nil
	}

	var seeded int
	err := s.q.QueryRowContext(ctx, `
		WITH inserted AS (
			INSERT INTO org_attachment_usage (org_id, bytes_used)
			SELECT orgs.id, COALESCE(sum(attachment_blobs.size_bytes), 0)
			FROM orgs
			LEFT JOIN attachment_blobs ON attachment_blobs.org_id = orgs.id
			GROUP BY orgs.id
			ON CONFLICT (org_id) DO NOTHING
			RETURNING 1
		)
		SELECT count(*) FROM inserted
	`).Scan(&seeded)
	return seeded, err
}
