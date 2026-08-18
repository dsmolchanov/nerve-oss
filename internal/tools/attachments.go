package tools

import (
	"encoding/base64"
	"fmt"
	"strings"
	"unicode"

	"neuralmail/internal/store"
)

const (
	maxAttachmentCount         = 10
	maxAttachmentBytes         = 10 << 20
	maxAttachmentTotalBytes    = 10 << 20
	maxAttachmentFilenameBytes = 255
)

var allowedAttachmentContentTypes = map[string]struct{}{
	"image/png":       {},
	"image/jpeg":      {},
	"image/webp":      {},
	"application/pdf": {},
	"text/plain":      {},
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": {},
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":       {},
}

// AttachmentInput is the MCP-facing attachment representation. ContentBase64
// is decoded and discarded before the value reaches the store.
type AttachmentInput struct {
	Filename      string `json:"filename"`
	ContentType   string `json:"content_type"`
	ContentBase64 string `json:"content_base64"`
}

// AttachmentInputError gives MCP callers a stable, machine-readable code for
// attachment validation failures.
type AttachmentInputError struct {
	Code    string
	Ordinal int
}

func (e *AttachmentInputError) Error() string {
	if e.Ordinal < 0 {
		return e.Code
	}
	return fmt.Sprintf("%s: attachment %d", e.Code, e.Ordinal)
}

func attachmentInputError(code string, ordinal int) error {
	return &AttachmentInputError{Code: code, Ordinal: ordinal}
}

// DecodeOutboundAttachments validates the complete attachment collection,
// normalizes filename/content type once, and returns the provider/store shape.
func DecodeOutboundAttachments(inputs []AttachmentInput) ([]store.OutboundAttachment, error) {
	if len(inputs) > maxAttachmentCount {
		return nil, attachmentInputError("attachment_count_exceeded", -1)
	}

	attachments := make([]store.OutboundAttachment, 0, len(inputs))
	totalBytes := 0
	for ordinal, input := range inputs {
		filename := strings.TrimSpace(input.Filename)
		if invalidAttachmentFilename(filename) {
			return nil, attachmentInputError("attachment_invalid_filename", ordinal)
		}

		contentType := strings.ToLower(strings.TrimSpace(input.ContentType))
		if _, ok := allowedAttachmentContentTypes[contentType]; !ok {
			return nil, attachmentInputError("attachment_type_not_allowed", ordinal)
		}

		// Reject on the encoded length first. Decoding to find out costs the
		// allocation the limit exists to prevent, and concurrent oversized
		// requests could exceed the process memory budget before any of them
		// was refused. Base64 expands by 4/3, so this bounds the decode.
		if len(input.ContentBase64) > base64.StdEncoding.EncodedLen(maxAttachmentBytes) {
			return nil, attachmentInputError("attachment_too_large", ordinal)
		}
		content, err := base64.StdEncoding.Strict().DecodeString(input.ContentBase64)
		if err != nil {
			return nil, attachmentInputError("attachment_invalid_encoding", ordinal)
		}
		if len(content) == 0 {
			return nil, attachmentInputError("attachment_empty", ordinal)
		}
		if len(content) > maxAttachmentBytes {
			return nil, attachmentInputError("attachment_too_large", ordinal)
		}
		totalBytes += len(content)
		if totalBytes > maxAttachmentTotalBytes {
			return nil, attachmentInputError("attachment_total_too_large", ordinal)
		}

		attachments = append(attachments, store.OutboundAttachment{
			Filename:    filename,
			ContentType: contentType,
			Content:     content,
		})
	}
	return attachments, nil
}

func invalidAttachmentFilename(filename string) bool {
	if filename == "" || len([]byte(filename)) > maxAttachmentFilenameBytes {
		return true
	}
	for _, character := range filename {
		if character == '/' || character == '\\' || character == 0 || unicode.IsControl(character) {
			return true
		}
	}
	return false
}
