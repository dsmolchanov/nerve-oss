package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// OutboxMessageAttachmentBytes returns the metadata-declared bytes that will
// be materialized for one delivery. Callers use it to reserve aggregate memory
// before LoadOutboxMessageAttachments executes its SELECT of blob content.
func (s *Store) OutboxMessageAttachmentBytes(ctx context.Context, orgID, outboxMessageID string) (int64, error) {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(outboxMessageID) == "" {
		return 0, errors.New("missing attachment org_id or outbox_message_id")
	}
	var size int64
	err := s.q.QueryRowContext(ctx, `
		SELECT COALESCE(sum(size_bytes), 0)::bigint
		FROM outbox_attachments
		WHERE org_id = $1 AND outbox_message_id = $2
	`, orgID, outboxMessageID).Scan(&size)
	return size, err
}

// LoadOutboxMessageAttachments materializes one outbox message's attachment
// bytes immediately before delivery. Metadata remains readable after release,
// but delivery must fail rather than silently omit released bytes.
func (s *Store) LoadOutboxMessageAttachments(ctx context.Context, orgID, outboxMessageID string) ([]OutboundAttachment, error) {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(outboxMessageID) == "" {
		return nil, errors.New("missing attachment org_id or outbox_message_id")
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT attachment.filename,
		       attachment.content_type,
		       attachment.sha256,
		       attachment.blob_sha256,
		       blob.content
		FROM outbox_attachments attachment
		LEFT JOIN attachment_blobs blob
		  ON blob.org_id = attachment.org_id
		 AND blob.sha256 = attachment.blob_sha256
		WHERE attachment.org_id = $1
		  AND attachment.outbox_message_id = $2
		ORDER BY attachment.ordinal
	`, orgID, outboxMessageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var attachments []OutboundAttachment
	var released []string
	for rows.Next() {
		var attachment OutboundAttachment
		var blobSHA sql.NullString
		if err := rows.Scan(
			&attachment.Filename,
			&attachment.ContentType,
			&attachment.SHA256,
			&blobSHA,
			&attachment.Content,
		); err != nil {
			return nil, err
		}
		if !blobSHA.Valid {
			released = append(released, attachment.SHA256)
			continue
		}
		if len(attachment.Content) == 0 {
			return nil, fmt.Errorf("outbox attachment blob %s is missing", blobSHA.String)
		}
		attachments = append(attachments, attachment)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(released) != 0 {
		return nil, fmt.Errorf("%w: digests=%s", ErrAttachmentsReleased, strings.Join(released, ","))
	}
	return attachments, nil
}
