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

// ReconcileOrgAttachmentUsage repairs quota accounting from the durable blob
// rows. The usage row is locked before the sum is read, matching the lock order
// used by blob insert/reuse and GC so a concurrent charge cannot be overwritten
// by a stale snapshot.
func (s *Store) ReconcileOrgAttachmentUsage(ctx context.Context) (int, error) {
	repaired := 0
	err := s.withTx(ctx, func(scoped *Store) error {
		rows, err := scoped.q.QueryContext(ctx, `
			SELECT org_id::text
			FROM org_attachment_usage
			ORDER BY org_id
		`)
		if err != nil {
			return err
		}
		var orgIDs []string
		for rows.Next() {
			var orgID string
			if err := rows.Scan(&orgID); err != nil {
				rows.Close()
				return err
			}
			orgIDs = append(orgIDs, orgID)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}

		for _, orgID := range orgIDs {
			var current int64
			if err := scoped.q.QueryRowContext(ctx, `
				SELECT bytes_used
				FROM org_attachment_usage
				WHERE org_id = $1
				FOR UPDATE
			`, orgID).Scan(&current); err != nil {
				return err
			}
			var actual int64
			if err := scoped.q.QueryRowContext(ctx, `
				SELECT COALESCE(sum(size_bytes), 0)::bigint
				FROM attachment_blobs
				WHERE org_id = $1
			`, orgID).Scan(&actual); err != nil {
				return err
			}
			if current == actual {
				continue
			}
			if _, err := scoped.q.ExecContext(ctx, `
				UPDATE org_attachment_usage
				SET bytes_used = $2, updated_at = now()
				WHERE org_id = $1
			`, orgID, actual); err != nil {
				return err
			}
			repaired++
		}
		return nil
	})
	return repaired, err
}
