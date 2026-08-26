package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

const (
	// MaxOutboxRetries is the maximum number of delivery attempts before giving up.
	MaxOutboxRetries = 5

	maxOutboundAttachmentCount         = 10
	maxOutboundAttachmentBytes         = 10 << 20
	maxOutboundAttachmentTotalBytes    = 10 << 20
	maxOutboundAttachmentFilenameBytes = 255
)

// ErrDomainNotVerified is returned by EnqueueOutboxMessage when the
// `From` address's domain exists in org_domains but is not in a
// sendable status (i.e. not `active` or `verified_dns`). Callers
// should surface this as a 4xx to the end user rather than enqueueing
// a message that would fail every retry.
var ErrDomainNotVerified = errors.New("domain not verified")

var (
	ErrOutboxIdempotencyConflict = errors.New("outbox idempotency conflict")
	ErrOutboxNotFailed           = errors.New("outbox message is not failed")
	ErrAttachmentCountExceeded   = errors.New("attachment count exceeded")
	ErrAttachmentTooLarge        = errors.New("attachment too large")
	ErrAttachmentEmpty           = errors.New("attachment empty")
	ErrAttachmentTotalTooLarge   = errors.New("attachment total too large")
	ErrAttachmentInvalidFilename = errors.New("attachment invalid filename")
	ErrAttachmentTypeNotAllowed  = errors.New("attachment type not allowed")
	ErrAttachmentsReleased       = errors.New("outbox attachments released")
	ErrOutboxClaimLost           = errors.New("outbox claim lost")
	ErrOutboxPolicyRevoked       = errors.New("outbox policy revoked")
)

var allowedOutboundAttachmentContentTypes = map[string]struct{}{
	"image/png":       {},
	"image/jpeg":      {},
	"image/webp":      {},
	"application/pdf": {},
	"text/plain":      {},
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": {},
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":       {},
}

// OutboundAttachment is the shared provider-facing attachment shape. Content
// is populated only immediately before delivery and is never stored on the
// outbox queue row itself.
type OutboundAttachment struct {
	Filename    string
	ContentType string
	SHA256      string
	Content     []byte
}

type OutboxMessage struct {
	ID                string
	OrgID             string
	InboxID           string
	Provider          string
	ProviderMessageID sql.NullString
	IdempotencyKey    string
	To                string
	From              string
	Subject           string
	TextBody          string
	HTMLBody          string
	ContentHash       string
	Attachments       []OutboundAttachment
	AutonomousLimits  *OutboundLimitInput
	// AutonomousPolicyEpoch is populated by the store from the locked
	// org_outbound_policy_state row. Callers may not select it.
	AutonomousPolicyEpoch int64
	ProviderStartedAt     sql.NullTime
	ProviderOperationID   sql.NullString
	ProviderResolvedAt    sql.NullTime
	// AllowLegacyIdempotencyReplay permits a raw-key lookup during the
	// tool-scoped outbox-key rollout. Callers set it only after recovering an
	// existing failed/stale tool-idempotency record for this same tool.
	AllowLegacyIdempotencyReplay bool

	// Threading headers for reply-chain continuity (RFC 5322).
	// Set when replying to an inbound message so the recipient's
	// email client threads the reply correctly.
	InReplyToMessageID string // Internet-Message-ID of the message being replied to
	References         string // Space-separated list of ancestor message IDs

	Status                string
	DeliveryStatus        string
	DeliveryStatusAt      sql.NullTime
	AttemptCount          int
	NextAttemptAt         time.Time
	LastAttemptAt         sql.NullTime
	LastError             sql.NullString
	LockedAt              sql.NullTime
	LockedBy              sql.NullString
	CreatedAt             time.Time
	TerminalAt            sql.NullTime
	AttachmentsReleasedAt sql.NullTime
	AttachmentsAvailable  bool
}

// OutboxEvent represents a delivery event in the append-only timeline.
//
// OutboxMessageID is the durable foreign key into outbox_messages and is
// the preferred linkage. ProviderMessageID is optional and only set after
// the provider has accepted the message and returned an ID; pre-provider
// events (suppression at enqueue, pre-flight rejection) leave it empty.
//
// At least one of OutboxMessageID or ProviderMessageID must be set.
// InsertOutboxEvent prefers the direct path when OutboxMessageID is
// non-empty and falls back to a join via provider_message_id otherwise
// (the legacy webhook callback path).
type OutboxEvent struct {
	OrgID             string
	OutboxMessageID   string
	ProviderMessageID string
	EventType         string
	RawPayload        json.RawMessage
	Reason            string
}

// Suppression is a per-org block on a recipient address.
type Suppression struct {
	OrgID     string
	Email     string
	Reason    string
	Source    string // "bounce", "complaint", "manual"
	CreatedAt time.Time
}

// OutboxEventRecord is a persisted row from outbox_events. Distinct
// from OutboxEvent (the insert-time input shape) because reads return
// the row id, creation timestamp, and stored provider_message_id.
type OutboxEventRecord struct {
	ID                string
	OrgID             string
	OutboxMessageID   string
	ProviderMessageID string
	EventType         string
	RawPayload        json.RawMessage
	Reason            string
	CreatedAt         time.Time
}

