package mcp

import (
	"encoding/json"
	"testing"
)

func TestComposeEmailInputDecodesFromName(t *testing.T) {
	var input composeEmailInput
	err := json.Unmarshal([]byte(`{
		"inbox_id":"inbox-1",
		"to":"invitee@example.com",
		"subject":"Invitation",
		"body":"Open the invitation link",
		"from_name":"Агата AI",
		"idempotency_key":"invitation-1"
	}`), &input)
	if err != nil {
		t.Fatalf("decode compose_email input: %v", err)
	}
	if input.FromName != "Агата AI" {
		t.Fatalf("expected from_name to be decoded, got %q", input.FromName)
	}
}

func TestComposeEmailInputKeepsFromNameOptional(t *testing.T) {
	var input composeEmailInput
	if err := json.Unmarshal([]byte(`{"inbox_id":"inbox-1"}`), &input); err != nil {
		t.Fatalf("decode legacy compose_email input: %v", err)
	}
	if input.FromName != "" {
		t.Fatalf("expected omitted from_name to stay empty, got %q", input.FromName)
	}
}
