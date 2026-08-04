package mcp

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"neuralmail/internal/auth"
	"neuralmail/internal/config"
	"neuralmail/internal/tools"
)

type attachmentFeatureGateStub struct {
	enabled bool
	err     error
	orgID   string
}

func (g *attachmentFeatureGateStub) Enabled(_ context.Context, flag string, orgID string) (bool, error) {
	if flag != "attachments" {
		return false, errors.New("unexpected flag")
	}
	g.orgID = orgID
	return g.enabled, g.err
}

func TestListToolsGatesAttachmentSchemas(t *testing.T) {
	for _, test := range []struct {
		name    string
		enabled bool
	}{
		{name: "off"},
		{name: "on", enabled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			listed := ListTools(test.enabled)
			toolsList := listed["tools"].([]map[string]any)
			for _, tool := range toolsList {
				name := tool["name"].(string)
				schema, hasSchema := tool["inputSchema"].(map[string]any)
				switch name {
				case "compose_email", "send_reply":
					if !hasSchema {
						t.Fatalf("%s has no input schema", name)
					}
					properties := schema["properties"].(map[string]any)
					_, hasAttachments := properties["attachments"]
					if hasAttachments != test.enabled {
						t.Fatalf("%s attachments presence=%t, want %t", name, hasAttachments, test.enabled)
					}
				case "draft_reply_with_policy":
					if hasSchema {
						properties := schema["properties"].(map[string]any)
						if _, ok := properties["attachments"]; ok {
							t.Fatal("draft_reply_with_policy must not expose attachments")
						}
					}
				}
			}
		})
	}
}

func TestDecodeAttachmentsUsesOrgScopedFailClosedGate(t *testing.T) {
	input := []tools.AttachmentInput{{
		Filename:      "note.txt",
		ContentType:   "text/plain",
		ContentBase64: base64.StdEncoding.EncodeToString([]byte("hello")),
	}}
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{OrgID: "org-a"})

	for _, test := range []struct {
		name    string
		gate    *attachmentFeatureGateStub
		wantErr bool
	}{
		{name: "enabled", gate: &attachmentFeatureGateStub{enabled: true}},
		{name: "disabled", gate: &attachmentFeatureGateStub{}, wantErr: true},
		{name: "lookup error", gate: &attachmentFeatureGateStub{err: errors.New("database unavailable")}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := NewServer(config.Default(), nil, nil, nil)
			server.FeatureFlags = test.gate
			attachments, err := server.decodeAttachments(ctx, input)
			if (err != nil) != test.wantErr {
				t.Fatalf("decode error=%v, wantErr=%t", err, test.wantErr)
			}
			if test.wantErr {
				var inputErr *tools.AttachmentInputError
				if !errors.As(err, &inputErr) || inputErr.Code != "attachment_feature_disabled" {
					t.Fatalf("expected feature-disabled typed error, got %v", err)
				}
			} else if len(attachments) != 1 || string(attachments[0].Content) != "hello" {
				t.Fatalf("unexpected decoded attachments: %#v", attachments)
			}
			if test.gate.orgID != "org-a" {
				t.Fatalf("gate saw org %q", test.gate.orgID)
			}
		})
	}
}

func TestDecodeAttachmentsDoesNotQueryGateWhenAbsent(t *testing.T) {
	server := NewServer(config.Default(), nil, nil, nil)
	attachments, err := server.decodeAttachments(context.Background(), nil)
	if err != nil || attachments != nil {
		t.Fatalf("empty attachments should bypass the gate: attachments=%v err=%v", attachments, err)
	}
}
