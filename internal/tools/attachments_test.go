package tools

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestDecodeOutboundAttachmentsNormalizesInput(t *testing.T) {
	attachments, err := DecodeOutboundAttachments([]AttachmentInput{{
		Filename:      "  report.pdf  ",
		ContentType:   " APPLICATION/PDF ",
		ContentBase64: base64.StdEncoding.EncodeToString([]byte("report bytes")),
	}})
	if err != nil {
		t.Fatalf("decode attachments: %v", err)
	}
	if len(attachments) != 1 {
		t.Fatalf("expected one attachment, got %d", len(attachments))
	}
	if attachments[0].Filename != "report.pdf" {
		t.Fatalf("expected normalized filename, got %q", attachments[0].Filename)
	}
	if attachments[0].ContentType != "application/pdf" {
		t.Fatalf("expected normalized content type, got %q", attachments[0].ContentType)
	}
	if string(attachments[0].Content) != "report bytes" {
		t.Fatalf("unexpected decoded content %q", attachments[0].Content)
	}
}

func TestDecodeOutboundAttachmentsValidationErrors(t *testing.T) {
	valid := AttachmentInput{
		Filename:      "note.txt",
		ContentType:   "text/plain",
		ContentBase64: base64.StdEncoding.EncodeToString([]byte("x")),
	}

	tests := []struct {
		name   string
		inputs []AttachmentInput
		code   string
	}{
		{name: "count", inputs: repeatAttachment(valid, maxAttachmentCount+1), code: "attachment_count_exceeded"},
		{name: "empty filename", inputs: []AttachmentInput{{ContentType: valid.ContentType, ContentBase64: valid.ContentBase64}}, code: "attachment_invalid_filename"},
		{name: "path filename", inputs: []AttachmentInput{{Filename: "../note.txt", ContentType: valid.ContentType, ContentBase64: valid.ContentBase64}}, code: "attachment_invalid_filename"},
		{name: "control filename", inputs: []AttachmentInput{{Filename: "note\n.txt", ContentType: valid.ContentType, ContentBase64: valid.ContentBase64}}, code: "attachment_invalid_filename"},
		{name: "long filename", inputs: []AttachmentInput{{Filename: strings.Repeat("é", 128), ContentType: valid.ContentType, ContentBase64: valid.ContentBase64}}, code: "attachment_invalid_filename"},
		{name: "type", inputs: []AttachmentInput{{Filename: valid.Filename, ContentType: "text/html", ContentBase64: valid.ContentBase64}}, code: "attachment_type_not_allowed"},
		{name: "encoding", inputs: []AttachmentInput{{Filename: valid.Filename, ContentType: valid.ContentType, ContentBase64: "eA"}}, code: "attachment_invalid_encoding"},
		{name: "empty", inputs: []AttachmentInput{{Filename: valid.Filename, ContentType: valid.ContentType}}, code: "attachment_empty"},
		{name: "single size", inputs: []AttachmentInput{{Filename: valid.Filename, ContentType: valid.ContentType, ContentBase64: base64.StdEncoding.EncodeToString(make([]byte, maxAttachmentBytes+1))}}, code: "attachment_too_large"},
		{name: "total size", inputs: []AttachmentInput{
			{Filename: "one.txt", ContentType: valid.ContentType, ContentBase64: base64.StdEncoding.EncodeToString(make([]byte, maxAttachmentTotalBytes/2+1))},
			{Filename: "two.txt", ContentType: valid.ContentType, ContentBase64: base64.StdEncoding.EncodeToString(make([]byte, maxAttachmentTotalBytes/2+1))},
		}, code: "attachment_total_too_large"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeOutboundAttachments(test.inputs)
			var inputErr *AttachmentInputError
			if !errors.As(err, &inputErr) {
				t.Fatalf("expected typed attachment error, got %v", err)
			}
			if inputErr.Code != test.code {
				t.Fatalf("expected code %q, got %q", test.code, inputErr.Code)
			}
		})
	}
}

func TestDecodeOutboundAttachmentsAcceptsAllowlist(t *testing.T) {
	for contentType := range allowedAttachmentContentTypes {
		t.Run(contentType, func(t *testing.T) {
			_, err := DecodeOutboundAttachments([]AttachmentInput{{
				Filename:      "file.bin",
				ContentType:   contentType,
				ContentBase64: base64.StdEncoding.EncodeToString([]byte("x")),
			}})
			if err != nil {
				t.Fatalf("allowlisted type rejected: %v", err)
			}
		})
	}
}

func repeatAttachment(input AttachmentInput, count int) []AttachmentInput {
	inputs := make([]AttachmentInput, count)
	for index := range inputs {
		inputs[index] = input
	}
	return inputs
}

func TestDecodeOutboundAttachmentsRejectsOversizedEncodingBeforeDecoding(t *testing.T) {
	// One byte past what a legal attachment can encode to. Decoding this to
	// discover the size is exactly the allocation the limit exists to prevent.
	oversized := strings.Repeat("A", base64.StdEncoding.EncodedLen(maxAttachmentBytes)+1)

	_, err := DecodeOutboundAttachments([]AttachmentInput{{
		Filename: "big.pdf", ContentType: "application/pdf", ContentBase64: oversized,
	}})

	var inputErr *AttachmentInputError
	if !errors.As(err, &inputErr) || inputErr.Code != "attachment_too_large" {
		t.Fatalf("expected attachment_too_large, got %v", err)
	}
}
