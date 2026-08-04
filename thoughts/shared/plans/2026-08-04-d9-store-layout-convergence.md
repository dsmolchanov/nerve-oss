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

## Phase 0 §4 shared-core reconciliation (2026-08-04)

The boundary split exposed a second class of drift: files intended for the
exact-mirror set differed in both additive baseline behavior and internal Go
types. Reconcile the OSS copies to nerve-cloud at `d62c4a0` for:

- `store.go`;
- `store_threads.go`;
- `org_domains.go`;
- `inboxes_manage.go`.

This is the start of the Phase 0 §4 baseline backport, not another pure-move
D9 change. The new message reply/receiving fields and domain receiving,
catch-all, and forwarding accessors depend on the separately backported core
migrations `0011`-`0017` before they can execute against a fresh OSS database.

Two internal API changes are intentional convergence, not removals from the
supported runtime contract:

- `SubscriptionSummary` now uses JSON-ready `*time.Time` values and includes
  the entitlement limits already returned by Cloud. `internal/store` is a Go
  `internal` package, and all repository consumers are updated and tested in
  the same baseline integration. The OSS-only billing boundary converts its
  existing `sql.NullTime` scan values and populates the added limits without
  changing the shared file.
- `CreateInboxForOrg` gains the Cloud `outboundProvider` argument. The existing
  OSS handler passes `"smtp"` explicitly, preserving its prior provider and
  database behavior.

Ownership correction: add `internal/store/inboxes_manage.go` to the
**exact-mirror** list. Its old three-argument constructor was the only reason it
could not be asserted; after the caller update, the file is byte-identical.
The OSS-only handler call remains outside the exact set.

`docs/MCP_Contract.md` deliberately remains OSS-authoritative. OSS implements
and tests `compose_email.from_name`; Cloud's copy is stale by that one field.
The exact-mirror gate must copy this OSS document into Cloud, not delete a live
runtime contract from OSS.

Verification:

- [x] The four reconciled store files are byte-identical to Cloud `d62c4a0`.
- [x] The complete pre-change exported declaration set remains present; the
  only changed existing shapes are the two documented internal convergences.
- [x] `go build ./...`, `go vet ./...`, and `go test -race ./... -count=1`
  pass without a database DSN.
- [ ] The real-PostgreSQL suite becomes green only when this commit is combined
  with the §4 migration backport: the isolated branch still ends at core
  `0010`, so the reconciled queries correctly expose missing `in_reply_to`,
  `references`, `received_email_id`, `forward_to`, and receiving columns.
- [x] `gofmt -l` and `git diff --check` pass.
