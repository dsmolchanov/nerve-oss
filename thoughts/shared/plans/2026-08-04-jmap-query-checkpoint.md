# Inbox-Scoped JMAP Query Checkpoint Implementation Plan

## Overview

Keep inbound JMAP polling in one query-state family and one mailbox scope. The
initial `Email/query` checkpoint must feed an inbox-filtered
`Email/queryChanges`, and every added Email must be fetched with the body-value
arguments needed to build complete stored text and HTML content.

This is an OSS-first change. No nerve-cloud files or runtime-lock metadata are
part of this branch.

## Current State Analysis

- `JMAPClient.FetchChanges` stores `Email/query.queryState` on the first poll,
  then sends it as `Email/changes.sinceState`. These tokens belong to different
  JMAP state families.
- The initial query uses `inMailbox`, while `Email/changes` is account-wide, so
  an incremental poll can ingest mail from outside the selected inbox.
- `Email/get` requests `bodyValues`, but the protocol defaults
  `fetchTextBodyValues` and `fetchHTMLBodyValues` to false.
- `messageId` is decoded as a scalar even though the JMAP Email property is a
  string array.
- Body extraction reads only the first `textBody` and `htmlBody` part even
  though those arrays describe parts to render sequentially.
- A failed `Email/get` currently returns the already-advanced state to direct
  callers, risking a skipped retry if they persist it.
- The client does not read the Core capability's `maxObjectsInGet`, so a large
  recovery set could exceed a provider's per-call `Email/get` limit.

## Desired End State

1. Initial and incremental polls use `queryState`/`sinceQueryState`/
   `newQueryState` consistently.
2. `Email/query` and `Email/queryChanges` share the same inbox filter and
   `receivedAt` descending sort.
3. An incremental response advances the query cursor even when `added` is
   empty.
4. An invalid incremental checkpoint (`cannotCalculateChanges`, plus
   `tooManyChanges` defensively) recovers every ID in one stable, inbox-scoped
   query snapshot before accepting its new checkpoint.
5. Any recovery or fetch error returns the caller's previous cursor.
6. `Email/get` is split at the session-advertised `maxObjectsInGet`; failure of
   any batch returns no partial Emails and preserves the caller's cursor.
7. `Email/get` explicitly requests text and HTML body values.
8. The first RFC `messageId` array value is stored as the Internet Message-ID.
9. All ordered text and HTML body parts are concatenated.
10. A checkpoint advances only when every requested `Email/get` ID is accounted
    for exactly once by `list` or `notFound`.

## Files in This Branch

- `internal/jmap/jmap_client.go`: query checkpoint, inbox scope, body fetch,
  cursor-on-error, Message-ID, and multipart extraction behavior.
- `internal/jmap/jmap_client_test.go`: `httptest` protocol regressions for every
  behavior in this plan.
- `thoughts/shared/plans/2026-08-04-jmap-query-checkpoint.md`: scope, follow-ups,
  and verification record.

## Implementation Approach

### 1. Keep One Query-State Family

- Retain the initial `Email/query` with `position: 0` and `limit: 50`.
- Replace the incremental `Email/changes` call with `Email/queryChanges`.
- Send `sinceQueryState`, read `newQueryState`, and extract IDs from ordered
  `added` objects.
- Do not cap `Email/queryChanges` with `maxChanges`; without paging an
  intermediate query-change state could otherwise leave changes unfetched.
- Generate the filter and sort through one helper used by both methods.
- Decode JMAP method errors into a typed error. If the server reports
  `cannotCalculateChanges` (or `tooManyChanges`), discard that checkpoint for
  the current attempt and paginate an inbox-scoped `Email/query` across the
  response's complete `total`.
- Set `calculateTotal: true` on every recovery page. Require a non-empty
  `queryState`, `canCalculateChanges: true`, the requested `position`, and the
  same `queryState` and `total` on every page. Restart from position zero on
  state/total drift, with three attempts maximum.
- Reject empty premature pages, malformed IDs, duplicate IDs, positions that
  do not match the request, and pages inconsistent with `total` or the limit.
- Keep ordinary first-time bootstrap deliberately bounded to the newest 50
  messages, but reject its checkpoint unless `canCalculateChanges` is true.
- Validate `Email/queryChanges.oldQueryState`, `newQueryState`, and every
  ordered `added` object rather than advancing on a malformed response.
- Accept the RFC example's explicit `added: null` as an empty set while still
  rejecting an omitted `added` property.
