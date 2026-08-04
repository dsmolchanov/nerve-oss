package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrAttachmentQuotaExceeded = errors.New("attachment storage quota exceeded")

type AttachmentBlobInfo struct {
	OrgID       string
	SHA256      string
	SizeBytes   int64
	ContentType string
	RefCount    int
	CreatedAt   time.Time
	LastRefAt   time.Time
}

// StoreAttachmentBlob serializes quota accounting per org, inserts content
// once per digest, and charges bytes only for a real insert. Reference counts
// are intentionally left to the attachment-reference triggers.
func (s *Store) StoreAttachmentBlob(
	ctx context.Context,
	orgID string,
	contentType string,
	content []byte,
) (digest string, inserted bool, err error) {
	if strings.TrimSpace(orgID) == "" {
		return "", false, errors.New("missing attachment org id")
	}
	if len(content) == 0 {
		return "", false, errors.New("attachment content is empty")
	}
	contentType = strings.TrimSpace(contentType)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	sum := sha256.Sum256(content)
	digest = hex.EncodeToString(sum[:])
	size := int64(len(content))

	err = s.withTx(ctx, func(scoped *Store) error {
		if _, err := scoped.q.ExecContext(ctx, `
			INSERT INTO org_attachment_usage (org_id, bytes_used)
			SELECT $1, COALESCE(sum(size_bytes), 0)
			FROM attachment_blobs
			WHERE org_id = $1
			ON CONFLICT (org_id) DO NOTHING
		`, orgID); err != nil {
			return err
		}

		var used, quota int64
		if err := scoped.q.QueryRowContext(ctx, `
			SELECT bytes_used, bytes_quota
			FROM org_attachment_usage
			WHERE org_id = $1
			FOR UPDATE
		`, orgID).Scan(&used, &quota); err != nil {
			return err
		}

		var storedSize int64
		insertErr := scoped.q.QueryRowContext(ctx, `
			INSERT INTO attachment_blobs
			  (org_id, sha256, size_bytes, content_type, content)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (org_id, sha256) DO NOTHING
			RETURNING size_bytes
		`, orgID, digest, size, contentType, content).Scan(&storedSize)
		if errors.Is(insertErr, sql.ErrNoRows) {
			inserted = false
			return nil
		}
		if insertErr != nil {
			return insertErr
		}
		if storedSize != size {
			return fmt.Errorf("stored attachment size=%d, want %d", storedSize, size)
		}
		inserted = true
		if used > quota || size > quota-used {
			return fmt.Errorf(
				"%w: used=%d size=%d quota=%d",
				ErrAttachmentQuotaExceeded,
				used,
				size,
				quota,
			)
		}
		result, err := scoped.q.ExecContext(ctx, `
			UPDATE org_attachment_usage
			SET bytes_used = bytes_used + $2, updated_at = now()
			WHERE org_id = $1
		`, orgID, size)
		if err != nil {
			return err
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if updated != 1 {
			return errors.New("attachment usage charge was not updated")
		}
		return nil
	})
	if err != nil {
		inserted = false
	}
	return digest, inserted, err
}

func (s *Store) GetAttachmentBlobInfo(ctx context.Context, orgID, digest string) (AttachmentBlobInfo, error) {
	var info AttachmentBlobInfo
	err := s.q.QueryRowContext(ctx, `
		SELECT org_id::text, sha256, size_bytes, content_type, ref_count, created_at, last_ref_at
		FROM attachment_blobs
		WHERE org_id = $1 AND sha256 = $2
	`, orgID, digest).Scan(
		&info.OrgID,
		&info.SHA256,
		&info.SizeBytes,
		&info.ContentType,
		&info.RefCount,
		&info.CreatedAt,
		&info.LastRefAt,
	)
	return info, err
}

// LoadAttachmentBlob materializes content and is deliberately separate from
// GetAttachmentBlobInfo so callers can acquire a byte budget first.
func (s *Store) LoadAttachmentBlob(ctx context.Context, orgID, digest string) ([]byte, error) {
	var content []byte
	err := s.q.QueryRowContext(ctx, `
		SELECT content FROM attachment_blobs
		WHERE org_id = $1 AND sha256 = $2
	`, orgID, digest).Scan(&content)
	return content, err
}

// DeleteUnreferencedAttachmentBlobs garbage-collects blobs that have remained
// unreferenced through the supplied grace-period cutoff. Usage is decremented
// in the same statement, while attachment metadata and its durable digest live
// on in the reference tables.
func (s *Store) DeleteUnreferencedAttachmentBlobs(ctx context.Context, lastRefBefore time.Time, limit int) (int, int64, error) {
	if lastRefBefore.IsZero() {
		return 0, 0, errors.New("missing attachment garbage-collection cutoff")
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var deleted int
	var bytesReleased int64
	err := s.q.QueryRowContext(ctx, `
		WITH picked AS (
		  SELECT org_id, sha256
		  FROM attachment_blobs
		  WHERE ref_count = 0
		    AND last_ref_at <= $1
		  ORDER BY last_ref_at, org_id, sha256
		  LIMIT $2
		  FOR UPDATE SKIP LOCKED
		), removed AS (
		  DELETE FROM attachment_blobs blob
		  USING picked
		  WHERE blob.org_id = picked.org_id
		    AND blob.sha256 = picked.sha256
		    AND blob.ref_count = 0
		  RETURNING blob.org_id, blob.size_bytes
		), released_by_org AS (
		  SELECT org_id, sum(size_bytes)::bigint AS size_bytes
		  FROM removed
		  GROUP BY org_id
		), usage_updated AS (
		  UPDATE org_attachment_usage usage
		  SET bytes_used = greatest(usage.bytes_used - released.size_bytes, 0),
		      updated_at = now()
		  FROM released_by_org released
		  WHERE usage.org_id = released.org_id
		  RETURNING usage.org_id
		)
		SELECT count(*), COALESCE(sum(size_bytes), 0)::bigint
		FROM removed
	`, lastRefBefore, limit).Scan(&deleted, &bytesReleased)
	return deleted, bytesReleased, err
}
