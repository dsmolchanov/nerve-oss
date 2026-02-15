package domains

import (
	"context"
	"database/sql"
)

type domainExpirer interface {
	ExpirePendingDomains(ctx context.Context) (int, error)
}

// ExpirePendingDomains deletes org_domains where status='pending' and expires_at < now().
// Called periodically (e.g., daily) or on domain registration to garbage-collect stale claims.
// Returns the number of deleted rows.
func ExpirePendingDomains(ctx context.Context, db *sql.DB) (int, error) {
	result, err := db.ExecContext(ctx, `DELETE FROM org_domains WHERE status = 'pending' AND expires_at < now()`)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}
