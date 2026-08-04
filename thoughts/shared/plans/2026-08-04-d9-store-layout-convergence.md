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

## Boundary follow-up (2026-08-04)

The first D9 PR aligned the thread/message layout, but the Phase 0 §4
exact-mirror gate still could not treat `store.go` as shared: OSS kept
cloud-control-plane billing, organization, token, and usage methods in that
file while Cloud already isolates those methods behind four cloud-only file
boundaries.

Complete the behavior-preserving layout work by moving the existing OSS
declarations into:

- `store_billing.go` — entitlement, subscription, and billing webhook methods;
- `store_orgs.go` — organization/default bootstrap and MCP endpoint methods;
- `store_tokens.go` — service-token/API-key methods plus their private scope
  parsing helpers;
- `store_usage.go` — usage counter, reservation, reconciliation, and event
  methods.

This follow-up is still pure movement. It does not copy Cloud implementations,
add Cloud-only methods, change types or queries, or claim that `store.go` is
already byte-identical. The remaining shared type/order reconciliation and the
three-list manifest belong to Phase 0 §4 after this boundary exists.

Verification:

- [x] The complete `go doc -all ./internal/store` output is identical before
  and after the move (SHA-256
  `2b19e61e36b0137c9c3e09ee3858f02d78003b9d2879c11ceca9e8c775f1216a`).
- [x] All 36 moved methods and both private token helpers retain their original
  declarations and bodies.
- [x] `go build ./...`, `go vet ./...`, and `go test ./... -count=1` pass.
- [x] `gofmt` and `git diff --check` pass.
