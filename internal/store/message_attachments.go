package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const MaxMessageAttachmentAttempts = 6

var ErrAttachmentLeaseLost = errors.New("attachment mirror lease lost")

type MessageAttachmentMetadata struct {
	ProviderAttachmentID string
	Filename             string
	ContentType          string
	ContentDisposition   string
	ContentID            string
	SizeBytes            *int64
}

type MessageAttachment struct {
	ID                   string
	OrgID                string
	MessageID            string
	Ordinal              int
	ProviderAttachmentID string
	ReceivedEmailID      string
	Filename             string
	ContentType          string
	ContentDisposition   string
	ContentID            string
	SizeBytes            sql.NullInt64
	Availability         string
	BlobSHA256           sql.NullString
	AttemptCount         int
	NextAttemptAt        time.Time
	LockedAt             sql.NullTime
	LockedBy             sql.NullString
	LastError            sql.NullString
	MirroredAt           sql.NullTime
	CreatedAt            time.Time
}

// SetMessageAttachmentsKnown persists the provider envelope before declaring
// the message metadata complete. Existing mirrored state and blob references
// survive replay; only immutable provider metadata is refreshed.
func (s *Store) SetMessageAttachmentsKnown(
	ctx context.Context,
	orgID string,
	messageID string,
	attachments []MessageAttachmentMetadata,
) error {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(messageID) == "" {
		return errors.New("missing message attachment owner")
	}
	seen := make(map[string]struct{}, len(attachments))
	for _, attachment := range attachments {
		providerID := strings.TrimSpace(attachment.ProviderAttachmentID)
		if providerID == "" {
			return errors.New("missing provider attachment id")
		}
		if _, duplicate := seen[providerID]; duplicate {
			return errors.New("duplicate provider attachment id")
		}
		seen[providerID] = struct{}{}
		if attachment.SizeBytes != nil && *attachment.SizeBytes < 0 {
			return errors.New("attachment size must not be negative")
		}
	}

	return s.withTx(ctx, func(scoped *Store) error {
		for ordinal, attachment := range attachments {
			contentType := strings.TrimSpace(attachment.ContentType)
			if contentType == "" {
				contentType = "application/octet-stream"
			}
			if _, err := scoped.q.ExecContext(ctx, `
				INSERT INTO message_attachments
				  (org_id, message_id, ordinal, provider_attachment_id, filename,
				   content_type, content_disposition, content_id, size_bytes)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
				ON CONFLICT (message_id, provider_attachment_id) DO UPDATE SET
				  ordinal = EXCLUDED.ordinal,
				  filename = EXCLUDED.filename,
				  content_type = EXCLUDED.content_type,
				  content_disposition = EXCLUDED.content_disposition,
				  content_id = EXCLUDED.content_id,
				  size_bytes = EXCLUDED.size_bytes
			`,
				orgID,
				messageID,
				ordinal,
				strings.TrimSpace(attachment.ProviderAttachmentID),
				attachment.Filename,
				contentType,
				attachment.ContentDisposition,
				attachment.ContentID,
				attachment.SizeBytes,
			); err != nil {
				return err
			}
		}

		result, err := scoped.q.ExecContext(ctx, `
			UPDATE messages SET attachments_state = 'known'
			WHERE id = $1 AND org_id = $2
		`, messageID, orgID)
		if err != nil {
			return err
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if updated != 1 {
			return ErrOwnershipMismatch
		}
		return nil
	})
}

func (s *Store) ListMessageAttachments(ctx context.Context, orgID, messageID string) ([]MessageAttachment, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT attachment.id::text, attachment.org_id::text, attachment.message_id::text, attachment.ordinal,
		       attachment.provider_attachment_id, coalesce(message.received_email_id, ''),
		       attachment.filename, attachment.content_type,
		       attachment.content_disposition, attachment.content_id,
		       attachment.size_bytes, attachment.availability,
		       attachment.blob_sha256, attachment.attempt_count,
		       attachment.next_attempt_at, attachment.locked_at,
		       attachment.locked_by, attachment.last_error,
		       attachment.mirrored_at, attachment.created_at
		FROM message_attachments attachment
		JOIN messages message
		  ON message.org_id = attachment.org_id AND message.id = attachment.message_id
		WHERE attachment.org_id = $1 AND attachment.message_id = $2
		ORDER BY attachment.ordinal, attachment.id
	`, orgID, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	attachments := make([]MessageAttachment, 0)
	for rows.Next() {
		var attachment MessageAttachment
		if err := rows.Scan(
			&attachment.ID,
			&attachment.OrgID,
			&attachment.MessageID,
			&attachment.Ordinal,
			&attachment.ProviderAttachmentID,
			&attachment.ReceivedEmailID,
			&attachment.Filename,
			&attachment.ContentType,
			&attachment.ContentDisposition,
			&attachment.ContentID,
			&attachment.SizeBytes,
			&attachment.Availability,
			&attachment.BlobSHA256,
			&attachment.AttemptCount,
			&attachment.NextAttemptAt,
			&attachment.LockedAt,
			&attachment.LockedBy,
			&attachment.LastError,
			&attachment.MirroredAt,
			&attachment.CreatedAt,
		); err != nil {
			return nil, err
		}
		attachments = append(attachments, attachment)
	}
	return attachments, rows.Err()
}

// ClaimMessageAttachments leases due mirror work. A pending row with an
// expired lock is reclaimable; fresh locks and future retries are left alone.
func (s *Store) ClaimMessageAttachments(
	ctx context.Context,
	limit int,
	workerID string,
	now time.Time,
	staleLockAfter time.Duration,
) ([]MessageAttachment, error) {
	if limit <= 0 {
		limit = 10
	}
	workerID = strings.TrimSpace(workerID)
	if workerID == "" {
		workerID = "attachment-mirror"
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if staleLockAfter <= 0 {
		staleLockAfter = 5 * time.Minute
	}
	staleCutoff := now.Add(-staleLockAfter)

	rows, err := s.q.QueryContext(ctx, `
		WITH exhausted AS (
			UPDATE message_attachments
			SET availability = 'failed',
			    last_error = 'attachment mirror lease expired after retry budget exhausted',
			    locked_at = NULL,
			    locked_by = NULL
			WHERE availability = 'pending'
			  AND attempt_count >= $5
			  AND (locked_at IS NULL OR locked_at <= $4)
			RETURNING id
		), picked AS (
			SELECT attachment.id, message.received_email_id
			FROM message_attachments attachment
			JOIN messages message
			  ON message.org_id = attachment.org_id AND message.id = attachment.message_id
			WHERE attachment.availability = 'pending'
			  AND attachment.attempt_count < $5
			  AND attachment.next_attempt_at <= $1
			  AND (attachment.locked_at IS NULL OR attachment.locked_at <= $4)
			  AND NOT EXISTS (SELECT 1 FROM exhausted WHERE exhausted.id = attachment.id)
			ORDER BY attachment.next_attempt_at, attachment.id
			LIMIT $2
			FOR UPDATE OF attachment SKIP LOCKED
		)
		UPDATE message_attachments attachment
		SET locked_at = $1,
		    locked_by = $3,
		    attempt_count = attachment.attempt_count + 1
		FROM picked
		WHERE attachment.id = picked.id
		RETURNING attachment.id::text, attachment.org_id::text,
		          attachment.message_id::text, attachment.ordinal,
		          attachment.provider_attachment_id, coalesce(picked.received_email_id, ''),
		          attachment.filename,
		          attachment.content_type, attachment.content_disposition,
		          attachment.content_id, attachment.size_bytes,
		          attachment.availability, attachment.blob_sha256,
		          attachment.attempt_count, attachment.next_attempt_at,
		          attachment.locked_at, attachment.locked_by,
		          attachment.last_error, attachment.mirrored_at,
		          attachment.created_at
	`, now, limit, workerID, staleCutoff, MaxMessageAttachmentAttempts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	attachments := make([]MessageAttachment, 0)
	for rows.Next() {
		var attachment MessageAttachment
		if err := rows.Scan(
			&attachment.ID,
			&attachment.OrgID,
			&attachment.MessageID,
			&attachment.Ordinal,
			&attachment.ProviderAttachmentID,
			&attachment.ReceivedEmailID,
			&attachment.Filename,
			&attachment.ContentType,
			&attachment.ContentDisposition,
			&attachment.ContentID,
			&attachment.SizeBytes,
			&attachment.Availability,
			&attachment.BlobSHA256,
			&attachment.AttemptCount,
			&attachment.NextAttemptAt,
			&attachment.LockedAt,
			&attachment.LockedBy,
			&attachment.LastError,
			&attachment.MirroredAt,
			&attachment.CreatedAt,
		); err != nil {
			return nil, err
		}
		attachments = append(attachments, attachment)
	}
	return attachments, rows.Err()
}

// RequeueMessageAttachment records a transient failure. The sixth claimed
// attempt is terminal, matching the mirror worker contract in the rollout.
func (s *Store) RequeueMessageAttachment(
	ctx context.Context,
	id string,
	workerID string,
	leaseAcquiredAt time.Time,
	nextAttemptAt time.Time,
	lastError string,
) (terminal bool, err error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(workerID) == "" || leaseAcquiredAt.IsZero() {
		return false, errors.New("missing attachment mirror lease owner")
	}
	if nextAttemptAt.IsZero() {
		nextAttemptAt = time.Now().UTC()
	}
	var availability string
	err = s.q.QueryRowContext(ctx, `
		UPDATE message_attachments
		SET availability = CASE
		      WHEN attempt_count >= $6 THEN 'failed'
		      ELSE 'pending'
		    END,
		    next_attempt_at = $4,
		    last_error = nullif($5, ''),
		    locked_at = NULL,
		    locked_by = NULL
		WHERE id = $1
		  AND availability = 'pending'
		  AND locked_by = $2
		  AND locked_at = $3
		RETURNING availability
	`, id, workerID, leaseAcquiredAt, nextAttemptAt, lastError, MaxMessageAttachmentAttempts).Scan(&availability)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrAttachmentLeaseLost
	}
	if err != nil {
		return false, err
	}
	return availability == "failed", nil
}

func (s *Store) MarkMessageAttachmentAvailable(
	ctx context.Context,
	id string,
	workerID string,
	leaseAcquiredAt time.Time,
	digest string,
	mirroredAt time.Time,
) error {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(workerID) == "" || leaseAcquiredAt.IsZero() || strings.TrimSpace(digest) == "" {
		return errors.New("missing completed attachment mirror field")
	}
	if mirroredAt.IsZero() {
		mirroredAt = time.Now().UTC()
	}
	return s.updateClaimedMessageAttachment(ctx, id, workerID, leaseAcquiredAt, `
		UPDATE message_attachments
		SET availability = 'available',
		    blob_sha256 = $4,
		    mirrored_at = $5,
		    last_error = NULL,
		    locked_at = NULL,
		    locked_by = NULL
		WHERE id = $1
		  AND availability = 'pending'
		  AND locked_by = $2
		  AND locked_at = $3
	`, digest, mirroredAt)
}

func (s *Store) MarkMessageAttachmentTerminal(
	ctx context.Context,
	id string,
	workerID string,
	leaseAcquiredAt time.Time,
	availability string,
	lastError string,
) error {
	switch availability {
	case "expired", "too_large", "failed":
	default:
		return fmt.Errorf("invalid terminal attachment availability %q", availability)
	}
	if strings.TrimSpace(id) == "" || strings.TrimSpace(workerID) == "" || leaseAcquiredAt.IsZero() {
		return errors.New("missing terminal attachment mirror lease owner")
	}
	return s.updateClaimedMessageAttachment(ctx, id, workerID, leaseAcquiredAt, `
		UPDATE message_attachments
		SET availability = $4,
		    last_error = nullif($5, ''),
		    locked_at = NULL,
		    locked_by = NULL
		WHERE id = $1
		  AND availability = 'pending'
		  AND locked_by = $2
		  AND locked_at = $3
	`, availability, lastError)
}

func (s *Store) updateClaimedMessageAttachment(
	ctx context.Context,
	id string,
	workerID string,
	leaseAcquiredAt time.Time,
	query string,
	args ...any,
) error {
	parameters := []any{id, workerID, leaseAcquiredAt}
	parameters = append(parameters, args...)
	result, err := s.q.ExecContext(ctx, query, parameters...)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return ErrAttachmentLeaseLost
	}
	return nil
}
