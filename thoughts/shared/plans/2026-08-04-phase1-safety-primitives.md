---
date: 2026-08-04T19:11:28+02:00
git_commit: 135932f4fa6fe28f91f820ce86583b960fdd6bbc
branch: codex/phase1-safety-primitives
repository: nerve-oss
source_plan: nerve-cloud/thoughts/shared/plans/2026-08-02-inbound-events-and-attachments.md
source_section: Phase 1
status: implementing
---

# Phase 1 shared safety primitives

## Goal

Land the OSS-owned primitives that later event and attachment phases require,
then let the ownership manifest copy all exact-mirror packages into Cloud.

## OSS implementation

1. Add `internal/httpsafe`: require an explicit timeout, refuse redirects,
   optionally require an exact host allowlist, and reject loopback, private,
   link-local, unique-local, unspecified, multicast, and other non-global
   addresses in `net.Dialer.ControlContext` after DNS resolution.
2. Construct the production webhook dispatcher client with `httpsafe` and cap
   response draining at 64 KiB. Tests may continue injecting an HTTP client.
3. Add an immediate, byte-denominated `internal/memguard.Budget` with
   idempotent releases and observable limit/used/available values. Configure a
   64 MiB runtime budget and a 30-second request read timeout.
4. Guard MCP request bodies with the shared budget and a 16 MiB wire cap.
   Understated or absent `Content-Length` is trued up from bytes actually read;
   exhaustion returns `503` plus `Retry-After`, wire overflow returns `413`,
   and every exit releases its reservation. Require EOF after the one accepted
   JSON value so a valid prefix cannot hide an oversized chunked tail. Set both
   server `ReadTimeout` and a request deadline so trickling bodies cannot pin
   memory.
5. Declare `store.OutboundAttachment` and attach it to the provider-facing
   `emailtransport.OutboundMessage`, avoiding a store/emailtransport import
   cycle. Actual blob loading begins only after the attachment schema exists.
6. Add `Store.withTx`. Bare stores open and own a transaction; stores already
   scoped by `RunAsOrg` execute inline on the caller transaction so tenant GUCs
   remain on the same connection.

## Cloud follow-through after sync

- Replace `postWebhookForward`'s default client with `httpsafe`.
- Share a configured `memguard.Budget` across the control-plane REST download,
  outbound worker, and the later bounded mirror worker. Attachment reads must
  acquire before selecting `bytea` content.
- Add the corresponding memory gauges to the existing Cloud metrics surface.

Those call sites are Cloud-only or depend on migrations from later phases, so
they are not fabricated in the OSS PR.

## Verification

- Dial-time rejection covers IPv4/IPv6 loopback, RFC1918, CGNAT, metadata
  link-local, IPv6 link-local/unique-local, unspecified, multicast, benchmark,
  documentation, translation/tunnelling, protocol-assignment and reserved
  ranges; a hostname resolving to loopback fails even when allowlisted, and
  redirects are refused.
- Webhook responses larger than 64 KiB are not drained without bound.
- Twenty concurrent one-byte reservations against a ten-byte budget admit
  exactly ten; success, error, timeout, cancellation, and double-release paths
  leave usage at zero.
- A request declaring one byte but sending a larger JSON body exhausts the
  real budget with `503`; a body over 16 MiB returns `413`, including a chunked
  body with a valid first JSON value and an oversized trailing segment.
- A slow body cannot outlive the configured read timeout or leak its prepaid
  reservation.
- Bare `withTx` commit/rollback and nested `RunAsOrg` rollback are DB-backed;
  the nested callback receives the same transaction-scoped store.
- The inherited OSS billing, cloudapi, entitlement, reconcile and webhook DB
  helpers keep their admin connection alive through database-drop cleanup; a
  full suite leaves zero `nerve_{bill,cloud,ent,rec,wh,test}_*` databases.
- `go build ./...`, `go vet ./...`, both timezone suites, exact-mirror staged
  Cloud build/test, `actionlint`, and `shellcheck` pass.
