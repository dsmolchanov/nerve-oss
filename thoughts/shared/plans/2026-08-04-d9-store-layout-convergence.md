# D9 Store Layout Convergence

## Context

The approved inbound-events and attachments rollout requires shared store files
to be mirrored byte-for-byte between `nerve-oss` and `nerve-cloud`. Cloud already
keeps thread, message, tool-call, and audit accessors in `store_threads.go`, while
OSS still keeps the same methods in `store.go`. Copying either file across the
repository boundary therefore creates duplicate declarations and blocks the
baseline backport.

This plan is the OSS-local, self-contained execution record for D9. It covers
only the behavior-preserving layout convergence needed before the backport.

## Scope

- Move these existing methods verbatim from `internal/store/store.go` to a new
  `internal/store/store_threads.go`:
  `ListThreads`, `GetThread`, `GetThreadInboxID`, `GetMessage`,
  `SearchInboxFTS`, `UpsertThread`, `InsertMessage`, `EnsureThread`,
  `UpdateThreadSignals`, `InsertMessageWithThread`, `MessageCount`,
  `RecordToolCall`, `RecordAudit`, and `ListAudit`.
- Preserve every exported declaration, signature, comment, query, scan target,
  and error path.
- Do not change tests or product behavior.

Files in scope:

- `internal/store/store.go`
- `internal/store/store_threads.go`
- `thoughts/shared/plans/2026-08-04-d9-store-layout-convergence.md`

## Out of Scope

- Backporting migrations `0011` through `0017`.
- Backporting `listing.go`, `org_webhooks.go`, `outbox_listen.go`,
  `inbox_smtp_config.go`, or `internal/webhooks/**`.
- Introducing the three-list sync manifest or changing sync workflows.
- Any schema, API, query, or runtime behavior change.

Those items belong to the following Phase 0 baseline-backport change. The
dependency inventory established for that change is: core migrations
`0011`-`0017`; shared store files `listing.go`, `listing_test.go`,
`org_webhooks.go`, `org_webhooks_test.go`, `outbox_listen.go`, and
`inbox_smtp_config.go`; and `internal/webhooks/{dispatcher.go,
dispatcher_test.go,dispatcher_integration_test.go}`. Cloud-only billing, org,
token, and usage store files are not backport dependencies.

## Verification

- [x] Compare the sorted exported declaration set from
  `go doc -all ./internal/store` before and after the move; both contain 118
  declarations and are identical.
- [x] `go build ./...` passes.
- [x] `go vet ./...` passes.
- [x] `gofmt -l` reports no changed Go file.
- [x] All 15 Go test packages pass against PostgreSQL 16.
- [x] No test file changes in this PR.

## Rollback

Move the methods back into `store.go`. There is no data migration or cleanup.
