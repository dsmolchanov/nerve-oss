package store

import "context"

// CloudSchemaSupportsM2M reports whether this database carries the Cloud
// schema 9+ managed-mailbox and legacy-domain provider lifecycle objects.
// The lifecycle store methods refuse pre-Cloud-9 databases outright, so
// callers probe this first to preserve the pre-Cloud-9 immediate-delete
// compatibility contract instead of stranding releases behind a hard error.
func (s *Store) CloudSchemaSupportsM2M(ctx context.Context) (bool, error) {
	return s.cloudSchemaSupportsM2M(ctx)
}