func contentHash(to, subject, textBody, htmlBody string, attachments []OutboundAttachment) string {
	h := sha256.New()
	h.Write([]byte(to))
	h.Write([]byte{0})
	h.Write([]byte(subject))
	h.Write([]byte{0})
	h.Write([]byte(textBody))
	h.Write([]byte{0})
	h.Write([]byte(htmlBody))
	for ordinal, attachment := range attachments {
		h.Write([]byte{0})
		h.Write([]byte(strconv.Itoa(ordinal)))
		h.Write([]byte{0})
		h.Write([]byte(attachment.Filename))
		h.Write([]byte{0})
		h.Write([]byte(attachment.ContentType))
		h.Write([]byte{0})
		h.Write([]byte(attachment.SHA256))
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func normalizeOutboundAttachments(attachments []OutboundAttachment) ([]OutboundAttachment, error) {
	if len(attachments) > maxOutboundAttachmentCount {
		return nil, fmt.Errorf("%w: count=%d max=%d", ErrAttachmentCountExceeded, len(attachments), maxOutboundAttachmentCount)
	}
	normalized := make([]OutboundAttachment, len(attachments))
	totalBytes := 0
	for ordinal, attachment := range attachments {
		attachment.Filename = strings.TrimSpace(attachment.Filename)
		if invalidOutboundAttachmentFilename(attachment.Filename) {
			return nil, fmt.Errorf("%w: ordinal=%d", ErrAttachmentInvalidFilename, ordinal)
		}
		attachment.ContentType = strings.ToLower(strings.TrimSpace(attachment.ContentType))
		if _, allowed := allowedOutboundAttachmentContentTypes[attachment.ContentType]; !allowed {
			return nil, fmt.Errorf("%w: ordinal=%d", ErrAttachmentTypeNotAllowed, ordinal)
		}
		if len(attachment.Content) == 0 {
			return nil, fmt.Errorf("%w: ordinal=%d", ErrAttachmentEmpty, ordinal)
		}
		if len(attachment.Content) > maxOutboundAttachmentBytes {
			return nil, fmt.Errorf("%w: ordinal=%d size=%d max=%d", ErrAttachmentTooLarge, ordinal, len(attachment.Content), maxOutboundAttachmentBytes)
		}
		totalBytes += len(attachment.Content)
		if totalBytes > maxOutboundAttachmentTotalBytes {
			return nil, fmt.Errorf("%w: ordinal=%d total=%d max=%d", ErrAttachmentTotalTooLarge, ordinal, totalBytes, maxOutboundAttachmentTotalBytes)
		}
		sum := sha256.Sum256(attachment.Content)
		attachment.SHA256 = hex.EncodeToString(sum[:])
		normalized[ordinal] = attachment
	}
	return normalized, nil
}

func invalidOutboundAttachmentFilename(filename string) bool {
	if filename == "" || len([]byte(filename)) > maxOutboundAttachmentFilenameBytes {
		return true
	}
	for _, character := range filename {
		if character == '/' || character == '\\' || character == 0 || unicode.IsControl(character) {
			return true
		}
	}
	return false
}

func (s *Store) EnqueueOutboxMessage(ctx context.Context, msg OutboxMessage) (string, error) {
	return s.enqueueOutboxMessage(ctx, msg, nil)
}

// enqueueOutboxMessage's afterConflict hook exists only to make the
// conflict-to-terminal race deterministic in tests. Production callers always
// pass nil through EnqueueOutboxMessage.
func (s *Store) enqueueOutboxMessage(ctx context.Context, msg OutboxMessage, afterConflict func() error) (string, error) {
	if msg.OrgID == "" || msg.InboxID == "" {
		return "", errors.New("missing org_id or inbox_id")
	}
	if msg.Provider == "" {
		return "", errors.New("missing provider")
	}
	if msg.IdempotencyKey == "" {
		return "", errors.New("missing idempotency_key")
	}
	if msg.AutonomousPolicyEpoch != 0 || msg.ProviderStartedAt.Valid || msg.ProviderOperationID.Valid || msg.ProviderResolvedAt.Valid {
		return "", errors.New("caller cannot select outbox policy fence state")
	}
	legacyIdempotencyKey := ""
	if msg.AutonomousLimits != nil {
		if msg.AutonomousLimits.ToolName == "" || msg.AutonomousLimits.IdempotencyKey == "" {
			return "", errors.New("missing autonomous tool idempotency identity")
		}
		if msg.AutonomousLimits.IdempotencyKey != msg.IdempotencyKey {
			return "", errors.New("outbox and autonomous idempotency keys differ")
		}
		if msg.AllowLegacyIdempotencyReplay {
			legacyIdempotencyKey = msg.IdempotencyKey
		}
		msg.IdempotencyKey = OutboundIdempotencyKey(
			msg.AutonomousLimits.ToolName, msg.AutonomousLimits.IdempotencyKey,
		)
	}
	if msg.To == "" || msg.From == "" {
		return "", errors.New("missing to/from")
	}
	if msg.Subject == "" {
		return "", errors.New("missing subject")
	}
	attachments, err := normalizeOutboundAttachments(msg.Attachments)
	if err != nil {
		return "", err
	}
	msg.Attachments = attachments
	id := msg.ID
	if id == "" {
		id = uuid.NewString()
	}

	// B4: pre-flight domain verification. If the `From` address uses a
	// domain the org has claimed in org_domains but hasn't verified yet,
	// reject synchronously with a typed error rather than enqueueing a
	// row that would fail every retry. Domains NOT in org_domains for
	// this org are allowed (preserves dev/legacy local paths); we only
	// enforce on explicitly-claimed-but-unverified domains.
	if err := s.checkSendableDomain(ctx, msg.OrgID, msg.From); err != nil {
		return "", err
	}

	// B2: short-circuit suppressed recipients. The row is still inserted
	// (preserving the idempotency contract — callers always get a handle
	// back) but lands directly in failed/suppressed state without ever
	// hitting the provider.
	suppressed, suppressReason, err := s.IsSuppressed(ctx, msg.OrgID, msg.To)
	if err != nil {
		return "", fmt.Errorf("check suppression: %w", err)
	}

	hash := contentHash(msg.To, msg.Subject, msg.TextBody, msg.HTMLBody, msg.Attachments)
	var outID string
	err = s.withTx(ctx, func(scoped *Store) error {
		if msg.AutonomousLimits != nil {
			if err := scoped.LockOrgPolicy(ctx, msg.OrgID); err != nil {
				return err
			}
			epoch, err := scoped.CurrentOutboundPolicyEpoch(ctx, msg.OrgID)
			if err != nil {
				return err
			}
			allowed, err := scoped.outboundPolicyFlagsAllowSend(ctx, msg.OrgID)
			if err != nil {
				return err
			}
			if !allowed {
				return ErrOutboxPolicyRevoked
			}
			msg.AutonomousPolicyEpoch = epoch
		}
		resolvedID, inserted, resolveErr := scoped.resolveOrInsertOutboxParent(
			ctx, id, msg, hash, legacyIdempotencyKey, suppressed, suppressReason, afterConflict,
		)
		if resolveErr != nil {
			return resolveErr
		}
		outID = resolvedID
		if !inserted {
			return nil
		}
		if msg.AutonomousLimits != nil {
			limits := *msg.AutonomousLimits
			if limits.Recipient == "" {
				limits.Recipient = msg.To
			}
			if err := scoped.ReserveOutboundLimits(ctx, msg.OrgID, outID, limits); err != nil {
				return err
			}
		}

		for ordinal, attachment := range msg.Attachments {
			digest, _, storeErr := scoped.StoreAttachmentBlob(ctx, msg.OrgID, attachment.ContentType, attachment.Content)
			if storeErr != nil {
				return fmt.Errorf("store outbox attachment ordinal=%d: %w", ordinal, storeErr)
			}
			if digest != attachment.SHA256 {
				return fmt.Errorf("outbox attachment ordinal=%d digest mismatch", ordinal)
			}
			if _, storeErr = scoped.q.ExecContext(ctx, `
				INSERT INTO outbox_attachments
				  (org_id, outbox_message_id, ordinal, filename, content_type, size_bytes, sha256, blob_sha256)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
			`, msg.OrgID, outID, ordinal, attachment.Filename, attachment.ContentType,
				len(attachment.Content), digest); storeErr != nil {
				return fmt.Errorf("insert outbox attachment ordinal=%d: %w", ordinal, storeErr)
			}
		}

		if suppressed {
			payload, marshalErr := json.Marshal(map[string]any{
				"event_type":      "suppressed_at_enqueue",
				"reason":          suppressReason,
				"recipient":       msg.To,
				"idempotency_key": msg.IdempotencyKey,
			})
			if marshalErr != nil {
				return marshalErr
			}
			if storeErr := scoped.InsertOutboxEvent(ctx, OutboxEvent{
				OrgID:           msg.OrgID,
				OutboxMessageID: outID,
				EventType:       "suppressed_at_enqueue",
				RawPayload:      payload,
				Reason:          suppressReason,
			}); storeErr != nil {
				return fmt.Errorf("insert suppression event: %w", storeErr)
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return outID, nil
}

func (s *Store) resolveOrInsertOutboxParent(
	ctx context.Context,
	id string,
	msg OutboxMessage,
	hash string,
	legacyIdempotencyKey string,
	suppressed bool,
	suppressReason string,
	afterConflict func() error,
) (outID string, inserted bool, err error) {
	status := "queued"
	deliveryStatus := "unknown"
	lastError := ""
	if suppressed {
		status = "failed"
		deliveryStatus = "suppressed"
		lastError = fmt.Sprintf("suppressed:%s", suppressReason)
	}
	var scopedHash sql.NullString
	row := s.q.QueryRowContext(ctx, `
		SELECT id::text, content_hash
		FROM outbox_messages
		WHERE org_id = $1 AND idempotency_key = $2
		LIMIT 1
	`, msg.OrgID, msg.IdempotencyKey)
	if scanErr := row.Scan(&outID, &scopedHash); scanErr == nil {
		if scopedHash.Valid && scopedHash.String != hash {
			return "", false, fmt.Errorf("%w: key=%q", ErrOutboxIdempotencyConflict, msg.IdempotencyKey)
		}
		return outID, false, nil
	} else if !errors.Is(scanErr, sql.ErrNoRows) {
		return "", false, scanErr
	}
	if legacyIdempotencyKey != "" {
		var storedHash sql.NullString
		row = s.q.QueryRowContext(ctx, `
			SELECT id::text, content_hash
			FROM outbox_messages
			WHERE org_id = $1 AND idempotency_key = $2
			LIMIT 1
		`, msg.OrgID, legacyIdempotencyKey)
		if scanErr := row.Scan(&outID, &storedHash); scanErr == nil {
			if storedHash.Valid && storedHash.String != hash {
				return "", false, fmt.Errorf("%w: key=%q", ErrOutboxIdempotencyConflict, legacyIdempotencyKey)
			}
			return outID, false, nil
		} else if !errors.Is(scanErr, sql.ErrNoRows) {
			return "", false, scanErr
		}
	}

	const maxConflictRetries = 3
	for attempt := 0; ; attempt++ {
		// The fence column and its parameter are last so the pre-fence variant
		// drops both without renumbering anything ahead of them.
		fence := s.resolveOutboundFence(ctx)
		row := s.q.QueryRowContext(ctx, adaptOutboxSQL(fence, `
			INSERT INTO outbox_messages (
				id, org_id, inbox_id, provider, idempotency_key,
				"to", "from", subject, text_body, html_body,
				content_hash, in_reply_to_message_id, "references",
				status, delivery_status, delivery_status_at, last_error,
				last_attempt_at, terminal_at,
				autonomous_policy_epoch
			)
			VALUES (
				$1, $2, $3, $4, $5,
				$6, $7, $8, nullif($9, ''), nullif($10, ''),
				$11, nullif($12, ''), nullif($13, ''),
				$14, $15,
				CASE WHEN $16 THEN now() END, nullif($17, ''),
				CASE WHEN $16 THEN now() END, CASE WHEN $16 THEN now() END,
				nullif($18, 0)
			)
			ON CONFLICT DO NOTHING
			RETURNING id::text
		`), trimOutboxArgs(fence, id, msg.OrgID, msg.InboxID, msg.Provider, msg.IdempotencyKey,
			msg.To, msg.From, msg.Subject, msg.TextBody, msg.HTMLBody, hash,
			msg.InReplyToMessageID, msg.References, status, deliveryStatus,
			suppressed, lastError, msg.AutonomousPolicyEpoch)...)
		if scanErr := row.Scan(&outID); scanErr == nil {
			return outID, true, nil
		} else if !errors.Is(scanErr, sql.ErrNoRows) {
			return "", false, s.noteOutboxSchemaError(ctx, scanErr)
		}

		if afterConflict != nil {
			if hookErr := afterConflict(); hookErr != nil {
				return "", false, fmt.Errorf("after outbox conflict: %w", hookErr)
			}
			afterConflict = nil
		}

		var storedHash sql.NullString
		var storedKey string
		row = s.q.QueryRowContext(ctx, `
			SELECT id::text, content_hash, idempotency_key
			FROM outbox_messages
			WHERE org_id = $1
			  AND (
				idempotency_key = $2
				OR (nullif($5, '') IS NOT NULL AND idempotency_key = $5)
				OR (inbox_id = $3 AND content_hash = $4 AND status IN ('queued', 'sending'))
			  )
			ORDER BY
			  CASE WHEN idempotency_key = $2 THEN 0 ELSE 1 END,
			  CASE WHEN idempotency_key = $5 THEN 0 ELSE 1 END,
			  CASE WHEN status IN ('queued', 'sending') THEN 0 ELSE 1 END,
			  created_at DESC,
			  id DESC
			LIMIT 1
		`, msg.OrgID, msg.IdempotencyKey, msg.InboxID, hash, legacyIdempotencyKey)
		if scanErr := row.Scan(&outID, &storedHash, &storedKey); scanErr == nil {
			if (storedKey == msg.IdempotencyKey || storedKey == legacyIdempotencyKey) &&
				storedHash.Valid && storedHash.String != hash {
				return "", false, fmt.Errorf("%w: key=%q", ErrOutboxIdempotencyConflict, msg.IdempotencyKey)
			}
			return outID, false, nil
		} else if !errors.Is(scanErr, sql.ErrNoRows) {
			return "", false, scanErr
		}

		if attempt >= maxConflictRetries {
			return "", false, fmt.Errorf("enqueue outbox message: unresolved conflict after %d retries", maxConflictRetries)
		}
	}
}

func (s *Store) ClaimOutboxMessages(ctx context.Context, limit int, workerID string, now time.Time, staleLockAfter time.Duration) ([]OutboxMessage, error) {
	if limit <= 0 {
		limit = 10
	}
	if workerID == "" {
		workerID = "worker"
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if staleLockAfter <= 0 {
		staleLockAfter = 5 * time.Minute
	}
	staleCutoff := now.Add(-staleLockAfter)
	// locked_by is a lease identity, not a process role. A fresh UUID on every
	// claim call prevents two Machines configured with the same worker label—or
	// one process after a stale reclaim—from passing each other's outcome CAS.
	claimLeaseID := workerID + ":" + uuid.NewString()

	fence := s.resolveOutboundFence(ctx)
	rows, err := s.q.QueryContext(ctx, adaptOutboxSQL(fence, `
		WITH picked AS (
			SELECT outbox.id
			FROM outbox_messages outbox
			WHERE (
				(outbox.status = 'queued' AND outbox.next_attempt_at <= $1)
				OR
				(outbox.status = 'sending' AND outbox.locked_at <= $4)
			)
			AND (
				outbox.autonomous_policy_epoch IS NULL
				OR (outbox.provider_started_at IS NOT NULL AND outbox.provider_resolved_at IS NULL)
				OR (
					EXISTS (
						SELECT 1
						FROM org_outbound_policy_state policy_state
						WHERE policy_state.org_id = outbox.org_id
						  AND policy_state.policy_epoch = outbox.autonomous_policy_epoch
					)
					AND EXISTS (
						SELECT 1 FROM org_feature_flags enabled
						WHERE enabled.org_id = outbox.org_id
						  AND enabled.flag = 'autonomous_outbound_policy'
						  AND enabled.enabled
					)
					AND EXISTS (
						SELECT 1 FROM org_feature_flags suspended
						WHERE suspended.org_id = outbox.org_id
						  AND suspended.flag = 'email_outbound_suspended'
						  AND NOT suspended.enabled
					)
				)
			)
			AND NOT EXISTS (
				SELECT 1
				FROM outbox_delivery_holds hold
				WHERE hold.org_id = outbox.org_id
				  AND hold.idempotency_key = outbox.idempotency_key
				  AND hold.released_at IS NULL
				  AND hold.expires_at > $1
			)
			ORDER BY outbox.next_attempt_at ASC
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		UPDATE outbox_messages o
		SET status = 'sending',
		    locked_at = $1,
		    locked_by = $3,
		    attempt_count = o.attempt_count + 1,
		    last_attempt_at = $1
		FROM picked
		WHERE o.id = picked.id
		RETURNING o.id, o.org_id::text, o.inbox_id::text, o.provider, o.provider_message_id, o.idempotency_key,
		          o."to", o."from", o.subject, coalesce(o.text_body, ''), coalesce(o.html_body, ''),
		          o.status, o.attempt_count, o.next_attempt_at, o.last_attempt_at, o.last_error, o.locked_at, o.locked_by,
		          coalesce(o.in_reply_to_message_id, ''), coalesce(o."references", ''),
		          coalesce(o.autonomous_policy_epoch, 0), o.provider_started_at, o.provider_operation_id, o.provider_resolved_at
	`), now, limit, claimLeaseID, staleCutoff)
	if err != nil {
		return nil, s.noteOutboxSchemaError(ctx, err)
	}
	defer rows.Close()

	var out []OutboxMessage
	for rows.Next() {
		var msg OutboxMessage
		if err := rows.Scan(
			&msg.ID,
			&msg.OrgID,
			&msg.InboxID,
			&msg.Provider,
			&msg.ProviderMessageID,
			&msg.IdempotencyKey,
			&msg.To,
			&msg.From,
			&msg.Subject,
			&msg.TextBody,
			&msg.HTMLBody,
			&msg.Status,
			&msg.AttemptCount,
			&msg.NextAttemptAt,
			&msg.LastAttemptAt,
			&msg.LastError,
			&msg.LockedAt,
			&msg.LockedBy,
			&msg.InReplyToMessageID,
			&msg.References,
			&msg.AutonomousPolicyEpoch,
			&msg.ProviderStartedAt,
			&msg.ProviderOperationID,
			&msg.ProviderResolvedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, msg)
	}
	return out, rows.Err()
}

// BeginOutboxProviderOperation is the autonomous send linearization point.
// It must run immediately before the network call. A first start rechecks the
// live epoch and fail-closed flags under the org policy lock; a previously
// started unresolved operation may be replayed with the same identity even
// after suspension so its unknown outcome can converge.
type OutboxProviderOperation struct {
	ID        string
	StartedAt time.Time
}

func (s *Store) BeginOutboxProviderOperation(ctx context.Context, msg OutboxMessage) (string, error) {
	operation, err := s.BeginOutboxProviderOperationState(ctx, msg)
	return operation.ID, err
}

// BeginOutboxProviderOperationState is BeginOutboxProviderOperation plus the
// persisted database start time needed to enforce bounded provider replay.
func (s *Store) BeginOutboxProviderOperationState(ctx context.Context, msg OutboxMessage) (OutboxProviderOperation, error) {
	if msg.ID == "" || msg.OrgID == "" || !msg.LockedBy.Valid || msg.LockedBy.String == "" {
		return OutboxProviderOperation{}, errors.New("missing claimed outbox identity")
	}
	// The legacy fast path must come first. The worker calls this for every
	// claimed message, and on Core 28 every row is legacy, so guarding ahead of
	// this return would refuse each one and requeue it without ever reaching the
	// provider -- Artifact B could not deliver mail on the Core 28 half of its
	// window. Only a genuinely fenced row needs the fence.
	if msg.AutonomousPolicyEpoch <= 0 {
		return OutboxProviderOperation{}, nil
	}
	if err := s.requireOutboundFence("BeginOutboxProviderOperationState"); err != nil {
		return OutboxProviderOperation{}, err
	}
	operationID := "outbox:" + msg.ID
	var operationStartedAt time.Time
	policyRevoked := false
	err := s.FenceOrgPolicy(ctx, msg.OrgID, func(scoped *Store) error {
		var (
			status     string
			lockedBy   sql.NullString
			lastError  sql.NullString
			savedEpoch sql.NullInt64
			startedAt  sql.NullTime
			storedOpID sql.NullString
			resolvedAt sql.NullTime
		)
		if err := scoped.q.QueryRowContext(ctx, `
			SELECT status, locked_by, last_error, autonomous_policy_epoch,
			       provider_started_at, provider_operation_id, provider_resolved_at
			FROM outbox_messages
			WHERE id = $1 AND org_id = $2::uuid
			FOR UPDATE
		`, msg.ID, msg.OrgID).Scan(
			&status, &lockedBy, &lastError, &savedEpoch, &startedAt, &storedOpID, &resolvedAt,
		); err != nil {
			return err
		}
		if status == "failed" && lastError.Valid && lastError.String == "policy_revoked" &&
			savedEpoch.Valid && savedEpoch.Int64 == msg.AutonomousPolicyEpoch {
			// A policy writer may have terminalized this claimed-but-not-started
			// row after the worker loaded it. Preserve the more specific outcome.
			policyRevoked = true
			return nil
		}
		if status != "sending" || !lockedBy.Valid || lockedBy.String != msg.LockedBy.String ||
			!savedEpoch.Valid || savedEpoch.Int64 != msg.AutonomousPolicyEpoch {
			return ErrOutboxClaimLost
		}
		if storedOpID.Valid && storedOpID.String != operationID {
			return fmt.Errorf("outbox provider operation identity mismatch")
		}
		if startedAt.Valid && !resolvedAt.Valid {
			// The logical provider operation may already have happened. Repeating
			// only with the same provider idempotency key is recovery, not a new
			// authorization decision.
			operationStartedAt = startedAt.Time
			return nil
		}

		currentEpoch, err := scoped.CurrentOutboundPolicyEpoch(ctx, msg.OrgID)
		if err != nil {
			return err
		}
		allowed, err := scoped.outboundPolicyFlagsAllowSend(ctx, msg.OrgID)
		if err != nil {
			return err
		}
		if currentEpoch != msg.AutonomousPolicyEpoch || !allowed {
			result, err := scoped.q.ExecContext(ctx, `
				UPDATE outbox_messages
				SET status = 'failed',
				    last_error = 'policy_revoked',
				    locked_at = NULL,
				    locked_by = NULL,
				    terminal_at = now()
				WHERE id = $1 AND status = 'sending' AND locked_by = $2
			`, msg.ID, msg.LockedBy.String)
			if err != nil {
				return err
			}
			rows, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if rows != 1 {
				return ErrOutboxClaimLost
			}
			// Returning the sentinel from the transaction callback would roll
			// back the terminalization. Commit first, then report revocation.
			policyRevoked = true
			return nil
		}
		err = scoped.q.QueryRowContext(ctx, `
			UPDATE outbox_messages
			SET provider_started_at = coalesce(provider_started_at, now()),
			    provider_operation_id = $2,
			    provider_resolved_at = NULL
			WHERE id = $1 AND status = 'sending' AND locked_by = $3
			RETURNING provider_started_at
		`, msg.ID, operationID, msg.LockedBy.String).Scan(&operationStartedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrOutboxClaimLost
		}
		return err
	})
	if err != nil {
		return OutboxProviderOperation{}, err
	}
	if policyRevoked {
		return OutboxProviderOperation{}, ErrOutboxPolicyRevoked
	}
	return OutboxProviderOperation{ID: operationID, StartedAt: operationStartedAt}, nil
}

func (s *Store) outboundPolicyFlagsAllowSend(ctx context.Context, orgID string) (bool, error) {
	required := []struct {
		flag string
		want bool
	}{
		{flag: "autonomous_outbound_policy", want: true},
		{flag: "email_outbound_suspended", want: false},
	}
	for _, item := range required {
		values, err := s.LookupFeatureFlag(ctx, orgID, item.flag)
		if err != nil {
			return false, err
		}
		if values.Org == nil || *values.Org != item.want {
			return false, nil
		}
	}
	return true, nil
}

// ResolveOutboxProviderAttempt records that the current logical attempt has a
// known non-success outcome. A later retry keeps the same operation identity
// and clears the resolution only at its next provider-start CAS.
func (s *Store) ResolveOutboxProviderAttempt(ctx context.Context, id, workerID, operationID string) error {
	if err := s.requireOutboundFence("ResolveOutboxProviderAttempt"); err != nil {
		return err
	}
	if id == "" || workerID == "" || operationID == "" {
		return errors.New("missing outbox provider attempt identity")
	}
	result, err := s.q.ExecContext(ctx, `
		UPDATE outbox_messages
		SET provider_resolved_at = now()
		WHERE id = $1
		  AND status = 'sending'
		  AND locked_by = $2
		  AND provider_started_at IS NOT NULL
		  AND provider_operation_id = $3
	`, id, workerID, operationID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrOutboxClaimLost
	}
	return nil
}

// CountOutboxByState returns the number of outbox rows currently in the
// given state. Used by the queue-depth metric exporter; cheap because
// state is indexed.
func (s *Store) CountOutboxByState(ctx context.Context, state string) (int64, error) {
	var count int64
	row := s.q.QueryRowContext(ctx, `
		SELECT count(*) FROM outbox_messages WHERE status = $1
	`, state)
	if err := row.Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) MarkOutboxMessageSent(ctx context.Context, id string, providerMessageID string) error {
	if id == "" {
		return errors.New("missing id")
	}
	fence := s.resolveOutboundFence(ctx)
	_, err := s.q.ExecContext(ctx, adaptOutboxSQL(fence, `
		UPDATE outbox_messages
		SET status = 'sent',
		    provider_message_id = nullif($2, ''),
		    last_error = null,
		    locked_at = null,
		    locked_by = null,
		    terminal_at = now(),
		    provider_resolved_at = CASE WHEN provider_started_at IS NOT NULL THEN now() ELSE provider_resolved_at END
		WHERE id = $1
	`), id, providerMessageID)
	return s.noteOutboxSchemaError(ctx, err)
}

func (s *Store) MarkClaimedOutboxMessageSent(ctx context.Context, id, workerID, operationID, providerMessageID string) error {
	return s.finishClaimedOutbox(ctx, id, workerID, operationID, "sent", "", providerMessageID, true)
}

// MigrateOutboxProviderToResend switches unsent outbox messages from smtp to resend
// and resets them for immediate retry. Called at startup when Resend is the configured provider.
func (s *Store) MigrateOutboxProviderToResend(ctx context.Context) (int64, error) {
	result, err := s.q.ExecContext(ctx, `
		UPDATE outbox_messages
		SET provider = 'resend',
		    status = 'queued',
		    next_attempt_at = now(),
		    locked_at = null,
		    locked_by = null
		WHERE provider = 'smtp'
		  AND status IN ('queued', 'sending')
	`)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// MarkOutboxMessageFailed permanently marks a message as failed after exhausting retries.
func (s *Store) MarkOutboxMessageFailed(ctx context.Context, id string, lastError string) error {
	if id == "" {
		return errors.New("missing id")
	}
	fence := s.resolveOutboundFence(ctx)
	_, err := s.q.ExecContext(ctx, adaptOutboxSQL(fence, `
		UPDATE outbox_messages
		SET status = 'failed',
		    last_error = nullif($2, ''),
		    locked_at = null,
		    locked_by = null,
		    terminal_at = now()
		WHERE id = $1
	`), id, lastError)
	return s.noteOutboxSchemaError(ctx, err)
}

// MarkClaimedOutboxMessageFailed terminalizes a pre-provider failure only
// while the caller still owns the exact claim lease.
func (s *Store) MarkClaimedOutboxMessageFailed(ctx context.Context, id, claimLeaseID, lastError string) error {
	return s.finishClaimedOutbox(ctx, id, claimLeaseID, "", "failed", lastError, "", false)
}

// MarkOutboxProviderFailure records a provider-confirmed permanent failure.
// Unlike pre-provider failures, a started operation becomes durably resolved.
func (s *Store) MarkOutboxProviderFailure(ctx context.Context, id string, lastError string) error {
	if id == "" {
		return errors.New("missing id")
	}
	fence := s.resolveOutboundFence(ctx)
	_, err := s.q.ExecContext(ctx, adaptOutboxSQL(fence, `
		UPDATE outbox_messages
		SET status = 'failed',
		    last_error = nullif($2, ''),
		    locked_at = null,
		    locked_by = null,
		    terminal_at = now(),
		    provider_resolved_at = CASE WHEN provider_started_at IS NOT NULL THEN now() ELSE provider_resolved_at END
		WHERE id = $1
	`), id, lastError)
	return s.noteOutboxSchemaError(ctx, err)
}

func (s *Store) MarkClaimedOutboxProviderFailure(ctx context.Context, id, workerID, operationID, lastError string) error {
	return s.finishClaimedOutbox(ctx, id, workerID, operationID, "failed", lastError, "", true)
}

// QuarantineClaimedOutboxUnknown preserves a claimable unresolved operation
// for a provider that cannot safely replay/read back an ambiguous result. The
// worker can periodically re-evaluate provider capability, while the durable
// unresolved fence keeps lifecycle cleanup nonterminal.
func (s *Store) QuarantineClaimedOutboxUnknown(ctx context.Context, id, workerID, operationID, lastError string) error {
	if err := s.requireOutboundFence("QuarantineClaimedOutboxUnknown"); err != nil {
		return err
	}
	if id == "" || workerID == "" || operationID == "" {
		return errors.New("missing quarantined outbox identity")
	}
	result, err := s.q.ExecContext(ctx, `
		UPDATE outbox_messages
		SET status = 'queued',
		    last_error = nullif($4, ''),
		    next_attempt_at = now() + interval '15 minutes',
		    locked_at = NULL,
		    locked_by = NULL,
		    terminal_at = NULL
		WHERE id = $1
		  AND status = 'sending'
		  AND locked_by = $2
		  AND provider_operation_id = $3
		  AND provider_started_at IS NOT NULL
		  AND provider_resolved_at IS NULL
	`, id, workerID, operationID, lastError)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrOutboxClaimLost
	}
	return nil
}

func (s *Store) finishClaimedOutbox(ctx context.Context, id, workerID, operationID, status, lastError, providerMessageID string, resolve bool) error {
	if id == "" || workerID == "" {
		return errors.New("missing claimed outbox identity")
	}
	fence := s.resolveOutboundFence(ctx)
	result, err := s.q.ExecContext(ctx, adaptOutboxSQL(fence, `
		UPDATE outbox_messages
		SET status = $4,
		    provider_message_id = nullif($6, ''),
		    last_error = nullif($5, ''),
		    locked_at = NULL,
		    locked_by = NULL,
		    terminal_at = now(),
		    provider_resolved_at = CASE WHEN $7 AND provider_started_at IS NOT NULL THEN now() ELSE provider_resolved_at END
		WHERE id = $1
		  AND status = 'sending'
		  AND locked_by = $2
		  AND (($3 = '' AND provider_operation_id IS NULL) OR provider_operation_id = $3)
	`), trimOutboxArgs(fence, id, workerID, operationID, status, lastError, providerMessageID, resolve)...)
	if err != nil {
		return s.noteOutboxSchemaError(ctx, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrOutboxClaimLost
	}
	return nil
}

func (s *Store) RequeueOutboxMessage(ctx context.Context, id string, nextAttemptAt time.Time, lastError string) error {
	return s.requeueOutboxMessage(ctx, id, "", nextAttemptAt, lastError)
}

func (s *Store) RequeueClaimedOutboxMessage(ctx context.Context, id, workerID string, nextAttemptAt time.Time, lastError string) error {
	if workerID == "" {
		return errors.New("missing outbox worker id")
	}
	return s.requeueOutboxMessage(ctx, id, workerID, nextAttemptAt, lastError)
}

// RequeueClaimedOutboxKnownProviderFailure atomically records a confirmed
// non-success provider outcome and either requeues it on the still-current
// policy epoch or terminalizes it when a suspension/close already advanced
// the epoch. There is no resolved+sending crash gap.
func (s *Store) RequeueClaimedOutboxKnownProviderFailure(ctx context.Context, id, claimLeaseID, operationID string, nextAttemptAt time.Time, lastError string) error {
	if err := s.requireOutboundFence("RequeueClaimedOutboxKnownProviderFailure"); err != nil {
		return err
	}
	if id == "" || claimLeaseID == "" || operationID == "" {
		return errors.New("missing known provider failure identity")
	}
	if nextAttemptAt.IsZero() {
		nextAttemptAt = time.Now().UTC().Add(10 * time.Second)
	}
	return s.withTx(ctx, func(scoped *Store) error {
		var orgID string
		if err := scoped.q.QueryRowContext(ctx, `
			SELECT org_id::text FROM outbox_messages WHERE id = $1
		`, id).Scan(&orgID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrOutboxClaimLost
			}
			return err
		}
		if err := scoped.LockOrgPolicy(ctx, orgID); err != nil {
			return err
		}
		var savedEpoch int64
		if err := scoped.q.QueryRowContext(ctx, `
			SELECT autonomous_policy_epoch
			FROM outbox_messages
			WHERE id = $1
			  AND status = 'sending'
			  AND locked_by = $2
			  AND provider_operation_id = $3
			  AND provider_started_at IS NOT NULL
			  AND provider_resolved_at IS NULL
			FOR UPDATE
		`, id, claimLeaseID, operationID).Scan(&savedEpoch); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrOutboxClaimLost
			}
			return err
		}
		currentEpoch, err := scoped.CurrentOutboundPolicyEpoch(ctx, orgID)
		if err != nil {
			return err
		}
		allowed, err := scoped.outboundPolicyFlagsAllowSend(ctx, orgID)
		if err != nil {
			return err
		}
		if currentEpoch != savedEpoch || !allowed {
			result, err := scoped.q.ExecContext(ctx, `
				UPDATE outbox_messages
				SET status = 'failed',
				    last_error = 'policy_revoked',
				    provider_resolved_at = now(),
				    locked_at = NULL,
				    locked_by = NULL,
				    terminal_at = now()
				WHERE id = $1 AND status = 'sending' AND locked_by = $2
			`, id, claimLeaseID)
			if err != nil {
				return err
			}
			rows, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if rows != 1 {
				return ErrOutboxClaimLost
			}
			return nil
		}
		result, err := scoped.q.ExecContext(ctx, `
			UPDATE outbox_messages
			SET status = 'queued',
			    next_attempt_at = $2,
			    last_error = nullif($3, ''),
			    provider_resolved_at = now(),
			    locked_at = NULL,
			    locked_by = NULL,
			    terminal_at = NULL
			WHERE id = $1 AND status = 'sending' AND locked_by = $4
		`, id, nextAttemptAt, lastError, claimLeaseID)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows != 1 {
			return ErrOutboxClaimLost
		}
		return nil
	})
}

func (s *Store) requeueOutboxMessage(ctx context.Context, id, workerID string, nextAttemptAt time.Time, lastError string) error {
	if id == "" {
		return errors.New("missing id")
	}
	if nextAttemptAt.IsZero() {
		nextAttemptAt = time.Now().UTC().Add(10 * time.Second)
	}
	return s.withTx(ctx, func(scoped *Store) error {
		scopedFence := scoped.resolveOutboundFence(ctx)
		var (
			orgID      string
			savedEpoch sql.NullInt64
		)
		if err := scoped.q.QueryRowContext(ctx, adaptOutboxSQL(scopedFence, `
			SELECT org_id::text, autonomous_policy_epoch
			FROM outbox_messages
			WHERE id = $1
			  AND ($2 = '' OR (status = 'sending' AND locked_by = $2))
		`), id, workerID).Scan(&orgID, &savedEpoch); err != nil {
			if errors.Is(err, sql.ErrNoRows) && workerID != "" {
				return ErrOutboxClaimLost
			}
			return err
		}
		if savedEpoch.Valid {
			if err := scoped.LockOrgPolicy(ctx, orgID); err != nil {
				return err
			}
		}
		var (
			startedAt  sql.NullTime
			resolvedAt sql.NullTime
		)
		if err := scoped.q.QueryRowContext(ctx, adaptOutboxSQL(scopedFence, `
			SELECT autonomous_policy_epoch, provider_started_at, provider_resolved_at
			FROM outbox_messages
			WHERE id = $1
			  AND ($2 = '' OR (status = 'sending' AND locked_by = $2))
			FOR UPDATE
		`), id, workerID).Scan(&savedEpoch, &startedAt, &resolvedAt); err != nil {
			if errors.Is(err, sql.ErrNoRows) && workerID != "" {
				return ErrOutboxClaimLost
			}
			return err
		}
		if savedEpoch.Valid && !(startedAt.Valid && !resolvedAt.Valid) {
			currentEpoch, err := scoped.CurrentOutboundPolicyEpoch(ctx, orgID)
			if err != nil {
				return err
			}
			allowed, err := scoped.outboundPolicyFlagsAllowSend(ctx, orgID)
			if err != nil {
				return err
			}
			if currentEpoch != savedEpoch.Int64 || !allowed {
				_, err := scoped.q.ExecContext(ctx, `
					UPDATE outbox_messages
					SET status = 'failed',
					    last_error = 'policy_revoked',
					    locked_at = NULL,
					    locked_by = NULL,
					    terminal_at = now()
					WHERE id = $1
				`, id)
				return err
			}
		}
		result, err := scoped.q.ExecContext(ctx, `
			UPDATE outbox_messages
			SET status = 'queued',
			    next_attempt_at = $2,
			    last_error = nullif($3, ''),
			    locked_at = null,
			    locked_by = null,
			    terminal_at = null
			WHERE id = $1
			  AND ($4 = '' OR (status = 'sending' AND locked_by = $4))
		`, id, nextAttemptAt, lastError, workerID)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows != 1 && workerID != "" {
			return ErrOutboxClaimLost
		}
		return nil
	})
}

// UpdateOutboxDeliveryStatus updates the current delivery status with monotonic enforcement.
// Delivery events can arrive out of order (e.g., "delivery_delayed" then "delivered").
// The delivery_status_severity() SQL function ensures status never regresses.
func (s *Store) UpdateOutboxDeliveryStatus(ctx context.Context, providerMessageID, status string) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE outbox_messages
		SET delivery_status = $1,
		    delivery_status_at = now()
		WHERE provider_message_id = $2
		  AND delivery_status_severity($1) > delivery_status_severity(delivery_status)
	`, status, providerMessageID)
	return err
}

// InsertOutboxEvent appends a delivery event to the append-only timeline.
// The outbox_events table is the full audit trail; delivery_status on
// outbox_messages is the derived "current state" for quick UI rendering.
//
// When OutboxMessageID is set, inserts directly using that as the link.
// Otherwise falls back to a join via provider_message_id (legacy webhook
// callback path that only knows the provider's ID).
func (s *Store) InsertOutboxEvent(ctx context.Context, evt OutboxEvent) error {
	_, err := s.InsertOutboxEventReturningID(ctx, evt)
	return err
}

// InsertOutboxEventReturningID is like InsertOutboxEvent but returns
// the persisted event id. Used by the webhook fan-out path so the
// dispatcher has a stable idempotency key per (webhook, event) pair.
// Returns empty string when the legacy join path finds no matching
// outbox_message row (e.g. event for a message from another org).
func (s *Store) InsertOutboxEventReturningID(ctx context.Context, evt OutboxEvent) (string, error) {
	if evt.OutboxMessageID == "" && evt.ProviderMessageID == "" {
		return "", errors.New("InsertOutboxEvent: must provide OutboxMessageID or ProviderMessageID")
	}
	if evt.OutboxMessageID != "" {
		var id string
		row := s.q.QueryRowContext(ctx, `
			INSERT INTO outbox_events (org_id, outbox_message_id, provider_message_id, event_type, raw_payload, reason)
			VALUES ($1, $2, nullif($3, ''), $4, $5, nullif($6, ''))
			RETURNING id::text
		`, evt.OrgID, evt.OutboxMessageID, evt.ProviderMessageID, evt.EventType, evt.RawPayload, evt.Reason)
		if err := row.Scan(&id); err != nil {
			return "", err
		}
		return id, nil
	}
	// Legacy join path: webhook callbacks only have the provider's ID.
	// QueryRow returns sql.ErrNoRows if the INSERT...SELECT inserted
	// zero rows (unknown provider_message_id); upstream callers treat
	// that as a benign "we don't have this message" signal.
	var id string
	row := s.q.QueryRowContext(ctx, `
		INSERT INTO outbox_events (org_id, outbox_message_id, provider_message_id, event_type, raw_payload, reason)
		SELECT $1, om.id, $2, $3, $4, $5
		FROM outbox_messages om WHERE om.provider_message_id = $2
		RETURNING id::text
	`, evt.OrgID, evt.ProviderMessageID, evt.EventType, evt.RawPayload, evt.Reason)
	if err := row.Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return id, nil
}

// IsSuppressed reports whether (orgID, email) is on the per-org suppression
// list. Email matching is case-insensitive. Returns the reason text when
// suppressed (e.g. "hard_bounce", "complaint", "manual:abuse").
func (s *Store) IsSuppressed(ctx context.Context, orgID, email string) (bool, string, error) {
	if orgID == "" || email == "" {
		return false, "", nil
	}
	var reason string
	row := s.q.QueryRowContext(ctx, `
		SELECT reason FROM suppressions
		WHERE org_id = $1 AND email_lower = lower($2)
		LIMIT 1
	`, orgID, email)
	if err := row.Scan(&reason); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, "", nil
		}
		return false, "", err
	}
	return true, reason, nil
}

// AddSuppression upserts a row into the suppression list. Used by webhook
// ingest (bounce/complaint) and the admin CRUD endpoint. Idempotent.
func (s *Store) AddSuppression(ctx context.Context, orgID, email, reason, source string) error {
	if orgID == "" || email == "" {
		return errors.New("missing org_id or email")
	}
	if source == "" {
		source = "manual"
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO suppressions (org_id, email_lower, reason, source)
		VALUES ($1, lower($2), $3, $4)
		ON CONFLICT (org_id, email_lower)
		DO UPDATE SET reason = EXCLUDED.reason, source = EXCLUDED.source
	`, orgID, email, reason, source)
	return err
}

// RemoveSuppression deletes a row from the suppression list, allowing
// future sends to that recipient. Returns true when a row was removed.
func (s *Store) RemoveSuppression(ctx context.Context, orgID, email string) (bool, error) {
	if orgID == "" || email == "" {
		return false, errors.New("missing org_id or email")
	}
	result, err := s.q.ExecContext(ctx, `
		DELETE FROM suppressions
		WHERE org_id = $1 AND email_lower = lower($2)
	`, orgID, email)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ListSuppressions returns all suppression entries for an org, newest first.
func (s *Store) ListSuppressions(ctx context.Context, orgID string, limit int) ([]Suppression, error) {
	if orgID == "" {
		return nil, errors.New("missing org_id")
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT org_id::text, email_lower, reason, source, created_at
		FROM suppressions
		WHERE org_id = $1
		ORDER BY created_at DESC
		LIMIT $2
	`, orgID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Suppression
	for rows.Next() {
		var s Suppression
		if err := rows.Scan(&s.OrgID, &s.Email, &s.Reason, &s.Source, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// checkSendableDomain verifies that the `From` address's domain is in a
// sendable state for the org. Returns ErrDomainNotVerified when the
// domain is explicitly claimed in org_domains but not yet active or
// verified_dns. Returns nil when the domain is sendable, not claimed at
// all (preserves legacy/dev paths), or when inputs are incomplete.
func (s *Store) checkSendableDomain(ctx context.Context, orgID, from string) error {
	if orgID == "" || from == "" {
		return nil
	}
	domain := extractDomainFromAddress(from)
	if domain == "" {
		return nil
	}
	// Internal dev/local paths aren't registered in org_domains. Skip
	// the check entirely rather than breaking local development.
	switch domain {
	case "local.neuralmail", "localhost":
		return nil
	}

	var status string
	row := s.q.QueryRowContext(ctx, `
		SELECT status FROM org_domains
		WHERE org_id = $1 AND lower(domain) = lower($2)
		ORDER BY
			CASE status
				WHEN 'active' THEN 0
				WHEN 'verified_dns' THEN 1
				WHEN 'provisioning' THEN 2
				WHEN 'pending' THEN 3
				WHEN 'failed' THEN 4
				ELSE 5
			END
		LIMIT 1
	`, orgID, domain)
	if err := row.Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Not claimed by this org — allow (legacy/dev path).
			return nil
		}
		return fmt.Errorf("check domain: %w", err)
	}
	switch status {
	case "active", "verified_dns":
		return nil
	default:
		return fmt.Errorf("%w: domain=%s status=%s", ErrDomainNotVerified, domain, status)
	}
}

// extractDomainFromAddress pulls the domain out of an RFC-5322 mailbox
// address. Handles both "user@host" and "Name <user@host>" forms.
// Returns "" when no domain can be found.
func extractDomainFromAddress(addr string) string {
	raw := strings.TrimSpace(addr)
	if raw == "" {
		return ""
	}
	if i := strings.LastIndex(raw, "<"); i >= 0 {
		if j := strings.Index(raw[i:], ">"); j > 0 {
			raw = raw[i+1 : i+j]
		}
	}
	at := strings.LastIndex(raw, "@")
	if at < 0 || at == len(raw)-1 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(raw[at+1:]))
}

// GetOutboxMessageByIDForOrg returns the outbox row for (orgID, id).
// Returns sql.ErrNoRows when not found or when the row belongs to a
// different org. Used by the DLQ admin endpoints and the B5 status
// lookup endpoint.
func (s *Store) GetOutboxMessageByIDForOrg(ctx context.Context, orgID, id string) (OutboxMessage, error) {
	var m OutboxMessage
	row := s.q.QueryRowContext(ctx, `
		SELECT id, org_id::text, inbox_id::text, provider, provider_message_id, idempotency_key,
		       "to", "from", subject, coalesce(text_body, ''), coalesce(html_body, ''),
		       status, delivery_status, delivery_status_at,
		       attempt_count, next_attempt_at, last_attempt_at, last_error,
		       locked_at, locked_by, created_at, terminal_at, attachments_released_at,
		       NOT EXISTS (
		         SELECT 1 FROM outbox_attachments attachment
		         WHERE attachment.org_id = outbox_messages.org_id
		           AND attachment.outbox_message_id = outbox_messages.id
		           AND attachment.blob_sha256 IS NULL
		       ) AS attachments_available
		FROM outbox_messages
		WHERE org_id = $1 AND id = $2
	`, orgID, id)
	if err := row.Scan(
		&m.ID, &m.OrgID, &m.InboxID, &m.Provider, &m.ProviderMessageID, &m.IdempotencyKey,
		&m.To, &m.From, &m.Subject, &m.TextBody, &m.HTMLBody,
		&m.Status, &m.DeliveryStatus, &m.DeliveryStatusAt,
		&m.AttemptCount, &m.NextAttemptAt, &m.LastAttemptAt, &m.LastError,
		&m.LockedAt, &m.LockedBy, &m.CreatedAt, &m.TerminalAt, &m.AttachmentsReleasedAt,
		&m.AttachmentsAvailable,
	); err != nil {
		return OutboxMessage{}, err
	}
	return m, nil
}

// ListFailedOutboxForOrg returns up to limit permanently-failed outbox
// messages for an org, newest first. Used by the B3 DLQ admin list
// endpoint.
func (s *Store) ListFailedOutboxForOrg(ctx context.Context, orgID string, limit int) ([]OutboxMessage, error) {
	if orgID == "" {
		return nil, errors.New("missing org_id")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, org_id::text, inbox_id::text, provider, provider_message_id, idempotency_key,
		       "to", "from", subject, coalesce(text_body, ''), coalesce(html_body, ''),
		       status, delivery_status, delivery_status_at,
		       attempt_count, next_attempt_at, last_attempt_at, last_error,
		       locked_at, locked_by, created_at, terminal_at, attachments_released_at,
		       NOT EXISTS (
		         SELECT 1 FROM outbox_attachments attachment
		         WHERE attachment.org_id = outbox_messages.org_id
		           AND attachment.outbox_message_id = outbox_messages.id
		           AND attachment.blob_sha256 IS NULL
		       ) AS attachments_available
		FROM outbox_messages
		WHERE org_id = $1 AND status = 'failed'
		ORDER BY created_at DESC, id DESC
		LIMIT $2
	`, orgID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxMessage
	for rows.Next() {
		var m OutboxMessage
		if err := rows.Scan(
			&m.ID, &m.OrgID, &m.InboxID, &m.Provider, &m.ProviderMessageID, &m.IdempotencyKey,
			&m.To, &m.From, &m.Subject, &m.TextBody, &m.HTMLBody,
			&m.Status, &m.DeliveryStatus, &m.DeliveryStatusAt,
			&m.AttemptCount, &m.NextAttemptAt, &m.LastAttemptAt, &m.LastError,
			&m.LockedAt, &m.LockedBy, &m.CreatedAt, &m.TerminalAt, &m.AttachmentsReleasedAt,
			&m.AttachmentsAvailable,
		); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListOutboxEventsForMessage returns the append-only event timeline for
// a specific outbox row, oldest first. Used by B3 (DLQ timeline view)
// and B5 (status lookup).
func (s *Store) ListOutboxEventsForMessage(ctx context.Context, orgID, outboxMessageID string) ([]OutboxEventRecord, error) {
	if orgID == "" || outboxMessageID == "" {
		return nil, errors.New("missing org_id or outbox_message_id")
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT id::text, org_id::text, outbox_message_id::text,
		       coalesce(provider_message_id, ''), event_type,
		       raw_payload, coalesce(reason, ''), created_at
		FROM outbox_events
		WHERE org_id = $1 AND outbox_message_id = $2
		ORDER BY created_at ASC
	`, orgID, outboxMessageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxEventRecord
	for rows.Next() {
		var r OutboxEventRecord
		if err := rows.Scan(&r.ID, &r.OrgID, &r.OutboxMessageID, &r.ProviderMessageID, &r.EventType, &r.RawPayload, &r.Reason, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReleaseSentOutboxAttachments releases blob references for sent messages
// whose terminal timestamp is at or before terminalBefore. Message rows,
// attachment metadata, digests, events, and idempotency tombstones remain.
func (s *Store) ReleaseSentOutboxAttachments(ctx context.Context, terminalBefore time.Time, limit int) (int, error) {
	if terminalBefore.IsZero() {
		return 0, errors.New("missing outbox attachment release cutoff")
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	released := 0
	err := s.withTx(ctx, func(scoped *Store) error {
		type candidate struct {
			id    string
			orgID string
		}
		rows, err := scoped.q.QueryContext(ctx, `
			SELECT id, org_id
			FROM outbox_messages
			WHERE status = 'sent'
			  AND terminal_at <= $1
			  AND attachments_released_at IS NULL
			ORDER BY terminal_at, id
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		`, terminalBefore, limit)
		if err != nil {
			return err
		}
		var candidates []candidate
		for rows.Next() {
			var item candidate
			if err := rows.Scan(&item.id, &item.orgID); err != nil {
				rows.Close()
				return err
			}
			candidates = append(candidates, item)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}

		for _, item := range candidates {
			if _, err := scoped.q.ExecContext(ctx, `
				UPDATE attachment_blobs blob
				SET last_ref_at = now()
				WHERE blob.org_id = $1
				  AND EXISTS (
				    SELECT 1
				    FROM outbox_attachments attachment
				    WHERE attachment.org_id = blob.org_id
				      AND attachment.outbox_message_id = $2
				      AND attachment.sha256 = blob.sha256
				      AND attachment.blob_sha256 IS NOT NULL
				  )
			`, item.orgID, item.id); err != nil {
				return err
			}
			if _, err := scoped.q.ExecContext(ctx, `
				UPDATE outbox_attachments
				SET blob_sha256 = NULL
				WHERE org_id = $1
				  AND outbox_message_id = $2
				  AND blob_sha256 IS NOT NULL
			`, item.orgID, item.id); err != nil {
				return err
			}
			if _, err := scoped.q.ExecContext(ctx, `
				UPDATE outbox_messages
				SET attachments_released_at = now()
				WHERE org_id = $1 AND id = $2
			`, item.orgID, item.id); err != nil {
				return err
			}
		}
		released = len(candidates)
		return nil
	})
	return released, err
}

// AbandonOutboxMessage permanently releases attachment bytes for a failed
// outbox row while retaining its DLQ history and attachment metadata. Repeating
// an already-completed abandon is a no-op. The parent lock serializes callers.
func (s *Store) AbandonOutboxMessage(ctx context.Context, orgID, id string) error {
	if orgID == "" || id == "" {
		return errors.New("missing org_id or id")
	}
	return s.withTx(ctx, func(scoped *Store) error {
		var status string
		var releasedAt sql.NullTime
		if err := scoped.q.QueryRowContext(ctx, `
			SELECT status, attachments_released_at
			FROM outbox_messages
			WHERE org_id = $1 AND id = $2
			FOR UPDATE
		`, orgID, id).Scan(&status, &releasedAt); err != nil {
			return err
		}
		if status != "failed" {
			return ErrOutboxNotFailed
		}
		if releasedAt.Valid {
			return nil
		}
		if _, err := scoped.q.ExecContext(ctx, `
			UPDATE attachment_blobs blob
			SET last_ref_at = now()
			WHERE blob.org_id = $1
			  AND EXISTS (
			    SELECT 1
			    FROM outbox_attachments attachment
			    WHERE attachment.org_id = blob.org_id
			      AND attachment.outbox_message_id = $2
			      AND attachment.sha256 = blob.sha256
			      AND attachment.blob_sha256 IS NOT NULL
			  )
		`, orgID, id); err != nil {
			return err
		}
		if _, err := scoped.q.ExecContext(ctx, `
			UPDATE outbox_attachments
			SET blob_sha256 = NULL
			WHERE org_id = $1
			  AND outbox_message_id = $2
			  AND blob_sha256 IS NOT NULL
		`, orgID, id); err != nil {
			return err
		}
		if _, err := scoped.q.ExecContext(ctx, `
			UPDATE outbox_messages
			SET attachments_released_at = now()
			WHERE org_id = $1 AND id = $2
		`, orgID, id); err != nil {
			return err
		}

		toolCallID, err := scoped.RecordToolCall(ctx, "abandon_outbox_message", id, "", "control-plane", 0)
		if err != nil {
			return err
		}
		input := sha256.Sum256([]byte(orgID + "\x00" + id))
		output := sha256.Sum256([]byte("attachments_released"))
		return scoped.RecordAudit(
			ctx,
			toolCallID,
			"nerve:admin.deliverability",
			hex.EncodeToString(input[:]),
			hex.EncodeToString(output[:]),
			id,
		)
	})
}

// ReplayOutboxMessage resets a failed outbox row so the worker picks it
// up on the next claim cycle. Clears attempt_count, last_error, lock
// fields, and sets next_attempt_at to now. Only replays rows in the
// `failed` state; returns false when the row is not eligible.
func (s *Store) ReplayOutboxMessage(ctx context.Context, orgID, id string) (bool, error) {
	if orgID == "" || id == "" {
		return false, errors.New("missing org_id or id")
	}
	var replayed bool
	err := s.withTx(ctx, func(scoped *Store) error {
		var status string
		var releasedDigests string
		if err := scoped.q.QueryRowContext(ctx, `
			SELECT outbox.status,
			       COALESCE((
			         SELECT string_agg(DISTINCT attachment.sha256, ',' ORDER BY attachment.sha256)
			         FROM outbox_attachments attachment
			         WHERE attachment.org_id = outbox.org_id
			           AND attachment.outbox_message_id = outbox.id
			           AND attachment.blob_sha256 IS NULL
			       ), '')
			FROM outbox_messages outbox
			WHERE outbox.org_id = $1 AND outbox.id = $2
			FOR UPDATE
		`, orgID, id).Scan(&status, &releasedDigests); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if status != "failed" {
			return nil
		}
		if releasedDigests != "" {
			return fmt.Errorf("%w: digests=%s", ErrAttachmentsReleased, releasedDigests)
		}
		if _, err := scoped.q.ExecContext(ctx, `
			UPDATE outbox_messages
			SET status = 'queued',
			    attempt_count = 0,
			    next_attempt_at = now(),
			    last_attempt_at = null,
			    last_error = null,
			    locked_at = null,
			    locked_by = null,
			    terminal_at = null
			WHERE org_id = $1 AND id = $2
		`, orgID, id); err != nil {
			return err
		}
		replayed = true
		return nil
	})
	return replayed, err
}