- If no IDs were added, return the new query state without calling
  `Email/get`.
- Return a fresh recovery state only after the corresponding `Email/get`
  request succeeds; if recovery or `Email/get` fails, return the original input
  cursor.

### 2. Fetch and Normalize Complete Message Content

- Parse and validate positive Core `maxObjectsInGet` session metadata, declare
  both Core and Mail capabilities in API requests, and split `Email/get` into
  batches no larger than the advertised maximum.
- Accumulate batches internally and return no Emails if any batch fails, so a
  caller can safely retry from its unchanged cursor.
- Reject malformed, unknown, duplicate, or unaccounted `Email/get` results and
  restore response order to the request order before returning Emails.
- Add `fetchTextBodyValues: true` and `fetchHTMLBodyValues: true` to
  `Email/get`.
- Read the first value from the RFC `messageId` string array.
- Walk `textBody` and `htmlBody` in server-provided order and append each
  referenced `bodyValues[partId].value`.

### 3. Add HTTP-Level Regression Coverage

Use an `httptest.Server` to decode actual JMAP request payloads and return JMAP
method responses. Cover:

- query-state request and response field names;
- absence of an unpaged `maxChanges` cap;
- identical inbox filter and sort on initial and incremental requests;
- typed JMAP method errors and lossless multi-page invalid-checkpoint recovery;
- stable recovery state/total validation, bounded drift restarts, and rejection
  of empty, duplicate, or inconsistent pages;
- `canCalculateChanges: false` rejection for both initial and recovery queries;
- Core `maxObjectsInGet` session parsing, Core+Mail request capabilities,
  batched `Email/get`, and all-or-nothing batch failure behavior;
- malformed `oldQueryState` and `added` response rejection;
- explicit-null versus missing `added` compatibility;
- required, strictly increasing unsigned `added.index` validation;
- complete `Email/get` list/notFound accounting and malformed-result rejection;
- malformed initial/incremental states and recovery fetch errors preserving the
  caller cursor;
- explicit text and HTML body fetch arguments;
- cursor advancement when `added` is empty;
- old-cursor preservation when `Email/get` fails after query advancement;
- first-value parsing of the `messageId` array;
- ordered multipart text and HTML concatenation.

## Out-of-Scope Follow-ups

- **`cc` persistence:** `Email/get` already asks for `cc`, but the inbound
  model and store path do not preserve it. Add model/storage support in a
  separate change.
- **Ordinary initial backlog pagination:** first-time bootstrap still imports
  only the newest 50 IDs. Recovery from an invalid persisted checkpoint is
  fully paginated; changing the initial product behavior remains a follow-up.
- **Configured `AccountID`:** session discovery still chooses the mail primary
  account rather than honoring `cfg.JMAP.AccountID`. Account-selection policy
  needs a separate compatibility decision and tests.
- **Poll-loop error handling:** checkpoint read/write errors, ingestion errors,
  and embedding-queue errors in `App.PollLoop` still need explicit logging,
  retry/backoff, and failure semantics. This branch only guarantees cursor
  preservation at the JMAP client boundary.
- **Cloud synchronization and deployment:** this OSS-first branch contains no
  nerve-cloud mirror or deployment changes; those proceed after OSS review.

## Automated Verification

- [x] Go formatting: `gofmt -w internal/jmap/jmap_client.go internal/jmap/jmap_client_test.go`
- [x] Focused regressions: `go test ./internal/jmap -run 'Test(FetchChanges|EmailGet|Call|EnsureSession|Recovery)' -count=1`
- [x] Race regressions: `go test -race ./internal/jmap -run 'Test(FetchChanges|EmailGet|Call|EnsureSession|Recovery)' -count=1`
- [x] JMAP package: `go test ./internal/jmap -count=1`
- [x] Static analysis: `go vet ./internal/jmap`
- [x] Patch hygiene: `git diff --check`

## Manual Verification

No live-provider verification is required for this isolated protocol fix. A
later integration pass can validate provider-specific `queryChanges` behavior
without expanding this branch.

## Rollback

Revert the JMAP client and its regression test. There is no schema migration,
checkpoint format migration, or persisted-data cleanup.

## Reference

- RFC 8620, JMAP Core: <https://www.rfc-editor.org/rfc/rfc8620.html>
- RFC 8621, JMAP Mail: <https://www.rfc-editor.org/rfc/rfc8621.html>
