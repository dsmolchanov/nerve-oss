# Preserve Compose Email Sender Names Implementation Plan

## Overview

Preserve the optional sender display name supplied to the MCP `compose_email`
tool while keeping the verified inbox address as the transport sender. This
lets transactional callers produce recognizable mailboxes such as
`Агата AI <support@ahata.ai>` instead of a bare address.

## Current State Analysis

- `compose_email` accepts unknown JSON fields but does not decode or forward
  `from_name`, so callers cannot control the visible sender name.
- The outbox stores only the inbox address and both SMTP and Resend receive the
  bare address.
- SMTP uses the same string for the visible `From` header and the envelope
  sender, so adding a display name without parsing would make `MAIL FROM`
  invalid.
- Content deduplication relies on a partial unique index for queued/sending
  rows. Concurrent inserts with different idempotency keys can race and expose
  a uniqueness error instead of returning the winning outbox ID.

## Desired End State

1. `compose_email` accepts an optional, validated `from_name`.
2. Resend and message storage retain the formatted RFC mailbox.
3. SMTP keeps the formatted header but uses the bare address for its envelope.
4. Callers that omit `from_name` retain the existing behavior and public Go
   method signature.
5. Concurrent content-equivalent enqueue calls return one shared outbox ID
   without uniqueness errors.

## What We're NOT Doing

- No inbox-address, domain-verification, or authorization changes.
- No new delivery provider or dependency.
- No database schema change beyond the existing content-dedup migration.
- No delivery-event or mailbox-placement tracking.

## Implementation Approach

Decode `from_name` at the MCP boundary, reject control characters, and format
it with `net/mail`. Pass the formatted mailbox through the outbox while storing
the display name and bare address separately in the message participant. Parse
the SMTP sender before constructing the envelope. Preserve the old
`ComposeEmail` method as a wrapper over an options-aware method.

For content deduplication, insert with `ON CONFLICT DO NOTHING`. If another
idempotency or content-equivalent insert wins, resolve its ID in a separate
statement so PostgreSQL `READ COMMITTED` obtains a fresh snapshot after a
concurrent transaction commits.

## Phase 1: MCP and Sender Formatting

### Changes Required

- `internal/mcp/server.go`: decode and forward optional `from_name`.
- `internal/tools/service.go`: validate/format the display name and preserve
  the legacy method signature.
- `docs/MCP_Contract.md`: document the optional field.

### Automated Verification

- [x] MCP JSON decoding covers present and omitted `from_name`.
- [x] Unicode, ASCII, quoted, and empty display names format correctly.
- [x] Control-character/header-injection inputs are rejected.

## Phase 2: Provider and Storage Semantics

### Changes Required

- `internal/emailtransport/providers/smtp/smtp_outbound.go`: use a bare SMTP
  envelope sender while retaining the formatted `From` header.
- `internal/store/outbox.go`: store the formatted sender and safely resolve
  idempotency/content conflicts.
- Preserve existing Resend behavior, which already accepts an RFC mailbox.

### Automated Verification

- [x] SMTP tests cover the envelope/header split and legacy bare addresses.
- [x] Resend tests cover the formatted mailbox.
- [x] PostgreSQL integration covers Unicode sender storage.
- [x] Sequential content-equivalent enqueues share one outbox ID.
- [x] Concurrent content-equivalent enqueues share one outbox ID without an
  error.

## Testing Strategy

- `go test ./...`
- `go vet ./...`
- `gofmt` on changed Go files.
- `git diff --check`
- PostgreSQL-backed integration tests, including concurrent enqueue calls.

## Manual Verification

1. Publish and promote the reviewed runtime through the cloud runtime lock.
2. Call `compose_email` with a Unicode `from_name` and an authorized test
   recipient.
3. Confirm the provider sees the formatted sender mailbox and the recipient
   sees the display name while SPF/DKIM remain aligned to the inbox address.

## Rollback

Roll back to the prior immutable runtime image. No data migration or cleanup is
required.
