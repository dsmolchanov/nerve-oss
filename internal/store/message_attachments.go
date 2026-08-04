package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

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
		SELECT id::text, org_id::text, message_id::text, ordinal,
		       provider_attachment_id, filename, content_type,
		       content_disposition, content_id, size_bytes, availability,
		       blob_sha256, attempt_count, next_attempt_at, locked_at,
		       locked_by, last_error, mirrored_at, created_at
		FROM message_attachments
		WHERE org_id = $1 AND message_id = $2
		ORDER BY ordinal, id
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
