package tools

import (
	"context"
	"neuralmail/internal/localauth"
)

// CheckLocalAccess binds all ownership lookups to the caller's inbox set.
// It is independent of cloud RLS and is also used by resource reads.
func (s *Service) CheckLocalAccess(ctx context.Context, kind, id string) error {
	identity, restricted := localauth.FromContext(ctx)
	if !restricted {
		return nil
	}
	var query string
	switch kind {
	case "inbox":
		for _, allowed := range identity.InboxIDs {
			if id == allowed {
				return nil
			}
		}
		return localauth.ErrForbidden
	case "thread":
		query = `SELECT EXISTS(SELECT 1 FROM threads WHERE id::text=$1 AND inbox_id::text=ANY($2))`
	case "message":
		query = `SELECT EXISTS(SELECT 1 FROM messages m JOIN threads t ON t.id=m.thread_id WHERE m.id::text=$1 AND t.inbox_id::text=ANY($2))`
	default:
		return localauth.ErrForbidden
	}
	var allowed bool
	if err := s.Store.DB().QueryRowContext(ctx, query, id, identity.InboxIDs).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return localauth.ErrForbidden
	}
	return nil
}

func (s *Service) LocalInboxes(ctx context.Context) ([]string, error) {
	identity, restricted := localauth.FromContext(ctx)
	if !restricted {
		return s.Store.ListInboxes(ctx)
	}
	rows, err := s.Store.DB().QueryContext(ctx, `SELECT id::text FROM inboxes WHERE id::text=ANY($1) ORDER BY id`, identity.InboxIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
