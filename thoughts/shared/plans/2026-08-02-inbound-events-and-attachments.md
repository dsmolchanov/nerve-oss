# Nerve: Inbound Event Fan-out + Attachments (Both Directions)

> **Revision 15 (2026-08-05)** — Phase 8's production rollback proof uses a
> dedicated, bounded and audited delivery hold instead of modifying queue
> timestamps or global feature state. Core migration
> `0028_outbox_delivery_holds.sql` records an exact `(org_id,
> idempotency_key)` hold before the synthetic attachment message is enqueued.
> Claims exclude only that row while its hold is active; expiry is mandatory
> (1–30 minutes) and automatically restores delivery. Hold, idempotent retry,
> and release operations write replay IDs to `audit_log`; durable history is
> retained and makes the down migration refuse. Runtime schema compatibility
> advances to `[28,28]`. The operator drill must release the row after the
> attachment flag has converged off, prove external delivery, and restore the
> org flag in a finally-style cleanup path.
>
> **Revision 14 (2026-08-05)** — the production household canary exposed a
> PostgreSQL RLS interaction that the superuser-backed integration test could
> not reproduce: a grantee can read its active domain grant, but
> `SELECT ... FOR KEY SHARE` also requires the owner-only mutation policy and
> therefore made normal API inbox creation fail closed. New forward migration
> `core/0027_email_tenancy_grant_lock.sql` adds a transaction advisory lock
> keyed by `(org_domain_id, grantee_org_id)` and retains `FOR KEY SHARE`
> through a narrow `SECURITY DEFINER` trigger with schema-qualified `public.*`
> relations, `pg_temp` explicitly last in its fixed `search_path`, a restored-
> in-function RLS bypass, and explicit domain/grantee predicates. The
> compatibility row lock is required because a migration-first rollout may
> still overlap an old revoker that does not take the advisory lock.
> `RevokeOrgDomainGrant` takes the advisory lock before its row lock and
> active-inbox guard. RLS policies are not relaxed. A real non-superuser
> app-role regression, a legacy-revoker overlap, the new transaction ordering,
> GUC restoration and a cross-tenant negative are mandatory. Runtime and
> control-plane schema windows advance to `[27,27]`; deployment rehearsal and
> apply targets include `0027`; rollback refuses while any domain grant exists.
>
> **Revision 13 (2026-08-04)** — Phase 2.1 org tombstoning now treats active
> service tokens as durable tenant resources. `DeleteOrgIfEmpty` refuses while
> an unexpired, unrevoked token exists, and `CreateServiceToken` takes the same
> org reconciliation lock plus active-org check so issuance cannot race a
> tombstone or recreate credentials for a deleted org. Rotation revokes the old
> token and inserts its replacement in that same locked transaction; a failed
> replacement insert rolls the revocation back. The OSS hotfix is
> promoted as a new immutable runtime before the Cloud production rollout.
>
> **Revision 12 (2026-08-04)** — Abrolia Phase 2.1 household email tenancy
> is an approved interleaved prerequisite and owns core migration `0024`
> (Cloud reconciliation owns `cloud/0008`). It adds replay-safe organization,
> root-domain grant, domain, inbox and webhook identities so multiple synthetic
> or pilot households can receive through `abrolia.com` without weakening RLS
> isolation. The already-planned but not-yet-implemented outbound attachment
> migration moves from `0024` to `0025`; feature flags move from `0025` to
> `0026`; the dual-reader compatibility ceiling moves from `0025` to `0026`.
> No implemented `0020`–`0023` migration is renamed, and no outbound-attachment
> scope changes beyond its migration number.
>
> **Revision 11 (2026-08-04)** — Phase 1 safety primitives are implemented
> OSS-first and promoted as immutable runtime `v0.0.7` from merge commit
> `8a3568e`: dial-time public-address enforcement, redirect refusal, bounded
> webhook response draining, aggregate byte budgeting, MCP wire/read limits,
> provider attachment types, and transaction reuse. The Cloud follow-through
> shares one configured 64 MiB budget with its REST attachment path, exports
> limit/used/available gauges, moves `forward_to` and provider downloads onto
> the safe transport, caps the legacy proxy at 10 MiB, and sets a positive
> 30-second request read timeout. The expected automatic sync conflict in
> Cloud's already-diverged `handler_test.go` was resolved by retaining Cloud's
> equivalent cleanup; every exact-mirror path is byte-identical to the OSS
> merge. Runtime `v0.0.7` retains core compatibility `[19,19]`; no production
> deploy or feature activation is part of this promotion.
>
> **Revision 10 (2026-08-04)** — the converged OSS baseline now ends at
> `0019_outbox_created_at.sql`, which restores stable outbox/DLQ creation time
> without overloading the mutable retry schedule. Runtime `v0.0.6` is published
> from OSS commit `5f6dcb2` and declares core `[19,19]`; it supersedes the
> pre-promotion `v0.0.5` artifact with the shared terminal-dedup race and DB
> cleanup review fixes. Cloud pins `v0.0.6`
> and advances its explicit migration predecessor to `0019`. The not-yet-landed
> feature sequence shifts by one as a unit: event expand/relax are
> `0020`/`0021`, blobs/message metadata/outbox attachments are
> `0022`/`0023`/`0024`, feature flags are `0025`, and the 1b compatibility
> window is `[0020,0025]`. The OSS sync workflow now owns the three-list
> manifest and the Cloud CI independently byte-compares every exact mirror.
> The protected branch additionally requires the functional `go-checks`,
> `cloud-e2e`, SDK, dashboard, exact-mirror and security contexts; the legacy
> five template contexts alone are not accepted as evidence of a working gate.
>
> **Revision 9 (2026-08-04)** — Phase 0's startup boundary is now explicit:
> production runtime verifies its OSS-owned core window `[18,18]`; the control
> plane verifies core `[18,18]` and cloud `[7,7]`; local Compose uses bounded
> `apply-to-max`. The ordered deploy gains an explicit `0018`/`0007` migration
> predecessor using temporary Fly Machines that inherit app-scoped DB secrets;
> the DSN is never copied into GitHub. Both target/status commands must exit
> successfully before either verify-only process can start. The scheduled
> reconciler remains migration-free and follows a successfully started control
> plane built from the same image. Phase 0 §7b is also closed: OSS publishes a
> non-expiring GitHub Release manifest and checksum, and Cloud verifies that
> authority against both its lock and release labels on the pinned image.
>
> **Revision 8 (2026-08-04)** — core `0018` is now the OSS-first forward repair for databases
> that recorded version `17` before the tenant-RLS and active-webhook uniqueness corrections
> existed. The attachment/event rollout had not started, so its reserved versions shift together:
> event expand/relax are `0019`/`0020`, blobs/message metadata/outbox attachments are
> `0021`/`0022`/`0023`, and feature flags are `0024`. The 1b compatibility window is therefore
> `[0019,0024]`. No schema step is inserted below an already-recorded Goose version. The
> Cloud's mirror branch was verified before `v0.0.3` publication from OSS commit `8f76b2d`; the
> resulting immutable artifact is pinned atomically in the post-merge Cloud follow-up. Publication
> alone does not deploy, and Phase 0 §7b's published-manifest authority remains outstanding.
>
> **Revision 7 (2026-08-04)** — closes the rollout contradictions found during PR #11 review:
> production gating now consistently describes the enforceable no-human-review gate; Machine image
> gates compare digests for equality instead of ordering hashes; the 1b compatibility window spans
> `0019`–`0024`; additive attachment/flag schema lands before attachment-aware binaries start; and
> every production deploy entrypoint shares one environment lock without reusable-workflow deadlock.
>
> **Revision 6 (2026-08-04)** — **D10**: the staging chain is dropped. Phase 7 rewritten around a
> production-snapshot migration rehearsal and a canary org behind the D8 per-org flag. Phase 0 §1 and
> the production gating are implemented and merged-pending in PR #8.
>
> **Revision 5 (2026-08-03)** — fourth-pass review confirmed D7 and REST delegation, signed off D8's
> direction, and raised 13 new P1 gaps. All verified; none rejected. **D9** added (OSS `store.go` split
> as a backport prerequisite). See [Enhancement History](#enhancement-history).

## Overview

Three consumer-blocking gaps, driven by the hermes-cloud family-assistant pilot (`hermes-cloud/thoughts/shared/plans/2026-08-02-family-ops-assistant-mvp.md`, Phase 3):

1. No reliable inbound push: `email.received` never reaches the signed, retried org-webhook channel.
2. Inbound attachment metadata is dropped at ingest, the proxy is unreachable with a runtime-scoped key, and the bytes stop being fetchable after Resend's 30-day retention.
3. Outbound attachments are unsupported end-to-end.

Two prerequisites dominate the schedule. **The pipeline is broken on `main`**: `verify_runtime_lock.sh` fails on a clean tree, `go build ./...` fails on a missing `internal/jmap`, `cloud_deploy.sh:26` calls a nonexistent `cmd/neuralmail`, both deploy workflows race on `push: main`, no image contains `nerve-reconcile`, and the SDK publish workflow rebuilds rather than promotes. And **the OSS/Cloud split has diverged structurally**: nerve-oss has no `internal/webhooks` package, no `store/org_webhooks.go`, and still carries a monolithic `store.go` holding 14 methods that Cloud has since extracted into `store_threads.go` — so a byte-identical backport does not compile.

## Current State Analysis

### Webhook delivery path

- `org_webhook_deliveries.outbox_event_id uuid NOT NULL REFERENCES outbox_events(id)` (`migrations/core/0017_org_webhooks.sql:48`), unique `(webhook_id, outbox_event_id)` (:73).
- **The claim query cannot tolerate a NULL FK.** `ClaimWebhookDeliveries` selects `d.outbox_event_id::text` (`store/org_webhooks.go:286`) into `&d.OutboxEventID`, a plain `string` (:33). One NULL aborts the *entire* claim batch (:299) — poisoning outbound delivery too — and the same statement already flipped those rows to `delivering` with `locked_at`, so they re-poison every 5 minutes.
- **Wildcards mean "all events"**: `cardinality(events) = 0 OR $2 = ANY(events)` (:217), default `ARRAY[]::text[]` (`0017:17`).
- **No `events` allowlist exists** (`cloudapi/handler_webhooks.go:89,109-112`), so the DB may already hold `events=['email.received']` on an `http://` endpoint, which D1 would read as consent.
- **The dispatcher is an authenticated SSRF primitive** (`webhooks/dispatcher.go:87-89`): stock client, timeout only, no dial-time filtering, default redirect policy.
- **`forward_to` is a second, worse SSRF path**: `postWebhookForward` POSTs the **full message body** to any operator-supplied URL via a bare client (`resend_webhook.go:311,375-427,402`).

### Ingest path

- **`RunAsOrg` owns a transaction**: dedicated `conn`, `BeginTx`, **transaction-local** `app.cloud_mode`/`app.current_org_id` GUCs, callback gets a `*Store` whose `q` is the `*sql.Tx` (`store/store.go:194-221`). A nested `BEGIN` lands on a different connection with no tenant GUCs — RLS denies every row.
- **OSS calls the same methods with no transaction**: `Service.SendReply` → `st.EnqueueOutboxMessage` on a plain `*sql.DB` (`nerve-oss/internal/tools/service.go:359`).
- **Partial-recipient success permanently drops mail** (`resend_webhook.go:192`, then `processed` at :149, short-circuit at :114).
- **No reconciler is scheduled** — `cmd/nerve-reconcile` appears only in CI's vet/test lists (`ci.yml:20,22`), **and no image contains the binary**: `deploy/cloud/Dockerfile.control-plane` builds only `/app/nerve-control-plane`.
- **Ingest runs inside Resend's ~5s webhook timeout** (`providers/resend/resend_receiving.go:52-55`).

### Attachments

- Metadata parsed, never persisted (`resend_receiving.go:31-37`). `download_url` is valid 1 hour and refreshable (:102), but Free/Pro/Scale carry **30-day retention**; past that `GetAttachment` 404s and the proxy returns `410` (`handler_messages.go:88-90`).
- **Past retention the attachment list is unrecoverable** — `GetReceivedEmail` is the only source of IDs and filenames.
- **Discovery is locked out**: `handleInboxThreads` requires `nerve:admin.billing`/`nerve:email.inbox.create` (`handler_inboxes.go:335`), same as the proxy (:43), while `docs/TENANT_GUIDE.md:37` promises `nerve:email.read`.
- **Proxy egress is unbounded**: `http.DefaultClient.Do` (:107), no timeout, follows redirects, unbounded `io.Copy` (:131).
- **`requireAnyScope` collapses 401 into 403** (`handler_auth.go:25-42`).
- **Legacy `attachments`**: nullable `message_id`, no `org_id`, no RLS (`0001_init.sql:64-70`), unreferenced by Go code.

### Outbound

- No attachment fields on `OutboundMessage` (`emailtransport/provider.go:34-45`) or `outbox_messages` (`0006_outbox.sql`).
- **`emailtransport` imports `store`** (`provider.go:7`, `outbox_worker.go:12`); `store` imports nothing back. An attachment type declared in `emailtransport` and consumed by `store` is an **import cycle**.
- **Content-dedup collapses attachment variants**: the `existing` CTE matches `content_hash` among `queued`/`sending` **before** the idempotency key is read (`store/outbox.go:158-176`); `contentHash` covers only body fields (:101-111).
- **Terminal outbox rows are the DLQ.** `MarkOutboxMessageSent` leaves rows in place (:277-290). `handleDLQ` lists **failed rows only**; `handleDLQByID` routes `/{id}`, `/{id}/events`, `/{id}/replay` (`cloudapi/handler_dlq.go:16,59-66`, registered at `handler.go:91-92`), backed by `store/outbox.go:576,602,644`. `chk_outbox_status` permits only `queued|sending|sent|failed` (`0006_outbox.sql:22`). There is no `terminal_at` column.
- **MCP has no body-size limit** (`nerve-oss/internal/mcp/server.go:69`); no `MaxBytesReader` anywhere in `nerve-oss/internal`.
- **Drafts have no identity or storage** (`nerve-oss/internal/tools/service.go:249-283,285`).

### Memory envelope

Both apps: `shared-cpu-1x` / **512 MB**, `http_service.concurrency hard_limit = 250` (`fly.runtime.toml`, `fly.control-plane.toml`). A 16 MB per-request cap admits up to **4 GB** in flight. Both servers set **only `ReadHeaderTimeout: 5s`** (`cmd/nerve-control-plane/main.go:214-217`, `nerve-oss/internal/app/app.go:155-158`) — no `ReadTimeout`, so a slow body has no deadline. `Content-Length` is client-supplied and untrusted. `fly.runtime.toml` sets `min_machines_running = 0`.

### Repo topology, deploy, and gates

- **Ownership on paper**: nerve-oss owns runtime images, MCP contract, core migrations (`docs/REPO_SPLIT_RUNBOOK.md:5-6`).
- **The divergence is structural, not just missing files.** OSS `migrations/core` stops at `0010`. OSS `internal/store/` lacks 14 cloud files including **`org_webhooks.go`**; OSS has **no `internal/webhooks` package**. Conversely **OSS has `internal/jmap`, which cloud lacks** — the root cause of the broken build, since the sync path list carries `internal/emailtransport/**` (which delivered `providers/jmap`) but not its dependency. And **OSS's `store.go` still contains the 14 methods Cloud extracted into `store_threads.go`** — `ListThreads`, `GetThread`, `GetThreadInboxID`, `GetMessage`, `SearchInboxFTS`, `UpsertThread`, `InsertMessage`, `EnsureThread`, `UpdateThreadSignals`, `InsertMessageWithThread`, `MessageCount`, `RecordToolCall`, `RecordAudit`, `ListAudit` (`nerve-oss/internal/store/store.go:203-428` vs `internal/store/store_threads.go:13-247`) — so copying that file in produces duplicate method declarations.
- **The sync allowlist is a hand-maintained file list** (`sync-to-cloud.yml:31-40,66-77`) driven by `git diff HEAD~1 HEAD` and three `paths:` filters; conflicts are non-fatal (`::warning::`, :78-80); nothing builds or tests cloud after applying.
- **Shared files have already diverged**: OSS `contentHash` is 3-arg (`nerve-oss/internal/store/outbox.go:40`), Cloud's is 4-arg.
- **`docs/MCP_Contract.md` differs between repos**, yet `verify_runtime_lock.sh` hashes the Cloud copy and Cloud's core tree against a lock pinning an OSS-built image (:21-40, `generate_runtime_manifest.sh:9-10`).
- **`RUNTIME_IMAGE=ghcr.io/dsmolchanov/nerve-runtime:v0.0.1`** — a **mutable tag**, not a digest.
- **`verify_runtime_lock.sh` fails on `main` right now** (verified by running it).
- **`cloud_deploy.sh` cannot complete** (`:26` calls `cmd/neuralmail`; `:17` migrates from an image whose core tree ends at `0010`). In practice only `store.Migrate` at control-plane startup (`main.go:50`) has ever applied `0011`–`0017` — and it applies **all** pending goose migrations unconditionally.
- **The deploy workflows race**: both on `push: main`, no `needs:`, no `environment:`. `control-plane-deploy.yml` fires on `internal/**` and **rebuilds the image per target**; `runtime-deploy.yml` fires on `runtime.lock`, so touching the lock is the deploy trigger.
- **`publish-python-sdk.yml` rebuilds the wheel** with `python -m build` at publish time — it promotes nothing.
- **No migrate CLI**; goose is the driver (`store/migrate.go:11`).
- **CI omits** `./internal/store`, `./internal/cloudapi`, `./internal/webhooks`, `./internal/emailtransport`; no Python job.
- **`go build ./...` fails today** (`providers/jmap/jmap_inbound.go:7`).
- **DB tests read `NM_TEST_DB_DSN`**, and `t.Skipf` when unreachable (`webhooks/dispatcher_integration_test.go:29-42`).
- **`sdk/python`**: no `[project.optional-dependencies]`; four disagreeing version sources (`pyproject.toml:3`=`0.1.2`, `__init__.py:25`=`0.1.1`, `client.py:42`=`0.1.2`, `tests/test_client.py:41` asserts `0.1.1`). `NerveClient.close()` closes only `self._http` (:382-384). `__init__.__all__` exports 8 names, none of them a REST client (`__init__.py:14-23`).
- **Fly scheduled Machines** are created with `fly machine run|create --schedule`, not a `fly.toml` key; a scheduled Machine cannot be started manually. **`fly secrets set` restarts Machines.**

## Key Decisions

| # | Question | Decision |
|---|---|---|
| D1 | Do `events=[]` wildcards receive `email.received`? | **Explicit opt-in**, with a DB preflight; non-HTTPS rows already carrying it are disabled, not grandfathered. |
| D2 | How do inbound bytes survive 30-day retention? | **Mirror into our own store**, asynchronously (the ~5s ingest budget forbids inline). |
| D3 | `draft_reply(attachments=)` in 0.2.0? | **Dropped**; parameter removed, not ignored. |
| D4 | Outbound storage shape? | **Content-addressed blob store**, refcounted, quota'd, GC'd — shared with D2. |
| D5 | Where does attachment persistence live? | **OSS-first**, after a baseline backport — which requires D9 first. |
| D6 | How much of the broken pipeline does this plan fix? | **All of it**, with workflow de-triggering as the **first commit**. |
| D7 | Retention for terminal outbox rows | **Release bytes, keep history.** No deletion. `sent` releases blob refs 90 days (configurable) after `terminal_at`; `failed` retains until explicit abandon. Owner-signed-off. |
| D8 | How is activation performed? | **DB-backed org-scoped flags** in **core** schema (the runtime consumes them), CLI writer, fail-closed cache, tri-state env override. Direction signed off; contract specified in Phase 7 §3. |
| D9 | How does the OSS baseline converge? | **Split OSS `store.go` to mirror Cloud's file layout first** — a mechanical, behaviour-preserving extraction PR in nerve-oss, landed before any backport, so the shared set can be byte-identically mirrored and the layouts stop diverging. |
| D10 | Is there a staging chain? | **No.** There is no staging infrastructure, and standing it up would not have tested the risky parts: it cannot exercise the backfill (no production-shaped inbound rows, no Resend envelopes), it is worse than CI at migration sequencing, and the contract suite needs a real Resend account, verified domain and external mailbox. Replaced by three mechanisms that each do the job better — a **production-snapshot migration rehearsal**, a **canary org in production** behind the D8 per-org flag, and the CI integration suite. Costs one verified subdomain instead of two Fly apps, a second Postgres and a second domain. |

## Desired End State

An org can subscribe a webhook to `email.received` and get signed, retried, deduplicated notifications, with no pre-existing subscription silently opted in and no cleartext endpoint receiving PII. A runtime key holding `nerve:email.read` can list threads, see attachments with an explicit availability state, and download bytes from our own store regardless of Resend retention. `compose_email`/`send_reply` accept attachments delivered via Resend or SMTP, deduplicated on a fingerprint including the files. Every gate runs, in a pipeline that deploys pinned artifacts in a defined order only after the protected-branch checks pass and the operator explicitly confirms production intent. This repository tier provides no second-person approval gate, and the plan does not claim one.

## What We're NOT Doing

- No refactor of the outbound `outbox_events` fan-out (byte-compatible; regression-tested).
- No removal of `forward_to` — it moves onto the safe HTTP transport.
- No durable drafts (D3).
- **No deletion of outbox rows, delivery events, webhook attempts, DLQ entries or idempotency tombstones** (D7).
- No change to `chk_outbox_status` — a new status value would break DLQ queries filtering on `failed`.
- No metering changes; storage quota is an abuse limit.
- No object-storage service; Postgres `bytea` behind a narrow, swappable interface.
- **No behaviour change in the D9 split** — pure code movement, verified by API-surface diff.
- **No staging environment** (D10). No staging Fly apps, no second Postgres, no second verified domain, no `cloud-staging` chain. Replaced by a snapshot rehearsal and a canary org.

## Implementation Approach

Nine phases. Ordering constraints:

1. **De-trigger the deploy workflows in the first commit** (D6) — until then, merging anything under `internal/**` deploys production.
2. **D9 split, then baseline backport, then pipeline repair** — nothing shared can land OSS-first until OSS compiles with the shared set.
3. **Schema rollout is expand → dual-read → relax → activate**, with **version-targeted** migrations, because a single `up` would apply the relax step alongside the expand step and reintroduce the batch-poisoning outage.
4. **Build every artifact once and promote it by digest** — image, wheel, runtime pin.

Chain: **de-trigger + D9 split + backport + pipeline (0) → shared primitives (1) → inbound fan-out (2) → attachment storage + mirror (3) → outbound contract (4) → cloud integration (5) → SDK 0.2.0 (6) → sequenced release (7) → post-release (8).**

---

## Phase 0: De-trigger, converge, repair

### 1. FIRST COMMIT — de-trigger the deploy workflows

`control-plane-deploy.yml` fires on `push: main` with `internal/**`; `runtime-deploy.yml` on `deploy/cloud/runtime.lock`. Every subsequent commit touches one or both.

- Remove `push:` from both; convert to `workflow_call` with `target_env` and pinned-artifact inputs.
- **`workflow_dispatch` is not left as a bypass.** Every dispatch targets `cloud-production`, whose deployment branch policy accepts only protected branches; it refuses to run without both `confirm=PRODUCTION` and an explicit artifact digest. Required reviewers are unavailable on this private-repository tier, so this is deliberately a CI-backed, single-operator gate rather than a claimed human-approval gate.
- **One environment-scoped concurrency lock covers every entrypoint.** The
  top-level `deploy.yml` owns `deploy-<environment>` for the whole ordered
  chain; direct runtime/control-plane dispatches use the same group and queue
  behind it. Reusable child workflows use a run-local group only when an
  explicit `caller_holds_deploy_lock=true` input proves their parent owns the
  shared lock — otherwise giving parent and child the same group would
  deadlock while the parent waits for the child.
- Land `deploy.yml` (Phase 7 §1) as a stub calling them in order.

Standalone PR, merged before anything else.

### 2. Unbreak the build — `internal/jmap`

`providers/jmap/jmap_inbound.go:7` imports the absent `neuralmail/internal/jmap`. The package exists in nerve-oss (`internal/jmap/{client,ingestor,jmap_client}.go`); the sync list carries `internal/emailtransport/**` but not this dependency. Fix via a **full bootstrap copy** (§4), not a `HEAD~1` diff — cloud has none of the files, so an incremental sync would copy nothing. If the JMAP provider is not wired into `cmd/nerve-control-plane`, delete `providers/jmap` from cloud instead and record the decision. No exclusion lists.

`go vet ./...` must also pass before §8 can adopt `./...`, and one failure stands in the way: `Metrics.WriteTo(io.Writer)` (`internal/observability/metrics.go`) takes the `io.WriterTo` name without its `(int64, error)` return, so a type assertion to that interface silently misses. **Rename it to `Metrics.Render`** — two call sites, both in-repo, in a package not on the sync path. It ships with this section because §8's gate depends on it.

### 3. D9 — split OSS `store.go` (own PR, no behaviour change)

Cloud's `store_threads.go:13-247` declares 14 methods that OSS still holds in `store.go:203-428`. Byte-identical mirroring is impossible until the layouts match.

- Extract from `nerve-oss/internal/store/store.go` into files mirroring Cloud: `store_threads.go` (the 14 methods above), plus any others the file-by-file comparison shows.
- **Pure movement.** Verified by an API-surface diff: the exported method set of `package store` before and after must be identical (`go doc -all ./internal/store` compared across the commit), and `go test ./...` green with no test changes.
- Then run the same comparison for `listing.go` and the webhook code, which
  also depend on Cloud-only struct fields and outbox APIs. **Enumerate the
  missing dependencies explicitly in the PR**; keep D9 pure movement and land
  the enumerated backport in §4, rather than discovering dependencies midway
  through that change.

### 4. Baseline backport + a sync manifest that expresses ownership

The current list is one flat set. Ownership is three-valued, and conflating them is why `store.go` (which must carry `inTx`) and `internal/cloudapi/**` (several handlers of which are cloud-only) cannot both be "synced".

`sync-manifest.yaml` in nerve-oss, with three lists:

| List | Semantics | Contents |
|---|---|---|
| **exact-mirror** | byte-identical, CI-asserted in both repos | `internal/store/migrations/core/**`, `internal/store/{store,store_threads,outbox,tool_idempotency,migrate,org_domains,org_webhooks,org_events,webhook_events,listing,outbox_listen,inbox_smtp_config}.go` + tests, `internal/emailtransport/**`, `internal/jmap/**`, `internal/webhooks/**`, `internal/httpsafe/**`, `internal/memguard/**`, `docs/MCP_Contract.md` |
| **patch-synced** | 3-way patch, divergence permitted | `internal/cloudapi/**` except the cloud-only list |
| **cloud-only** | never synced, never asserted | `internal/cloudapi/{handler_messages,handler_inboxes,handler_webhooks,handler_dlq}.go`, `internal/store/{store_billing,store_orgs,store_tokens,store_usage}.go`, `internal/attachments/**`, `cmd/**` |

`store.go` is **exact-mirror** — without it Cloud never receives `inTx` (Phase 1 §4). `internal/attachments/**` is cloud-only: the mirror worker is a control-plane concern built on shared store methods.

Backport into nerve-oss (after D9): `migrations/core/0011..0017*.sql`, then the
OSS-owned forward repair `0018_repair_tenant_isolation.sql`,
`store/org_webhooks.go` + test, `internal/webhooks/**`, and the transitive store
files named above. The dedicated Cloud mirror plan is
`2026-08-04-cloud-mirror-core-repair-0018.md`.

Workflow changes:
- **Triggers are computed from the manifest**, not three hardcoded `paths:` filters — a new exact-mirror path must not require editing the trigger separately.
- **Changed-file detection handles bootstrap**: a manifest path absent in the destination is copied wholesale, not diffed against `HEAD~1`.
- **Conflicts are fatal** — `git apply --3way` failure exits non-zero and no PR opens.
- **After applying, build and test cloud** (`go build ./... && go test ./...`) inside the sync job.
- **Both repos assert** every exact-mirror path is byte-identical; drift fails CI.

**Gate**: nerve-oss `go build ./... && go test ./...` green, `MigrateCore`
applies `0001`–`0018` on a fresh DB, `0018` repairs a simulated legacy
version-17 database without partial changes on refusal, and both repos'
exact-mirror sets match. `0019_outbox_created_at.sql` is the baseline head;
feature migrations begin at `0020`. Because the `0018` repair makes duplicate
active `(org_id, url)` a live
write-side error, the Cloud-only create-webhook boundary maps PostgreSQL
`23505` to `409 Conflict`; a DB-backed repeated-POST test prevents regression
to the previous `500` response.

### 5. Migrate CLI with target versions

```
nerve-migrate up     [--scope core|cloud|all] [--to <version>]
nerve-migrate down   --scope core --steps 1
nerve-migrate status [--scope ...]
```

`--to` is essential: first stop at the converged baseline `0019`; the four-step
feature rollout then requires stopping at expand `0020` before the dual reader
ships. `store/migrate.go` gains `MigrateUpToCore/Cloud` over
`goose.UpToContext` and `MigrateDownCore/Cloud` over `goose.DownContext`. On
the exact-mirror list — land OSS-first.

**Implementation sequencing (2026-08-04):** target/down/current primitives,
the dedicated CLI, core repair `0018`, bounded startup, and the explicit
`0018`/`0007` migration predecessor landed first. The D9/baseline convergence
then added core `0019_outbox_created_at.sql`; the shared review hotfix published
digest-pinned runtime `v0.0.6` from OSS commit `5f6dcb2`. Cloud advances the predecessor and compiled
core window to `0019` in the same sync PR. The runtime intentionally ignores
Cloud-owned schema versions, while the control plane verifies both scopes.

**Startup migration becomes bounded.** `store.Migrate` at `cmd/nerve-control-plane/main.go:50` currently applies *all* pending migrations, so deploying the dual reader would itself apply `0021` and reintroduce the outage. Replace with `NM_MIGRATE_ON_START`:

- `verify` (**new default in cloud**): assert the applied version is within `[minRequired, maxSupported]` compiled into the binary; refuse to start outside that window, and **never apply**.
- `apply-to-max`: apply only up to `maxSupported` — for OSS-local and dev.
- `off`.

`minRequired`/`maxSupported` are constants per binary, which also gives the rollback floor in Phase 7 §5 for free.

### 6. Fix `scripts/deploy/cloud_deploy.sh`

`run_cloud_migrations` → `go run ./cmd/nerve-migrate up --scope cloud --to 0007`. `run_core_migrations` keeps the image-based invocation with `--to 0019` plus an assertion that `nerve-migrate status --scope core` reports the exact target and zero pending.

Run the core command through the pinned image's explicit
`/app/nerve-migrate` entrypoint (the image default is `/app/nerve-runtime`),
then extend `verify_cloud_deploy_order.sh` to require both real CLI invocations,
the zero-pending assertion, and the absence of the nonexistent
`./cmd/neuralmail` path.

### 7. Lock authority

Split, because the two halves have different owners and only one is reachable from this repo.

#### 7a. Bind the lock to the artifact (nerve-cloud, now)

- **Pin by digest**: `RUNTIME_IMAGE=ghcr.io/dsmolchanov/nerve-runtime@sha256:<digest>` plus a readable `RUNTIME_VERSION`. A tag pin names a mutable reference; the same tag can be re-pushed to a different build.
- **Read the image's OCI labels back from the registry** and assert them against the lock:

  | Label | Lock field |
  |---|---|
  | `org.opencontainers.image.version` | `RUNTIME_VERSION` |
  | `org.opencontainers.image.source` | `RUNTIME_SOURCE_REPO` (new) |
  | `org.opencontainers.image.revision` | `RUNTIME_SOURCE_REVISION` (new) |

  So the pin resolves to an immutable artifact *and* that artifact must be an image built from a recorded nerve-oss commit — not merely a digest that exists.
- **No skip path.** Authenticate through ghcr.io's token endpoint. Every CI and
  deploy caller grants `packages: read` (or stronger), explicitly exports its
  `GITHUB_TOKEN`, and the verifier rejects a credential-less CI invocation;
  local checks may use `GHCR_TOKEN`/`GITHUB_TOKEN`, while anonymous access is
  sufficient only for a public package. Note it is the token endpoint, **not**
  `Authorization: Bearer base64(PAT)`.
- Add `CLOUD_SCHEMA_HASH` for `migrations/cloud`, which nothing hashes today.
- Bind every deployable control-plane/runtime digest to its compiled
  `[minRequired,maxSupported]` schema window in the release manifest (Phase 7
  §5). Digests are opaque identities, not ordered versions: Machine gates use
  exact expected-digest equality (or an explicit allowlist), while rollback
  safety uses the declared compatibility window.
- **Regenerate and commit the lock** so `main` is green (after §1, since touching the lock is a deploy trigger).

#### 7b. Publish and consume the OSS manifest

OSS release CI publishes `runtime-manifest.json` and its checksum as GitHub
Release assets as well as a short-lived Actions artifact. The manifest declares
the MCP hash, core hash, and compiled core compatibility window. Cloud's
`verify_runtime_lock.sh` downloads the release asset instead of regenerating a
different manifest from its working tree, verifies the local mirrored core tree
against it, and separately verifies the Cloud-owned migration hash. Matching
`io.nerve.runtime.*` image labels bind every manifest field back to the pinned
GHCR digest. GitHub Release assets are required because Actions artifacts expire
and cannot serve as production lock authority.

### 8. CI coverage

- Remove the permanently failing `dependency-review` job while this remains a
  private, user-owned repository without GitHub Code Security/GHAS. The action
  has no degraded scanning mode: it returns `403` before inspecting the diff on
  every PR. Do not use `continue-on-error`, which would later hide real findings;
  retain `govulncheck`, and re-enable Dependency Review if the repository becomes
  public or gains the required entitlement.
- `postgres:16` service; set **`NM_TEST_DB_DSN`** (not `DATABASE_URL`, which no test reads) and `NM_REQUIRE_DB=1`, converting `t.Skipf("postgres unavailable")` (`dispatcher_integration_test.go:41`) to `t.Fatalf`. A silently-skipped suite reporting green is worse than a failing one.
- `./...` on vet and test (possible once §2 lands).
- `[project.optional-dependencies] dev` in `sdk/python/pyproject.toml`, then an `sdk-python` job. Scoped to `pytest`: the current suite drives coroutines with `asyncio.run()` rather than `pytest-asyncio` markers, so async/mocking plugins arrive in Phase 6 with the tests that need them.
- **`-count=1` on the test run.** Go otherwise replays a cached `ok` without running anything, which satisfies the skip gate below on a run where the database was never reachable.
- **A skip gate.** A skipped test is indistinguishable from a passing one in the package summary — `ok neuralmail/internal/webhooks` prints whether the integration test ran or bailed on an unreachable database. Every `t.Skip` here is a DB-availability bail, so the job fails when `go test -v` output contains `--- SKIP`. This supersedes revision 4's `NM_REQUIRE_DB`: it needs no test-file edits (several sit in the synced `internal/cloudapi/**`) and catches any future skip, not only DB ones.
- **A cleanup gate.** DB helpers keep their admin connection open until the
  registered database-drop cleanup runs. The prior `defer adminDB.Close()` ran
  first and silently left every `nerve_cloud_*`, `nerve_wh_*`, `nerve_bill_*`,
  and `nerve_rec_*` database behind. Apply the same ordering to all four
  helpers; the Cloud webhook helper is preserved unchanged when that
  currently-absent package is later backported into the planned exact-mirror
  set. After the Go suite, CI queries `pg_database` and `pg_roles` and fails if
  any named test database or RLS application role remains.

**Expect pre-existing failures.** `./internal/cloudapi` has never run in CI and three of its tests fail against a real database. All three are test bugs, fixed here — a gate that ships red is not a gate:

| Test | Defect |
|---|---|
| `TestCreateOrgProvisionsTrial` | `AddDate` preserves wall-clock time, so a 90-day span crossing a DST transition is an hour off the interval Postgres computed in its UTC session. Latent in CI (UTC), real locally. Compute in UTC. |
| `TestListPlansExcludesTrialAndReturnsEntitlements` | `0007_seed_plan_tiers.sql` already seeds these plan codes, so the `INSERT` violates `plan_entitlements_plan_code_key`. Upsert, and assert intent rather than a row count coupled to what a migration seeds. |
| `TestOrgDomainsCreateListDNSVerifyAndDelete` | Cloud's `internal/domains` emits DMARC and SPF records beside the ownership TXT while the assertion expects exactly one. The test is byte-identical across repos but `internal/domains` is not synced and has diverged. Assert the ownership record is present. |

Run the suite under both `TZ=UTC` and a DST-observing zone; the first defect only reproduces outside UTC.

### 9. Reconcile image and Machine

`Dockerfile.control-plane` builds only `/app/nerve-control-plane` — `ensure_reconcile_machine.sh <image>` has no image to name.

- **Build a multi-binary control-plane image**: add `go build -o /out/nerve-reconcile ./cmd/nerve-reconcile` and `COPY` it alongside. One image, one digest, promoted once; the Machine overrides the command to `/app/nerve-reconcile`. (A separate `Dockerfile.reconcile` is the alternative; the multi-binary image is chosen so one digest covers the control plane and the reconcile Machine.)
- **The scheduled entrypoint never migrates.** `cmd/nerve-reconcile` removes its
  unconditional `store.Migrate`; target-version migration jobs own schema
  movement, and the reconciler fails naturally if its required schema is not
  present rather than bypassing the rollout sequence on its next hourly run.
- `scripts/deploy/ensure_reconcile_machine.sh`: idempotent create-or-update keyed on a `role=reconcile` metadata label. Absent → `fly machine run <digest> --entrypoint /app/nerve-reconcile --schedule hourly --region iad --vm-memory 512 --metadata role=reconcile --restart no`; present with a different digest or schedule → update. The explicit entrypoint override is required: a positional command changes Docker `CMD` but would leave `/app/nerve-control-plane` as the image entrypoint, while `flyctl machine run --command` applies only with `--shell`. Create and update also clear `init.exec`, because a leftover exec override takes precedence over both entrypoint and command. Called from `deploy.yml`, never by hand.
- Post-conditions: exactly one `role=reconcile` Machine, expected digest, `schedule=hourly`, restart policy `no` (a scheduled job must not restart-loop; the next cycle retries).
- **Advisory lock on a pinned connection.** `pg_try_advisory_lock` is session-scoped; taken through a `*sql.DB` pool it lands on an arbitrary connection that may be returned to the pool mid-run, so the lock protects nothing. Use `pg_advisory_xact_lock` inside a transaction spanning the run, or acquire a dedicated `*sql.Conn` and hold it for the whole run. Spec: dedicated `*sql.Conn` + `pg_try_advisory_lock`, released explicitly in a `defer`, with the conn never returned to the pool until the run ends.
- Emit `Report` counters as last-run Prometheus gauges on stdout; the
  reconciler is a one-shot process, so an in-memory cumulative counter would
  reset before it could be scraped. Include a `skipped_concurrent` gauge so a
  lock-contention no-op remains observable.

### 10. Runbook

Record D9, the three-list manifest and its ownership boundary, the digest-pinned artifact authority, bounded startup migration, D7 retention, D8 flags, and the promotion sequence.

### Success Criteria

#### Automated:
- [ ] Neither deploy workflow has `push`; a commit touching `internal/**` triggers no deploy; every `workflow_dispatch` rejects a missing digest or missing `confirm=PRODUCTION` before any deploy and is restricted to protected branches (implemented on PR #8, pending green review/merge)
- [ ] A direct runtime or control-plane dispatch queues behind an in-flight top-level production deploy; the top-level reusable children do not deadlock on their parent's lock
- [ ] `go build ./...` and `go vet ./...` succeed in nerve-cloud
- [ ] **D9**: `go doc -all ./internal/store` is byte-identical before and after the split; `go test ./...` green with zero test-file changes
- [ ] nerve-oss `go build ./... && go test ./...` green after backport; `MigrateCore` applies `0001`–`0019` on a fresh DB; the legacy-version-17 repair and stable-outbox-created-time tests are green; every exact-mirror path is byte-identical across repos
- [ ] Repeating `POST /v1/webhooks` for one active `(org_id, url)` returns `409`, not `500`, and persists only one subscription
- [ ] Sync CI: an un-synced manifest path **fails**; a conflicting patch **fails the job**; the post-apply cloud build+test runs and can fail the job; a manifest path absent in cloud is bootstrapped wholesale
- [ ] `nerve-migrate up --to 0019` establishes the converged baseline with `0020` pending; `up --to 0020` stops at feature expand with `0021` still pending; `--to` is honoured per scope
- [ ] With `NM_MIGRATE_ON_START=verify`, a binary whose `maxSupported` is below the DB version **refuses to start**; one within the window starts and applies nothing
- [ ] The Cloud mirror and lock advance atomically to immutable OSS runtime `v0.0.6`, core hash/window `[19,19]`, and exact-mirror CI passes independently in Cloud
- [ ] `verify_runtime_lock.sh` **passes on `main`**, and fails when the manifest digest does not match `RUNTIME_IMAGE`
- [ ] The control-plane image contains both `/app/nerve-control-plane` and `/app/nerve-reconcile`
- [ ] `ensure_reconcile_machine.sh` is idempotent; changing the digest updates rather than duplicates
- [ ] Two concurrent reconcile runs → exactly one repair pass; the lock is held on a dedicated connection for the run's full duration (asserted via `pg_locks`)
- [ ] CI runs the four previously-omitted packages; with `NM_REQUIRE_DB=1` and Postgres stopped, DB-backed tests **fail rather than skip**
- [ ] A full DB-backed suite leaves zero `nerve_test_*`, `nerve_cloud_*`,
  `nerve_wh_*`, `nerve_bill_*`, and `nerve_rec_*` databases and zero
  `rls_app_*` roles behind
- [ ] Security CI retains `govulncheck` and does not advertise an unsupported Dependency Review job; re-enabling it requires a successful entitlement probe
- [ ] `pip install -e 'sdk/python[dev]' && pytest sdk/python` green

#### Manual:
- [ ] A deliberately broken store test fails the PR
- [ ] `fly machine list` shows the scheduled reconcile Machine at the expected digest; one cycle has executed

---

## Phase 1: Shared safety primitives (nerve-oss → sync)

### 1. `internal/httpsafe`

`*http.Client` with a `net.Dialer.Control` hook rejecting loopback, RFC1918, link-local (`169.254/16`, `fe80::/10`), unique-local (`fc00::/7`), unspecified and multicast at **dial** time (surviving DNS rebinding); `CheckRedirect` → `http.ErrUseLastResponse`; required explicit timeout; optional host allowlist.

Adopted by the dispatcher (`webhooks/dispatcher.go:87-89`, plus `io.CopyN(io.Discard, resp.Body, 64<<10)` at :204), **`postWebhookForward`** (`resend_webhook.go:402`), and the Phase 3 mirror worker.

### 2. `internal/memguard` — aggregate byte budget across **every** large consumer

At `hard_limit = 250` against **512 MB**, a 16 MB per-request cap admits ~4 GB. Revision 4 guarded only MCP ingress and the mirror worker, missing the two largest consumers.

A byte-denominated semaphore, `Acquire(ctx, n) (release func(), err error)`, applied at four sites:

| Site | Reservation |
|---|---|
| **MCP ingress** (`nerve-oss/internal/mcp/server.go`) | Reserve **the cap**, not `Content-Length` — the header is client-supplied and a low value would let an oversized body slip past the budget. Reserve `min(declared, cap)` up front and **true up incrementally** as `MaxBytesReader` yields bytes, aborting with `503` if the budget is exhausted mid-read. |
| **REST attachment download** (`handler_messages.go`) | Acquire `size_bytes` **before** `SELECT content` — a `bytea` read materialises the whole blob in Go memory — and hold until the response is fully written. Exhausted → `503` + `Retry-After`. |
| **Outbox worker** | **Lazy-load one message's attachments at a time.** A claim batch of 10 messages × 10 MB would otherwise be 100 MB resident before the first send. Attachments are loaded immediately before the provider call and released immediately after. |
| **Mirror worker** | `mirrorConcurrency` (default 3) *and* the byte budget: 3 × 10 MB worst case. |

**Slow bodies must not pin the budget.** Both servers set only `ReadHeaderTimeout: 5s` (`main.go:214-217`, `nerve-oss/internal/app/app.go:155-158`). Add `ReadTimeout` and, for attachment-bearing requests, an explicit `SetReadDeadline` on the body, so a reservation cannot be held indefinitely by a trickling client.

Budgets are config values and exported metrics. VM sizing is re-evaluated at the Phase 7 rehearsal rather than assumed adequate.

### 3. `store.OutboundAttachment` — declared in `store`

`emailtransport` already imports `store` (`provider.go:7`, `outbox_worker.go:12`) and `store` imports nothing back, so declaring it in `emailtransport` and consuming it from `store.EnqueueOutboxMessage` is an import cycle.

```go
// package store
type OutboundAttachment struct {
    Filename, ContentType, SHA256 string
    Content []byte // loaded at send time; never carried on the queue row
}
```

`emailtransport.OutboundMessage` gains `Attachments []store.OutboundAttachment`.

### 4. `Store.withTx`

Cloud calls store methods inside `RunAsOrg`'s transaction; OSS-local calls them bare (`nerve-oss/internal/tools/service.go:359`). "The caller owns the transaction" is not a contract OSS satisfies.

```go
// Store gains an unexported `inTx bool`, set true by RunAsOrg.
//
// withTx runs fn atomically. If this Store is already transaction-scoped it
// runs fn inline — a nested transaction would land on a different connection
// without the tenant GUCs RunAsOrg sets (store.go:194-221), and RLS would deny
// every row. Otherwise it begins and commits its own transaction.
func (s *Store) withTx(ctx context.Context, fn func(*Store) error) error
```

`store.go` is on the exact-mirror list precisely so `inTx` reaches Cloud.

### Success Criteria

#### Automated:
- [ ] `httpsafe` refuses `127.0.0.1`, `169.254.169.254`, `[::1]`, a name resolving into 10/8, a rebind host public at validation and private at dial, and a 302 into link-local; both the dispatcher and `postWebhookForward` use it (asserted by construction)
- [ ] `memguard`: a request declaring `Content-Length: 1` but sending 16 MB is charged the real bytes and rejected when the budget is exhausted mid-read
- [ ] A REST download acquires before `SELECT content`; concurrent downloads exceeding the budget get `503` rather than OOM
- [ ] The outbox worker never holds more than one message's attachments (asserted on peak resident bytes across a 10-message batch)
- [ ] A trickling client cannot hold a reservation past `ReadTimeout`
- [ ] Budget returns to zero after success, error and timeout paths (leak test)
- [ ] `go build ./...` succeeds with `OutboundAttachment` in `store`; the `emailtransport` placement is documented as failing with an import cycle
- [ ] `withTx` inside `RunAsOrg` issues no `BEGIN` and rolls back with the caller; bare it commits independently

---

## Phase 2: Org event journal + `email.received` fan-out

### 0. Subscription preflight and re-consent

```sql
SELECT id, org_id, url, events, disabled_at FROM org_webhooks
WHERE 'email.received' = ANY(events) OR url LIKE 'http://%';
```

Rows matching `email.received` on a **non-HTTPS** URL are **disabled** by `0021`, not grandfathered — disabling is reversible and observable; cleartext PII is neither. Unknown event names are logged and left (they match nothing). `http://` rows not subscribing to inbound keep working for outbound with a deprecation warning. The query and result go in the PR; a non-empty result needs operator ack.

### 1. Four-step rollout with **version-targeted** migrations

Revision 4 named four steps but left migration as a single `up --scope all`, which would apply `0021` alongside `0020` and reintroduce the outage. Each step now pins a target version and a **pinned artifact**:

| Step | Migration | Artifact | Gate before proceeding |
|---|---|---|---|
| **1a. Expand** | `up --to 0020` — create `org_events`, `ADD COLUMN org_event_id`. `outbox_event_id` stays `NOT NULL` | unchanged | `status` shows `0021`+ pending |
| **1b. Dual reader** | none | **pinned control-plane digest** with schema window `[0020,0026]` | **No pre-1b control-plane instance remains** — assert every control-plane Machine reports exactly the expected 1b digest via the Machines API. If a rolling release intentionally permits more than one digest, the release manifest must list each allowed digest with the same compatible window. This is the gate that makes 1c safe. |
| **1c. Relax** | `up --to 0023` (see §4) | producer flag off | zero pending through `0023` |
| **1d. Activate** | none | flag on via D8 | flag cache converged (Phase 7 §3) |

The 1b binary supports the whole additive rollout window, `0020` through
`0026`, so it remains healthy while `0021`–`0026` land. It starts in `verify`
mode (Phase 0 §5), which **never applies migrations**; only the explicit
`nerve-migrate up --to ...` jobs may advance schema. An artifact whose declared
window excludes the current DB version refuses to start. That separates
"schema this binary can safely run against" from "schema a deploy is allowed
to apply" and closes the startup-migration hole without making 1c impossible.

**N−1 test**: the pre-1b binary against a `0021` schema containing a NULL `outbox_event_id` reproduces the claim-batch failure; the 1b binary claims the same row alongside an outbound row.

### 2. Migrations

```sql
-- core/0020_org_events.sql  (EXPAND ONLY)
CREATE TABLE org_events (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  event_type text NOT NULL, ref_kind text NOT NULL, ref_id uuid NOT NULL,
  payload jsonb NOT NULL,
  fanned_out_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (org_id, event_type, ref_kind, ref_id)
);
CREATE INDEX idx_org_events_fanout_owed ON org_events (created_at) WHERE fanned_out_at IS NULL;
ALTER TABLE org_webhook_deliveries ADD COLUMN org_event_id uuid REFERENCES org_events(id) ON DELETE CASCADE;
-- + RLS enable/force + tenant policy (mirror 0017:27-39)
```

```sql
-- core/0021_org_events_relax.sql  (RELAX — 1b readers must be everywhere first)
ALTER TABLE org_webhook_deliveries
  ALTER COLUMN outbox_event_id DROP NOT NULL,
  ADD CONSTRAINT chk_delivery_event_source
    CHECK ((outbox_event_id IS NOT NULL) <> (org_event_id IS NOT NULL));
CREATE UNIQUE INDEX idx_webhook_deliveries_unique_org_event
  ON org_webhook_deliveries (webhook_id, org_event_id) WHERE org_event_id IS NOT NULL;
UPDATE org_webhooks SET disabled_at = now()
WHERE disabled_at IS NULL AND url NOT LIKE 'https://%' AND 'email.received' = ANY(events);
```

`fanned_out_at` makes the journal recoverable, not merely idempotent: a replay seeing `inserted=false` can distinguish "already delivered" from "journalled but never fanned out".

### 3. Journal + fan-out — `store/org_events.go`

`InsertOrgEventAndFanOut(ctx, orgID, eventType, refKind, refID, payload) (eventID string, deliveries int, err error)`, wrapped in `Store.withTx` so it is atomic in both Cloud and OSS-local shapes.

`INSERT ... ON CONFLICT DO NOTHING RETURNING id`; on conflict re-select — `fanned_out_at IS NOT NULL` → `(id, 0, nil)`, NULL → continue (crash recovery). Fan-out uses D1-aware matching and writes `org_event_id`. `UPDATE ... SET fanned_out_at = now()` last. Errors are **returned**.

`EnqueueWebhookDelivery` (`org_webhooks.go:184`) takes a nullable `outboxEventID`/`orgEventID` pair with the matching `ON CONFLICT` target; a wrapper preserves the outbound signature.

### 4. Sensitive-event matching (D1), and the migration set this phase needs

```go
// store/webhook_events.go
var WebhookEventTypes = []string{ /* the 7 outbound types, + "email.received" */ }
var SensitiveWebhookEventTypes = map[string]bool{"email.received": true}
```

Matching: `($2 = ANY(events)) OR (cardinality(events) = 0 AND NOT $3)`. `handleCreateOrgWebhook` gains the allowlist: unknown type → `400` listing valid values; `http://` → `400`.

**Step 1c applies through `0023`, not `0021`.** Phase 2's ingest writes `message_attachments` rows (§5), `0023` creates that table, and `0023` depends on `0022`. Revision 4 stated this only in later prose while the step table named only the relax migration. The `migrate` job's target for 1c is `0023`, and the 1d activation gate asserts zero pending through `0023`.

### 5. Ingest — `cloudapi/resend_webhook.go`

- After `InsertMessageWithThread` (:290), **inside the same `RunAsOrg` callback**, persist attachment metadata **and explicitly set `messages.attachments_state = 'known'`** (Phase 3 §3), then call `InsertOrgEventAndFanOut`. On error, return it: ingest fails, the Svix event is marked `failed` (:138), Resend retries, `InsertMessageWithThread` is replay-safe.
- Payload: `{"event":"email.received","org_id","inbox_id","thread_id","message_id","from","subject","has_attachments","attachment_count","created_at"}`.
- **Fix partial-recipient loss** (:180-195): error if **any** recipient failed; include failed recipients in the error for `webhook_events.last_error`.

### 6. Reconciler

`reconcileOrgEventFanOut`: `fanned_out_at IS NULL AND created_at < now() - interval '5 minutes'` → re-fan-out → stamp. `Report.OrgEventsFannedOut`.

### Success Criteria

#### Automated:
- [ ] `up --to 0020` leaves `outbox_event_id NOT NULL` and `0021` pending; the 1b binary starts against `0020` and remains green through `0026` while applying nothing
- [ ] Compatibility enforcement is independent of the dual reader: a test binary whose `maxSupported = 0020` refuses to start against `0021`, while the 1b artifact declares `[0020,0026]`
- [ ] **N−1**: pre-1b binary against `0021` with a NULL row reproduces the claim-batch failure; 1b claims it alongside an outbound row
- [ ] `nerve-migrate down` for `0021` is clean with no `org_event_id` rows and **refuses** when they exist
- [ ] Transaction ownership: from `RunAsOrg`, a forced error rolls back the message insert *and* the event; bare, it commits independently
- [ ] End-to-end: fixture → `org_events` → deliveries → dispatcher → `httptest` → signature verifies → `delivered`
- [ ] Duplicate replay → no second event or delivery; `fanned_out_at` unchanged
- [ ] Fault injection: fan-out error → nothing committed → redelivery re-ingests; a forced `fanned_out_at IS NULL` row is repaired by the scheduled reconciler
- [ ] Partial-recipient: B fails → handler errors → `webhook_events` `failed` → redelivery ingests B without duplicating A
- [ ] D1: `events=[]` gets `email.delivered` but not `email.received`; explicit subscription gets it
- [ ] Preflight: `0021` disables an `http://` row carrying `email.received`, leaves an `http://` outbound-only row enabled
- [ ] `POST /v1/webhooks`: unknown event → `400`; `http://` → `400`; HTTPS + `["email.received"]` → `201`
- [ ] Outbound fan-out payloads byte-identical to pre-change (golden test)

#### Manual:
- [ ] Preflight run against production; result and ack in the PR
- [ ] Canary org in production: real inbound mail → signed payload verifying against the create-response secret; a pre-existing wildcard sees nothing

---

## Phase 3: Attachment storage, inbound mirror, read surface

### 1. Migration `core/0022_attachment_blobs.sql`

```sql
CREATE TABLE attachment_blobs (
  org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  sha256 text NOT NULL,
  size_bytes bigint NOT NULL CHECK (size_bytes > 0),
  content_type text NOT NULL,
  content bytea NOT NULL,
  ref_count int NOT NULL DEFAULT 0 CHECK (ref_count >= 0),
  created_at timestamptz NOT NULL DEFAULT now(),
  last_ref_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (org_id, sha256)
);

CREATE TABLE org_attachment_usage (
  org_id uuid PRIMARY KEY REFERENCES orgs(id) ON DELETE CASCADE,
  bytes_used bigint NOT NULL DEFAULT 0 CHECK (bytes_used >= 0),
  bytes_quota bigint NOT NULL DEFAULT 2147483648,
  updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO org_attachment_usage (org_id) SELECT id FROM orgs ON CONFLICT DO NOTHING;
-- + RLS enable/force + tenant policy on both
```

`(org_id, sha256)` is the key: a global `sha256` PK would make `ref_count` a cross-tenant side channel. Dedup is per-org. Org creation (`store_orgs.go`) also inserts the usage row, and the reconciler seeds any missing — an unseeded row is otherwise indistinguishable from a quota failure.

### 2. Reserve-and-charge — sequenced statements, refcount driven by references

Revision 4's single mega-CTE had two correctness races. **(a)** All CTEs in one statement share the snapshot taken at statement start. A second concurrent upload of the same SHA blocks on the `usage_row` upsert, but when it proceeds its `existing` CTE still reads the *pre-block* snapshot, sees no blob, takes the `ins` branch, and hits a unique violation. **(b)** `ref_count` was incremented by the blob statement rather than by reference creation, so a duplicate ingest or a mirror retry could bump the count without any new reference row.

Both are fixed by ordering statements inside `withTx` and making refcount trigger-driven:

```sql
-- 1. Serialize this org's quota. Separate statement => the next statement
--    gets a fresh snapshot that includes any concurrent blob insert.
INSERT INTO org_attachment_usage (org_id) VALUES ($1) ON CONFLICT (org_id) DO NOTHING;
SELECT bytes_used, bytes_quota FROM org_attachment_usage WHERE org_id = $1 FOR UPDATE;

-- 2. Fresh snapshot: a concurrent insert of the same content is now visible.
INSERT INTO attachment_blobs (org_id, sha256, size_bytes, content_type, content)
VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (org_id, sha256) DO NOTHING
RETURNING size_bytes;          -- empty => already present, charge nothing

-- 3. Charge only on a real insert, only if it fits (checked against the locked row).
UPDATE org_attachment_usage SET bytes_used = bytes_used + $3, updated_at = now()
WHERE org_id = $1;
```

- Step 1's `FOR UPDATE` serializes concurrent uploads for the org; whoever proceeds sees committed work from whoever went first.
- If step 2 returns a row and `bytes_used + size > bytes_quota` from step 1, the caller **rolls back the transaction** — nothing is committed, and an immediate retry is rejected identically rather than sliding into an already-present path.
- An **already-present** blob is admitted regardless of quota: it consumes no new bytes, and refusing a reference to bytes already stored would be an error that storing nothing could fix.
- Blobs are inserted with `ref_count = 0`.

**`ref_count` changes only when a reference row changes.** Triggers on `message_attachments` and `outbox_attachments`:

| Trigger | Effect |
|---|---|
| `AFTER INSERT` (when `blob_sha256 IS NOT NULL`) | `ref_count + 1`, `last_ref_at = now()` |
| `AFTER UPDATE OF blob_sha256` | decrement the old value if it was non-NULL, increment the new if non-NULL |
| `AFTER DELETE` (when `blob_sha256 IS NOT NULL`) | `ref_count - 1` |

The `AFTER UPDATE` variant is what makes D7 work at all: release retains the metadata row and nulls `blob_sha256`, which no `AFTER DELETE` trigger would ever see. Because the reference-row insert is `ON CONFLICT DO NOTHING`, a duplicate ingest inserts nothing and the trigger does not fire — the refcount cannot drift from the reference set.

### 3. Migration `core/0023_message_attachments.sql`

```sql
ALTER TABLE messages ADD CONSTRAINT uq_messages_org_id UNIQUE (org_id, id);   -- if absent

-- Rollout default stays 'pending_backfill' for BOTH existing and new rows.
-- The metadata-aware writer sets 'known' explicitly in the same transaction
-- (Phase 2 §5). The default is NOT flipped to 'known' here: during cutover the
-- migration runs before the new control plane, so an old writer would otherwise
-- create metadata-less rows already marked 'known' and invisible to backfill.
ALTER TABLE messages ADD COLUMN attachments_state text NOT NULL DEFAULT 'pending_backfill'
  CHECK (attachments_state IN ('known','pending_backfill','unknown_metadata_expired'));

-- Classify existing rows by what is actually recoverable.
UPDATE messages SET attachments_state = 'known'
  WHERE direction = 'outbound' OR received_email_id IS NULL OR received_email_id = '';
UPDATE messages SET attachments_state = 'unknown_metadata_expired'
  WHERE attachments_state = 'pending_backfill' AND created_at < now() - interval '30 days';
```

Outbound messages and inbound rows without a `received_email_id` can never gain inbound attachment metadata, so leaving them `pending_backfill` would make the "zero pending" gate unreachable forever. They are `known` (with zero attachments); only recoverable inbound rows stay `pending_backfill`.

```sql
CREATE TABLE message_attachments (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  message_id uuid NOT NULL,
  ordinal int NOT NULL,
  provider_attachment_id text NOT NULL,
  filename text NOT NULL DEFAULT '',
  content_type text NOT NULL DEFAULT 'application/octet-stream',
  content_disposition text NOT NULL DEFAULT '',
  content_id text NOT NULL DEFAULT '',
  size_bytes bigint,
  availability text NOT NULL DEFAULT 'pending'
    CHECK (availability IN ('pending','available','expired','too_large','failed')),
  blob_sha256 text,
  attempt_count int NOT NULL DEFAULT 0,
  next_attempt_at timestamptz NOT NULL DEFAULT now(),
  locked_at timestamptz, locked_by text, last_error text, mirrored_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (message_id, provider_attachment_id),
  UNIQUE (org_id, id),
  FOREIGN KEY (org_id, message_id)  REFERENCES messages (org_id, id) ON DELETE CASCADE,
  FOREIGN KEY (org_id, blob_sha256) REFERENCES attachment_blobs (org_id, sha256)
);
CREATE INDEX idx_message_attachments_claim
  ON message_attachments (next_attempt_at) WHERE availability = 'pending';

ALTER TABLE attachments ENABLE ROW LEVEL SECURITY;
ALTER TABLE attachments FORCE ROW LEVEL SECURITY;
CREATE POLICY deny_all_legacy_attachments ON attachments USING (false) WITH CHECK (false);
COMMENT ON TABLE attachments IS
  'DEPRECATED 2026-08 — superseded by message_attachments. Deny-all RLS. Drop after 0023 has been live 30 days.';
-- + RLS on message_attachments + the three refcount triggers from §2
```

Queue columns mirror `outbox_messages`, so `ClaimOutboxMessages`' `FOR UPDATE SKIP LOCKED` pattern transfers directly.

### 4. Backfill is a release gate

`nerve-reconcile backfill-attachments --since <ts> [--resume]` walks `attachments_state='pending_backfill'` rows, re-fetches envelopes, writes metadata, sets `known`. Progress **is** the column, so an interrupted run resumes. Rate-limited against Resend. Prints the retention deadline; refuses to start on an out-of-window `--since` without `--force`; unresolvable rows become `unknown_metadata_expired`.

The API surfaces `attachments: []` alongside `attachments_state`, so a consumer distinguishes "no attachments" from "we can no longer know".

**The activation gate is zero `pending_backfill` rows in total**, not merely zero newer than the deadline — the §3 classification makes that reachable.

### 5. Mirror worker — `internal/attachments/mirror.go` (cloud-only)

Claim `availability='pending' AND next_attempt_at <= now()` plus lease-expired rows, `FOR UPDATE SKIP LOCKED`, incrementing `attempt_count`, stamping `locked_at`/`locked_by`. Bounded by `mirrorConcurrency` and the Phase 1 §2 budget. `GetAttachment` for a fresh URL, then download via `httpsafe` (Resend allowlist, 30s timeout) with `io.CopyN(dst, body, maxAttachmentBytes+1)` — the extra byte → `too_large`. SHA-256 while streaming; §2's sequence inside `withTx`; quota rejection rolls back and marks `failed`. Backoff with `last_error`; `attempt_count >= 6` → `failed`; Resend 404 → `expired`.

### 6. Dereference, retention and abandon (D7)

**Release, never delete.** Migration `0025` (Phase 4) adds:

```sql
ALTER TABLE outbox_messages
  ADD COLUMN terminal_at timestamptz,
  ADD COLUMN attachments_released_at timestamptz;
UPDATE outbox_messages SET terminal_at = now() WHERE status IN ('sent','failed');
CREATE INDEX idx_outbox_release_owed ON outbox_messages (terminal_at)
  WHERE attachments_released_at IS NULL AND terminal_at IS NOT NULL;
```

- `MarkOutboxMessageSent`/`MarkOutboxMessageFailed` set `terminal_at = now()`; **replay clears it**; the enqueue path sets it for suppressed messages, which land terminal without ever being claimed (`outbox.go:153`).
- The reconcile sweep releases **bytes only** for `status='sent'` rows past `terminal_at + outboundAttachmentRetention` (default **90 days**, configurable): `UPDATE outbox_attachments SET blob_sha256 = NULL` — the `AFTER UPDATE` trigger decrements — then stamp `attachments_released_at`.
- **`failed` rows keep bytes until explicitly abandoned**, so replay stays possible for the DLQ's lifetime.
- **Abandon is a real endpoint**: `POST /v1/admin/outbox/{id}/abandon`, routed alongside the existing `/{id}`, `/{id}/events`, `/{id}/replay` (`handler_dlq.go:59-66`). Scope `nerve:admin.deliverability` (matching the other DLQ handlers, :21). Store method `AbandonOutboxMessage(ctx, orgID, id)` runs in `withTx`: refuses unless `status='failed'` (`409` otherwise), nulls `blob_sha256` on its `outbox_attachments`, stamps `attachments_released_at`, writes an audit row via `RecordAudit`, and is **idempotent** — an already-abandoned row returns `200` with no further change. Concurrent calls serialize on `SELECT ... FOR UPDATE` of the parent.
- **Retained always**: the row, `outbox_events`, `org_webhook_deliveries`, the `outbox_attachments` metadata (filename, type, size, **sha256**), and the `(org_id, idempotency_key)` tombstone. `chk_outbox_status` untouched, so `/v1/admin/outbox/failed` and the list/timeline/replay paths are unaffected.
- Replaying a released message returns a typed `attachments_released` error naming the missing digests. `/v1/admin/outbox/{id}` exposes `attachments_available` so an operator sees replayability first.

**GC**: `attachment_blobs WHERE ref_count = 0 AND last_ref_at < now() - interval '7 days'` → delete, decrement usage. `reconcileAttachmentUsage` recomputes `bytes_used` and seeds missing usage rows.

### 7. Read surface and scopes

- `GET /v1/inboxes/{id}/threads/{thread_id}`: each message gains `attachments: [{id, filename, content_type, size_bytes, availability}]` and `attachments_state`.
- **Scope fix on every read path**: `handleInboxThreads` (`handler_inboxes.go:335`), thread-detail and message-read handlers, and `handleAttachmentProxy` (`handler_messages.go:43`) all gain `"nerve:email.read"`.
- **401 vs 403**: `auth.ErrUnauthenticated` + a shared `writeAuthError` mapping it to `401` with `WWW-Authenticate`, `ErrForbidden` to `403`.
- **Proxy rewrite**: `uuid.Parse` both IDs before any SQL (`400`); look up by `(org_id, message_id, id)`; **acquire the memguard budget before `SELECT content`** (Phase 1 §2); serve `available` from `attachment_blobs`; `pending` → `202` + `Retry-After`; `expired` → `410`; `too_large`/`failed` → `409`. The `http.DefaultClient` + unbounded `io.Copy` path is deleted.

### Success Criteria

#### Automated:
- [ ] `attachment_blobs` PK is `(org_id, sha256)` (asserted against `information_schema`)
- [ ] First upload for a brand-new org succeeds; so does one whose usage row was deleted out from under it
- [ ] **Concurrent same-SHA**: two parallel uploads of identical content both succeed — one inserts, one takes the already-present path — with **no unique violation** and `ref_count = 2`
- [ ] **Quota rejection commits nothing**: blobs, `bytes_used` and `ref_count` unchanged; an immediate retry is rejected identically rather than taking the already-present path
- [ ] 20 concurrent mirrors against a quota admitting 10 → exactly 10 succeed, `bytes_used <= bytes_quota`
- [ ] **Refcount follows references only**: a duplicate ingest (conflicting reference insert) does **not** change `ref_count`; a mirror retry does not either
- [ ] `AFTER UPDATE OF blob_sha256` decrements on release; `AFTER INSERT`/`AFTER DELETE` behave symmetrically; cascade delete of a message decrements
- [ ] **D7 sent-path**: a released `sent` row is absent from `/v1/admin/outbox/failed` (failed-only) but its history is intact via `/v1/admin/outbox/{id}` and `/{id}/events`, with `attachments_available: false`, sha256 retained, and the idempotency tombstone present
- [ ] **D7 failed-path**: a `failed` row past retention keeps bytes and replays successfully; `POST /{id}/abandon` releases bytes, is idempotent on repeat, returns `409` on a non-`failed` row, writes an audit row, and a subsequent `/{id}/replay` returns typed `attachments_released`
- [ ] Replay clears `terminal_at`; a suppressed enqueue sets it
- [ ] Pre-existing terminal rows get `terminal_at = now()` and are not released for a full window
- [ ] Queue mechanics: lease expiry re-claim; transient failure sets `last_error` and backs off; `attempt_count >= 6` → `failed`; Resend 404 → `expired`
- [ ] Bounded download: 20 MB source against a 10 MB cap → `too_large` without allocating the full body
- [ ] Tenant integrity: mismatched `(org_id, message_id)` fails the FK; org A gets `404` for org B's attachment; org A cannot reference org B's blob sha
- [ ] Availability: `pending` → `202`; `expired` → `410`; `too_large` → `409`; malformed UUID → `400` before any query
- [ ] **Classification**: after `0023`, outbound rows and inbound rows without `received_email_id` are `known`; recoverable inbound rows are `pending_backfill`; rows past retention are `unknown_metadata_expired`; **a row inserted by the old writer during cutover is `pending_backfill`, not `known`**
- [ ] Backfill resumes after interruption and drives `pending_backfill` to zero **in total**
- [ ] Discovery→download with a key holding only `nerve:email.read`
- [ ] `401` without credentials, `403` with a valid key lacking scope
- [ ] Legacy `attachments`: deny-all verified under a tenant role

#### Manual:
- [ ] Canary org in production: PDF email → `available` within 60s → downloaded with the household runtime key
- [ ] Canary org: with Resend returning 404, the download still succeeds from our blob

---

## Interleaved Phase 2.1: Abrolia household email tenancy foundation

This approved prerequisite occupies `core/0024_email_tenancy.sql` before the
still-unimplemented outbound attachment phase. Abrolia routes many household
inboxes through one verified root domain, while each household remains its own
organization and RLS tenant. A root-domain owner therefore grants a bounded
right to create inboxes for one grantee org; ownership of the domain itself is
never transferred.

### 1. Schema and reconciliation identity

- Add stable, nullable `external_ref` reconciliation keys to `orgs`,
  `org_domains`, `inboxes` and `org_webhooks`; active resources use partial
  uniqueness where tombstoned rows must remain addressable.
- Add `orgs.deleted_at` for fail-closed tenant tombstoning.
- Add `org_domain_grants(owner_org_id, org_domain_id, grantee_org_id,
  external_ref, status, revoked_at)` with one active grant per
  `(org_domain_id, grantee_org_id)` and immutable owner/domain/grantee identity
  on replay.
- Grant RLS exposes the row to its owner and grantee, but mutation remains an
  owner operation. Inbox creation proves either domain ownership or an active
  grant in the same transaction.
- The down migration refuses while grants, external reconciliation identities,
  or tenant tombstone state still exist; rollback may not silently discard
  durable control-plane identity.

### 2. Replay-safe store contract

- `EnsureOrg`, `EnsureOrgDomain`, `EnsureOrgDomainGrant` and
  `EnsureOrgWebhook` use conflict-safe insert/fetch paths and return the same
  resource for a matching retry; a reused external ref with different immutable
  fields returns a typed idempotency conflict.
- Concurrent grant creation and org/domain deletion serialize on sorted
  transaction advisory locks, so deletion cannot race a late grant into an
  orphaned or tombstoned tenant.
- Grant revocation performs its active-inbox guard through a narrowly scoped
  RLS-bypass query so an owner cannot revoke access while grantee inboxes still
  exist merely because owner-scoped RLS hid them.
- Inbox activation and new grant revocation serialize on the same transaction
  advisory lock keyed by domain and grantee. The grantee-side trigger retains
  a compatibility row lock for old revokers through a schema-qualified,
  temp-safe definer function and narrow temporary RLS bypass, captures the
  result, and restores `app.cloud_mode` before inbox RLS evaluates the pending
  row.
- Existing attachment metadata and durable byte sizes remain stable when an
  envelope or reconciliation replay omits fields it no longer owns.

### 3. Cloud-owned follow-through

`cloud/0008_email_tenancy_and_idempotency.sql` adds stable external references
to Cloud API keys. Cloud reconciliation ensures one org, grant, inbox, key and
signed inbound webhook per household. Repeating the same provisioning request
returns existing resources and never re-emits an existing key secret. Runtime
startup pins core `[24,24]`; the control plane pins core `[24,24]` and cloud
`[8,8]` before production activation.

After the production canary, forward repair `core/0027` advances both runtime
and control-plane core windows to `[27,27]`; no Cloud-owned schema migration is
required for this trigger/store serialization fix.

### Success Criteria

#### Automated:

- [x] Fresh migration and upgrade from the `0023` head both reach core `0024`;
  the compiled runtime window is `[24,24]`.
- [x] Matching external-ref retries return one org/domain/grant/webhook under
  concurrent callers; mismatched immutable identity returns a conflict.
- [x] A grantee can create and read its inbox on the granted root domain but
  cannot read another grantee's inbox or tenant messages.
- [x] Revocation is refused while an active grantee inbox exists, including
  when invoked from an owner-scoped RLS transaction.
- [x] A real non-superuser app role can create a granted-domain inbox; a
  legacy revoker holding only the grant row lock rejects the waiting inbox;
  a committed inbox blocks the new advisory-lock revoker; the trigger restores
  tenant GUC state, rejects caller `pg_temp` spoof tables, and does not bypass
  cross-tenant inbox RLS.
- [x] Grant creation racing org deletion waits for the same advisory lock and
  then fails closed rather than creating a grant for a deleted org.
- [x] Down migration refuses durable tenancy/reconciliation state instead of
  dropping it.
- [x] An active service token blocks org deletion; after revocation deletion
  succeeds; token issuance for the tombstoned org fails, with issuance and
  deletion serialized by the same advisory lock. Rotation is atomic across old
  token revocation and replacement insertion, including rollback on insert
  failure.

#### Manual:

- [ ] After the digest-pinned Cloud deployment, provision two clearly
  synthetic household orgs under `abrolia.com`, each with its own inbox and
  tenant key; send the same PDF to both; each key can read and download only
  its own durable attachment bytes.

---

## Phase 4: Outbound attachments (nerve-oss)

### 1. Migration `core/0025_outbox_attachments.sql`

```sql
ALTER TABLE outbox_messages ADD CONSTRAINT uq_outbox_messages_org_id UNIQUE (org_id, id);  -- if absent
-- + terminal_at / attachments_released_at and their backfill (Phase 3 §6)

CREATE TABLE outbox_attachments (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  outbox_message_id uuid NOT NULL,
  ordinal int NOT NULL,
  filename text NOT NULL,
  content_type text NOT NULL,
  size_bytes bigint NOT NULL CHECK (size_bytes > 0),
  sha256 text NOT NULL,          -- retained after release (D7)
  blob_sha256 text,              -- NULL once bytes are released
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (outbox_message_id, ordinal),
  FOREIGN KEY (org_id, outbox_message_id) REFERENCES outbox_messages (org_id, id) ON DELETE CASCADE,
  FOREIGN KEY (org_id, blob_sha256) REFERENCES attachment_blobs (org_id, sha256)
);
-- + RLS + the three refcount triggers from Phase 3 §2
```

`sha256` and `blob_sha256` are separate columns precisely so D7 can null the live reference while retaining the digest as audit metadata. `size_bytes > 0` stands: **zero-byte attachments are rejected at validation** with a typed `attachment_empty` error before any SQL, rather than silently accepted and then violating the CHECK.

### 2. Wire-size limit before decode

`r.Body = http.MaxBytesReader(w, r.Body, maxMCPBodyBytes)` before `Decode` (`server.go:69`), `maxMCPBodyBytes = 16 MB`, **paired with the Phase 1 §2 aggregate budget and `ReadTimeout`**. Over per-request limit → `413`; over budget → `503` + `Retry-After`.

### 3. Attachment-aware fingerprint and a correctly-ordered enqueue

```go
func contentHash(to, subject, textBody, htmlBody string, atts []OutboundAttachment) string {
    h := sha256.New()
    // ... existing NUL-separated body fields ...
    for i, a := range atts {   // ordinal is significant and part of the hash
        fmt.Fprintf(h, "%d\x00%s\x00%s\x00%s\x00", i, a.Filename, a.ContentType, a.SHA256)
    }
    return hex.EncodeToString(h.Sum(nil))
}
```

The load-bearing fix: the `existing` CTE short-circuits on `content_hash` before the idempotency key is read (`outbox.go:158-176`), so a key-only change is inert.

**Ordering matters, and revision 4 had it backwards.** `outbox_attachments` carries an immediate FK to `outbox_messages`, so inserting refs before the parent fails outright. Inside `Store.withTx`:

1. **Resolve or insert the parent**, returning `{id, inserted bool, fingerprintMatch bool}` — the existing CTE already distinguishes these cases; the helper surfaces them instead of collapsing to an id.
2. If `inserted == false` and the stored `content_hash` **differs** from this call's fingerprint, the same `(org_id, idempotency_key)` is being reused for different content → return a typed `idempotency_conflict` error rather than silently returning the old message.
3. If `inserted == false` and the fingerprint matches → return the existing id, create **no** blobs and **no** refs. This is the replay path, and it is why refcount must be reference-driven (Phase 3 §2): a replay must not bump anything.
4. Only when `inserted == true`: reserve-and-charge each blob (Phase 3 §2), then insert `outbox_attachments`, whose `AFTER INSERT` trigger increments `ref_count`.
5. The **suppression path** (`outbox.go:153`, which inserts a terminal row without going through the normal insert) runs through the same helper and sets `terminal_at`.

Quota rejection → typed error, transaction rolled back, nothing written.

> OSS `contentHash` is 3-arg, Cloud's is 4-arg. Land the OSS change in the four-arg-plus-attachments form so the synced patch *converges* the signatures.

### 4. Worker and adapters

- `outbox_worker.go`: **lazy-load one message's attachments at a time** (Phase 1 §2), joining `outbox_attachments` → `attachment_blobs` immediately before the provider call and releasing after. A row with `blob_sha256 IS NULL` fails with typed `attachments_released` rather than sending an incomplete message.
- Resend: map to the `attachments` array, base64 content.
- SMTP: `multipart/mixed`, base64 transfer encoding, RFC 2231 filenames for non-ASCII.

### 5. MCP tool schemas

`compose_email`/`send_reply` gain optional `attachments: [{filename, content_type, content_base64}]`, gated by the D8 flag. Validation before enqueue, each a typed tool error:

| Limit | Value | Error |
|---|---|---|
| count | ≤ 10 | `attachment_count_exceeded` |
| per-file | ≤ 10 MB decoded, **> 0 bytes** | `attachment_too_large` / `attachment_empty` |
| total | ≤ 10 MB decoded | `attachment_total_too_large` |
| filename | ≤ 255 bytes, no `/` `\` NUL or control chars, non-empty after trim | `attachment_invalid_filename` |
| content_type | MIME allowlist | `attachment_type_not_allowed` |
| base64 | strict decode | `attachment_invalid_encoding` |

Allowlist: `image/png`, `image/jpeg`, `image/webp`, `application/pdf`, `text/plain`, `application/vnd.openxmlformats-officedocument.{wordprocessingml.document,spreadsheetml.sheet}`. Filenames are sanitised once server-side, and the sanitised value enters the fingerprint. The idempotency key needs no change (`hashJSON`, `server.go:146,151-161`). `draft_reply` is unchanged (D3).

### 6. `docs/MCP_Contract.md` (OSS)

Updated **here**, before the runtime build, so the published manifest's `MCP_CONTRACT_HASH` describes the shipped contract.

### Success Criteria

#### Automated:
- [ ] `go build ./... && go test ./...` green in nerve-oss; exact-mirror paths identical across repos after sync
- [ ] Dedup: identical bodies + different attachments → two rows; identical bodies + identical attachments → one
- [ ] **FK ordering**: enqueue with attachments succeeds (parent first); a test forcing the reverse order fails on the FK, documenting why
- [ ] **Idempotency conflict**: reusing a key with a different fingerprint returns typed `idempotency_conflict`, not the old message
- [ ] **Replay**: reusing a key with a matching fingerprint returns the existing id and leaves `ref_count` unchanged
- [ ] Suppressed enqueue with attachments sets `terminal_at` and is consistent with the normal path
- [ ] Zero-byte attachment → typed `attachment_empty` before SQL; the CHECK is never reached
- [ ] Atomicity in both shapes: forced failure after blob upsert leaves no blob, ref, message or charge
- [ ] 17 MB body → `413` before decoding; 5 concurrent 16 MB bodies against a 64 MB budget → the 5th gets `503`
- [ ] Enqueue + worker roundtrip: 2 attachments delivered with correct bytes, order, filenames, MIME; a released row fails typed
- [ ] SMTP multipart golden test, byte-exact, non-ASCII filename; Resend adapter payload test
- [ ] `tools/list` exposes `attachments` on `compose_email`/`send_reply`, not on `draft_reply`, and omits it with the flag off
- [ ] `MCP_Contract.md` hash in the published manifest matches the shipped `tools/list`

---

## Phase 5: Cloud integration

1. **Accept the sync PR** for the repair `0018`, stable-created-time baseline `0019`, feature migrations `0020`–`0025`, and every exact-mirror path. `diff -r migrations/core` empty; the sync job has already built and tested cloud (Phase 0 §4).
2. **Wire the mirror worker** into `cmd/nerve-control-plane` with config and metrics.
3. **Cloud-only handlers**: Phase 3 §7's read surface, scopes, 401/403, and the abandon endpoint — all in the cloud-only list (`handler_messages.go`, `handler_inboxes.go`, `handler_webhooks.go`, `handler_dlq.go`).
4. **Reconcile additions** (Phase 2 §6, Phase 3 §6) plus metrics.
5. **Regenerate `runtime.lock`** with the new runtime image digest and all four
   hashes; publish the release-manifest bindings from each runtime/control-plane
   digest to its compiled schema compatibility window — own commit.

### Success Criteria

- [ ] `diff -r migrations/core` empty; every exact-mirror path identical
- [ ] `go build ./... && go test ./...` green with Postgres, **zero skips**
- [ ] `verify_runtime_lock.sh` green against the published manifest, digest matched
- [ ] Mirror worker starts with the control plane and reports queue depth
- [ ] Manual — canary org: `compose_email` with a PDF lands in a real external mailbox on **both** Resend and SMTP

---

## Phase 6: SDK 0.2.0 — built and tested **before** release

Revision 4 had Phase 6 install, test and publish a wheel that Phase 7 built. The SDK now lands first, producing the single artifact the release promotes.

### 1. Transport split with correct lifecycle

`NerveClient` speaks MCP to the runtime origin (`client.py:59`); `https://nerve-runtime.fly.dev/v1/*` returns `404` (`TENANT_GUIDE.md:16`). Attachments are added to the **REST** thread response, so `NerveClient.get_thread` (MCP) would never see them — REST delegation, not an MCP contract bump.

- `NerveRestClient(base_url, api_key=None, bearer_token=None)` in `rest.py` with `get_thread(inbox_id, thread_id)` and `get_attachment(message_id, attachment_id)`. **Both auth modes**, mirroring `client.py:60-63,87-91` — a REST client understanding only `X-Nerve-Cloud-Key` would silently drop bearer credentials.
- **Lifecycle**: `client.rest` is a **lazy singleton** owned by `NerveClient`. `NerveClient.close()` currently closes only `self._http` (`client.py:382-384`); it now closes both pools, in a `try/finally` so a failure closing one still closes the other, and is safe to call twice. `NerveRestClient` is **also a standalone async context manager** with its own `close()`, for direct use.
- `rest_base_url` defaults to the production REST origin **only when `base_url` is the production runtime origin**; otherwise it is required and `client.rest` raises `NerveConfigurationError`. Defaulting unconditionally would transmit a staging or self-hosted credential to `https://nerve.email`.
- Status mapping: `202 → NerveAttachmentPendingError`, `410 → NerveAttachmentExpiredError`, `409 → NerveAttachmentUnavailableError`.
- **Public surface**: `__init__.py` exports `NerveRestClient`, `NerveConfigurationError` and the three attachment exceptions, added to `__all__` (currently 8 names, `__init__.py:14-23`).
- Documented flow: `client.rest.get_thread()` → `client.rest.get_attachment()`. `NerveClient.get_thread` stays MCP-only, and its docstring says so.

### 2–3. Attachments and `draft_reply`

`compose_email(..., attachments=)` / `send_reply(..., attachments=)` accept `[{filename, content_type, content_base64}]` or `(filename, bytes)`; client-side pre-validation mirrors the server caps. `draft_reply`'s `attachments` parameter is **removed** (`client.py:282,289,292-293`), not accept-and-ignore; `body_or_draft_id`'s docstring (:299,308) says body-only.

### 4. `tools.py` — the second schema registry

`send_reply` (:124-140) and `compose_email` gain `attachments`; `draft_reply_with_policy` (:113) must not. A test asserts `tools.py` matches the server's `tools/list`.

### 5. Versions and the promoted artifact

All four sources → `0.2.0` (`pyproject.toml:3`, `__init__.py:25`, `client.py:42`, `tests/test_client.py:41`), with a CI test asserting equality and a check on the built wheel's `importlib.metadata.version`. **The wheel is built once here**, uploaded as a CI artifact with its SHA-256 recorded, and that exact file is what the canary contract suite installs and what publishes. `publish-python-sdk.yml` is changed from `python -m build` to downloading and publishing the promoted artifact, with a SHA check.

### 6. Admin surface

`create_webhook(org_id, url, events)` — `events` required non-empty (D1), `url` required HTTPS. `rotate_webhook_secret(webhook_id, org_id=None)` — `org_id` **required on the bootstrap-admin route**, since `handleRotateOrgWebhookSecret` resolves via `resolveOrgIDForPrincipal(principal, query.Get("org_id"))` (`handler_webhooks.go:169`) and a bootstrap principal has no `OrgID`. Same for `delete_webhook`, `list_webhooks`.

### 7. Docs

`TENANT_GUIDE.md`: two origins and two accessors, the `email.received` payload and verification recipe, explicit-subscription requirement, availability states and `attachments_state`; reconcile the `nerve:email.read` row (:37). `REPO_SPLIT_RUNBOOK.md`: D9, the three-list manifest, artifact authority, promotion, D7, D8.

### 8. Runtime config

`fly.runtime.toml` `min_machines_running = 1` (currently `0`) — committed **here**, so it is part of the runtime config the release deploys rather than a change applied after the runtime is already live.

### Success Criteria

#### Automated:
- [ ] `pytest sdk/python` green
- [ ] `client.rest.get_thread()` returns `attachments[]` and targets the REST origin; `NerveClient.get_thread` targets MCP
- [ ] Non-production `base_url` without `rest_base_url` raises `NerveConfigurationError` before any request
- [ ] A bearer-token client produces a bearer-authenticated REST request
- [ ] **Lifecycle**: after `await client.close()`, **both** pools report closed; double-close is a no-op; `NerveRestClient` works standalone as an async context manager; a leak test asserts no open transports at exit
- [ ] `NerveRestClient` and the four new exceptions are importable from `nerve_email` and present in `__all__`
- [ ] Each availability status maps to its typed exception
- [ ] `draft_reply(attachments=...)` raises `TypeError`
- [ ] `tools.py` matches `tools/list` for the three tools
- [ ] All four version sources are `0.2.0`; wheel metadata matches; the recorded wheel SHA is stable across the pipeline
- [ ] `create_webhook` with empty `events` or `http://`, and `rotate_webhook_secret` without `org_id` on a bootstrap client, raise before any HTTP call

---

## Phase 7: Sequenced release (D6, D8, D10)

### 1. One chain, gated by a snapshot rehearsal and a canary org (D10)

Revisions 3–5 specified a staging chain. There is no staging infrastructure — Fly hosts only `nerve-runtime` and `nerve-control-plane` — and standing some up would not have bought what it appeared to:

| What staging was for | Why it would not have worked | What does the job |
|---|---|---|
| Rehearse the `0020`/`0021` sequence | A staging DB is not production-shaped, so the interesting failures do not occur there | **Snapshot rehearsal** (below) + the CI N−1 and `maxSupported` tests (Phase 0 §8, Phase 2 §1) |
| Rehearse the backfill | Backfill walks inbound messages with `received_email_id` and re-fetches envelopes from Resend. A staging DB has no such rows and Resend has no envelopes for them — staging **cannot** test this at all | **Snapshot rehearsal** against restored production data |
| Run the contract suite | Needs real Resend, a verified domain, real inbound mail, a real external mailbox. Staging would need a second verified domain, webhook endpoint and DNS, and still be less faithful | **Canary org in production**, gated by the D8 per-org flag |
| Not break the pilot while testing | D8 flags already isolate this: the pilot org stays off until the canary passes | **Per-org flags** |
| Catch a binary that will not boot | — | Fly health checks: `fly.control-plane.toml` already has `/healthz` with a grace period, so a failing machine never takes traffic and the deploy rolls back |

```
build:      control-plane+reconcile image digest, runtime digest (from lock), wheel SHA
rehearse:   restore latest prod snapshot → ephemeral DB → migrate(--to each step)
            → backfill --dry-run → assert zero pending → drop
production: [protected-branch policy + confirm=PRODUCTION]
            → migrate(--to) → runtime → control-plane → reconcile-machine → backfill
            → activate(canary org) → contract-suite(canary) → activate(pilot org)
            → smoke → publish-wheel
```

Every job takes **artifact digests as inputs** — nothing is rebuilt. `control-plane-deploy.yml` becomes deploy-only, taking a digest. `verify_cloud_deploy_order.sh` is rewritten to assert the `needs:` graph in `deploy.yml` and fails when an edge is removed.

**What gates production**, given that environment protection rules are unavailable on this plan (`required_reviewers` and `wait_timer` both return 422; see `docs/REPO_SPLIT_RUNBOOK.md`):

1. **Branch policy** — `cloud-production` sets `deployment_branch_policy.protected_branches = true`, and `main` requires `lint`, `unit`, `integration`, `e2e`, `coverage`, `codex-review-window`. Production cannot be deployed from code that has not passed CI.
2. **Snapshot rehearsal** — a required `needs:` predecessor of `migrate`.
3. **Typed confirmation** — `confirm=PRODUCTION`. Stops a mis-selected dropdown; not a second-person review.

`main` currently has no required approving reviews, so there is no human-approval step. Requiring PR approvals on `main` is the cheap way to add one and is recommended; it is not a blocker for this plan.

**The one prerequisite D10 introduces**: a **canary org** with its own verified receiving domain — a subdomain of an existing verified domain is enough, since inbound routes by domain to org (`GetReceivingOrgDomainByDomain`, `resend_webhook.go:203`). One DNS record set, no new apps, no second Postgres, no second Resend account.

### 2. Migration, rehearsal and backfill are explicit, version-targeted jobs

`migrate` takes a `--to <version>` input per step (Phase 2 §1), then asserts `status`.

`rehearse` restores the most recent production snapshot into an ephemeral database, replays every `--to` step in cutover order, runs `backfill-attachments --dry-run`, asserts zero `pending_backfill` remain, and drops the database. It is where the migration sequence meets production-shaped data, and it is a required predecessor of the production `migrate`. A rehearsal against a snapshot older than 24h fails rather than proceeding on stale shape.

`backfill` runs `nerve-reconcile backfill-attachments` against production and asserts **zero** `pending_backfill` rows.

### 3. D8 — the executable flag contract

**Schema in `core`** (`0026_feature_flags.sql`), not cloud: the runtime is OSS and must find the table in a self-hosted core schema.

```sql
CREATE TABLE org_feature_flags (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid REFERENCES orgs(id) ON DELETE CASCADE,   -- NULL = global default
  flag text NOT NULL,
  enabled boolean NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now(),
  updated_by text NOT NULL
);
CREATE UNIQUE INDEX uq_org_feature_flags_org ON org_feature_flags (org_id, flag) WHERE org_id IS NOT NULL;
CREATE UNIQUE INDEX uq_org_feature_flags_global ON org_feature_flags (flag) WHERE org_id IS NULL;
CREATE INDEX idx_org_feature_flags_lookup ON org_feature_flags (flag, org_id);
-- RLS: tenant policy for org rows; global rows (org_id IS NULL) readable by all
-- tenants, writable only outside cloud_mode (i.e. by the CLI on a direct DSN).
```

Two partial unique indexes because a plain `UNIQUE (org_id, flag)` does not constrain NULL `org_id` — without the second index, duplicate global rows are possible and precedence becomes non-deterministic.

**Precedence**, highest first:
1. **Env override, tri-state**: `NERVE_FLAG_<NAME>` unset → no opinion; `force-on`/`force-off` → wins outright. This is the kill-switch, and it restarts Machines — documented as a rollout.
2. Org row (`org_id = current org`).
3. Global row (`org_id IS NULL`).
4. Compiled default — **`false`** for every flag in this plan.

**Cache**: key `(flag, org_id)`, TTL 30s, negative results cached too. **Fail-closed**: a DB error resolves to the compiled default (off) and logs at warn — a flag lookup failure must never enable a gated feature.

**Convergence gate**: after a write, the pipeline waits `2 × TTL + 5s` and then verifies effective state through a read endpoint on each app before asserting activation. Without that wait, "activate" races the cache.

**Writer**: `nerve-flags set <flag> --org <id>|--global --enabled=<bool>` / `get` / `list`, a cloud-only CLI in the same image as `nerve-migrate`, run by the deploy pipeline on the direct DSN — reusing the migrate job's credentials rather than adding an authenticated admin endpoint and a CI-held admin key. It sets `updated_by` from the CI actor, writes an audit row, and is idempotent (setting an existing value is a no-op returning success).

**OSS-local**: with `Cloud.Mode == false`, flags resolve from env and compiled defaults **without querying the table**, so a self-hosted OSS deployment that has the core migration but no rows behaves predictably. Tested explicitly.

### 4. Cutover

0. `rehearse` replays the `0018` repair, the `0019` stable-created-time
   baseline, and the `0020`, `0023`, `0024`, `0026`, and `0027`
   feature targets plus a backfill dry-run against a restored production
   snapshot and must be green.
1. `up --to 0019` applies the shared converged baseline. **Gate**: status
   reports current core version `19`, the three repaired tables have forced RLS,
   the active webhook index is unique, and `outbox_messages.created_at` is
   present, non-null and independently stable across retry/replay schedule
   changes. Duplicate active `(org_id, url)` rows stop the release for explicit
   operator resolution; feature rollout does not proceed in the same job.
2. `up --to 0020` (1a expand).
3. 1b dual-reader digest deploys; **gate**: every control-plane Machine reports exactly the expected 1b digest (or a digest on the explicit compatible allowlist).
4. `up --to 0023` (1c).
5. With producer flags still off, `up --to 0026` lands the additive tenancy,
   `outbox_attachments` and feature-flag schema required by the new binaries.
6. `up --to 0027` lands the domain-grant RLS/locking forward repair while
   retaining row-lock compatibility with any old revoker still draining.
7. Runtime deploys at its digest, flags off; `tools/list` omits `attachments`.
8. Control plane + reconcile Machine deploy at the multi-binary digest.
9. `backfill` runs to zero.
10. `nerve-flags` enables the **canary org**; convergence gate passes.
11. Contract suite runs against the canary org — real inbound mail, real attachment download, real `compose_email` with a PDF to an external mailbox. Pilot org is still off throughout.
12. On green, `nerve-flags` enables the **pilot org**; convergence gate passes; smoke.
13. **The promoted wheel** publishes after the production smoke.

Steps 10–12 are the substitute for a staging chain: the same code, the same
Resend account, the same schema, exercised end to end before the org that
matters is switched on. If step 11 fails, the canary org is switched off and
the pilot org was never on.

### 5. Rollback is bounded, not "flag off"

Flag-off is not a general rollback, and revision 4 implied it was.

- **Compatibility floor**: once `0021` is applied, no binary whose declared
  schema window excludes `0021` may run — it could reintroduce the NULL
  claim-batch outage. The release manifest binds each allowed digest to its
  compiled window; the deploy job checks that binding against the live schema,
  and each binary independently enforces it through
  `NM_MIGRATE_ON_START=verify`. No comparison orders one digest before another.
- **Drain before downgrade**: once attachment-bearing outbox rows exist, an older worker would send messages without their files. Before rolling the worker back, assert zero `queued`/`sending` rows having `outbox_attachments`, and zero `pending` `message_attachments`.
- **Convergence wait**: after flag-off, wait `2 × TTL` before asserting the feature is inert.
- Schema is additive; no down-migration is part of rollback.

### Success Criteria

#### Automated:
- [ ] No deploy workflow has `push`; `workflow_dispatch` requires `confirm=PRODUCTION` and an explicit artifact digest before runtime starts (implemented on PR #8, pending green review/merge)
- [ ] `cloud-production` restricts deploys to protected branches, and `main`
  requires all twelve merge contexts: the legacy five named gates plus
  `codex-review-window`, functional `go-checks`, `sdk-python`, `dashboard`,
  `cloud-e2e`, `exact-mirror`, and `govulncheck`
- [ ] `verify_cloud_deploy_order.sh` asserts the `needs:` graph and fails when an edge is removed
- [ ] `rehearse` is a required predecessor of the production `migrate`; removing that `needs:` edge fails the order check
- [ ] `rehearse` fails on a snapshot older than 24h, and fails when a `--to` step errors against restored data
- [ ] The same control-plane digest and wheel SHA flow through every job — nothing is rebuilt
- [ ] The 1b gate fails when any control-plane Machine digest differs from the expected digest or is absent from the explicit compatible allowlist
- [ ] **Canary isolation**: with the flag on for the canary org and off for the pilot org, a pilot-org `tools/list` omits `attachments` and pilot inbound mail produces no `email.received` delivery
- [ ] **D8**: duplicate global rows are rejected by the partial unique index; precedence env > org > global > default is asserted for all 16 combinations; a DB error resolves off (fail-closed); the convergence gate fails if asserted before `2 × TTL`; `nerve-flags set` is idempotent and writes an audit row; per-org isolation holds (org A on, org B off)
- [ ] **D8 OSS-local**: with `Cloud.Mode=false` and an empty table, flags resolve from env/defaults with **no query issued**
- [ ] Flipping a flag causes **no Machine restart** and **no `runtime.lock` diff**
- [ ] **Rollback**: deploying an artifact whose declared schema window excludes the live DB version is refused; the drain gate fails while attachment-bearing rows are queued
- [ ] `publish-python-sdk.yml` publishes the promoted wheel (SHA verified) and does not rebuild

#### Manual:
- [ ] Canary org exists with a verified receiving subdomain, and inbound mail to it routes to that org
- [ ] Full cutover run: rehearse → baseline `0019` gate → 1a (`0020`) → 1b → exact-digest gate → 1c (`0023`) → tenancy (`0024`) → `0026` → grant-lock repair (`0027`) → runtime → control-plane → backfill → canary on → contract green → pilot on
- [ ] Flags off mid-pilot leaves queued attachment sends deliverable: create a
  bounded `0028` hold for the exact synthetic idempotency key, enqueue one
  attachment message, converge the org flag off, release only that hold,
  prove the external attachment delivery, and restore the org flag.

---

## Phase 8: Post-release

- hermes-cloud pins updated to the landed SHAs (`hermes-cloud/docs/source-pins.md`).
- Runtime cold-start verified gone (first MCP call < 2s after idle).
- Follow-ups filed: drop the legacy `attachments` table after `0023` has been live 30 days; sweep the remaining `401`/`403` call sites; re-evaluate `[[vm]] memory` against observed attachment traffic; backport or formally re-assign the cloud-only store files.
- Deterministic rollback proof is OSS-first: `0028` and store APIs create,
  inspect, expire, and release one exact outbox hold; the cloud-only operator
  command exposes those APIs without ad-hoc SQL. Its TTL is required and
  bounded to 1–30 minutes, so an abandoned drill cannot become an outage.
- The Phase 8 production drill is complete only when evidence contains the
  hold/release replay IDs, the synthetic outbox/message identity, flag-off
  convergence, successful external attachment delivery, and flag restoration.

---

## Testing Strategy

- **Unit**: nullable scan, attachment fingerprint, the reserve sequence, `withTx` in both shapes, flag precedence.
- **Integration** against real Postgres (`NM_TEST_DB_DSN`, `NM_REQUIRE_DB=1`, zero skips): ingest with recorded Resend fixtures, `org_event` → dispatcher → HTTP delivery, mirror worker against a fake Resend, outbox worker against a fake provider, DLQ abandon/replay.
- **Concurrency**: parallel same-SHA upload, parallel quota reservation, competing mirror claims, competing reconcile runs under the advisory lock, budget exhaustion.
- **Compatibility**: the N−1 reader test, a synthetic out-of-window startup
  refusal, the 1b `[0020,0026]` span, and the exact-digest Machine gate (Phase
  2 §1); the D9 API-surface diff; the sync job's post-apply cloud build and
  test.
- **Fault injection**: fan-out failure mid-transaction, partial-recipient failure, mirror truncation, DNS rebinding at dial, quota exhaustion, crash between blob write and ref insert, interrupted backfill resume, slow-body budget hold.
- **Cross-tenant RLS negatives** for `org_events`, `attachment_blobs`, `message_attachments`, `outbox_attachments`, `org_feature_flags`, and the legacy `attachments` deny-all.
- **Regression**: golden payloads for the untouched outbound fan-out; DLQ list/timeline/replay unchanged after a D7 release.

## Migration Notes

- **`0018` is a forward repair, not the event expand step.** It repairs tenant RLS and active-webhook uniqueness for databases that had already recorded version `17`; duplicate active `(org_id, url)` rows must be explicitly resolved before it can apply. `0019_outbox_created_at.sql` is the next converged baseline migration and gives the retained outbox/DLQ history a stable creation timestamp. OSS runtime `v0.0.6` pins their shared core hash and `[19,19]` window.
- **Rollout is expand → dual-read → relax → activate, with `--to` targets** (Phase 2 §1). `0020` expands with `outbox_event_id` still `NOT NULL`; the dual reader declares `[minRequired,maxSupported] = [0020,0026]` and runs in `verify` mode, so it remains compatible through the additive rollout but never applies a migration itself. Only after its exact digest is everywhere does `0021` relax.
- **Step 1c applies through `0023`**, because ingest writes `message_attachments`.
- **`0021` disables non-HTTPS webhooks carrying `email.received`** — deliberate and reversible, because `events` was never validated. Run the preflight and record the result before merging.
- **`0023` leaves the rollout default at `pending_backfill`** and classifies existing rows (outbound and no-`received_email_id` → `known`; recoverable inbound → `pending_backfill`; past retention → `unknown_metadata_expired`). The new writer sets `known` explicitly, so a row created by an old writer during cutover stays visible to backfill. The activation gate is **zero `pending_backfill` in total**.
- **D7: nothing is deleted.** `sent` rows release blob *bytes* 90 days (configurable) after `terminal_at`; `failed` rows retain bytes until explicit abandon via `POST /v1/admin/outbox/{id}/abandon`. Rows, timelines, webhook deliveries, attachment metadata + digests and idempotency tombstones are retained; `chk_outbox_status` is untouched. Pre-existing terminal rows get `terminal_at = now()` for a full grace window.
- **Down-migrations**: `0018` down keeps the corrected version-17 security guarantees; `0019` down removes only the stable-created-time index/column; `0021` down is clean only with no `org_event_id` rows and refuses otherwise. `0022`–`0026` down remove only the new additive feature schema, `0024` additionally refuses while tenancy reconciliation state exists, `0027` refuses while any domain grant exists rather than restoring the RLS-incompatible trigger under active tenancy, and `0028` refuses while any delivery-hold history exists.
- **The `0011`–`0017` backport is a no-op for databases already at version `17`**, but `0018` is their required forward repair. The full tree through `0018` must be byte-identical for `CORE_SCHEMA_HASH` to converge — which requires D9 first.
- **Legacy `attachments`** gets deny-all RLS in `0023`; the drop is a dated follow-up.

## References

- Consumer contract: `hermes-cloud/thoughts/shared/plans/2026-08-02-family-ops-assistant-mvp.md` (Phase 3; blob store at :181)
- Prior art on retention: `thoughts/shared/plans/2026-03-01-resend-inbox-outbox-observability.md:1313`
- Resend: [attachments](https://resend.com/docs/dashboard/receiving/attachments), [pricing](https://resend.com/pricing/) (30-day retention on Free/Pro/Scale)
- Fly: [scheduled Machines](https://fly.io/docs/machines/flyctl/fly-machine-run/#start-a-machine-on-a-schedule), [secrets](https://fly.io/docs/apps/secrets/) (setting secrets restarts Machines)
- Key code — nerve-cloud: `0001_init.sql:64`; `0006_outbox.sql:22`; `0017_org_webhooks.sql:17,20,48,73`; `store/store.go:194-221`; `store/store_threads.go:13-247`; `store/org_webhooks.go:33,184,206,217,286`; `store/outbox.go:101,153,158,277,576,602,644`; `reconcile/service.go:27,37-48`; `cloudapi/handler.go:91-92`; `cloudapi/handler_dlq.go:16,21,59-66`; `cloudapi/resend_webhook.go:180-195,198-368,311,375-427,402`; `cloudapi/handler_webhooks.go:89,109,169`; `cloudapi/handler_messages.go:43,101,107,131`; `cloudapi/handler_inboxes.go:335`; `cloudapi/handler_auth.go:25-42`; `webhooks/dispatcher.go:87,204`; `webhooks/dispatcher_integration_test.go:29-42`; `emailtransport/provider.go:7,34-45`; `emailtransport/outbox_worker.go:12`; `providers/resend/resend_receiving.go:31-37,52-55,102`; `providers/jmap/jmap_inbound.go:7`; `cmd/nerve-control-plane/main.go:50,214-217`; `deploy/cloud/Dockerfile.control-plane`; `scripts/deploy/cloud_deploy.sh:17,26`; `scripts/ci/verify_runtime_lock.sh:21-40`; `scripts/release/generate_runtime_manifest.sh:9-10`; `deploy/cloud/{runtime.lock,fly.runtime.toml,fly.control-plane.toml}`; `.github/workflows/{ci.yml:20-23,runtime-deploy.yml,control-plane-deploy.yml,publish-python-sdk.yml}`; `docs/TENANT_GUIDE.md:16,37`; `docs/REPO_SPLIT_RUNBOOK.md:5-6,26-35`
- Key code — nerve-oss: `internal/mcp/server.go:69,146,151-161`; `internal/tools/service.go:249,285,359`; `internal/store/store.go:203-428`; `internal/store/outbox.go:40`; `internal/jmap/`; `internal/app/app.go:155-158`; `.github/workflows/sync-to-cloud.yml:31-40,63,66-80`
- SDK: `client.py:42,59,60-63,87-91,282,289,292-293,299,308,382-384`; `tools.py:113,124-140`; `pyproject.toml:3`; `__init__.py:14-23,25`; `tests/test_client.py:41`
- Pinned consumer baseline: nerve-cloud @ c85d27c, nerve-oss @ a29da6c

---

## Enhancement History

### 2026-08-04 — Revision 6 (D10: no staging)

Owner asked why a staging machine was needed. It was not — the staging chain was a reflex, and checking what it would actually buy showed it failing at every job it was nominally there for.

**D10: staging dropped.** Phase 7 rewritten. The decisive facts, all verified:

- **It cannot test the backfill at all.** `backfill-attachments` walks inbound messages with a non-empty `received_email_id` and re-fetches envelopes from Resend. A staging database has no such rows, and Resend holds no envelopes for them. This was the single riskiest step in the cutover and staging was structurally incapable of exercising it.
- **It is worse than CI at migration sequencing.** The `0019`/`0020` batch-poisoning risk is covered by the N−1 test and the `maxSupported` start-refusal (Phase 0 §8, Phase 2 §1), both against a disposable Postgres. Staging adds nothing unless its database is production-shaped — at which point it is a snapshot restore, which is what replaced it.
- **The contract suite needs production anyway.** Real Resend account, verified domain, real inbound mail, real external mailbox. Staging would need a second verified domain, webhook endpoint and DNS set, and still be less faithful.
- **The real concern — not breaking the pilot — is already solved by D8.** Per-org flags let a canary org exercise the whole path in production while the pilot org stays off.
- **"Does it boot" is covered by Fly.** `fly.control-plane.toml` already declares `/healthz` checks with a grace period, so a broken machine never takes traffic and the deploy rolls back.

Replaced by: a **snapshot rehearsal** job (restore production backup → ephemeral DB → replay every `--to` step → `backfill --dry-run` → assert → drop), required as a `needs:` predecessor of the production `migrate`; a **canary org** behind the D8 flag, enabled and contract-tested before the pilot org is switched on; and the CI integration suite. Cost: one verified subdomain, versus two Fly apps, a second Postgres and a second domain.

**Production gating corrected to what is actually enforceable.** Revision 5 assumed a required reviewer on the production environment. This repo is private on a plan without protection rules — `required_reviewers` and `wait_timer` both return 422, while `deployment_branch_policy` succeeds. So the gate is: `cloud-production` restricted to protected branches, `main` requiring `lint`/`unit`/`integration`/`e2e`/`coverage`/`codex-review-window`, plus a typed `confirm=PRODUCTION`. That is a stronger CI gate than a rubber-stamp approval, but there is **no human-approval step**; adding required PR approvals on `main` is the cheap fix and is recommended, not required.

**Implemented and pushed** (PR #8): Phase 0 §1 in full, plus Phase 0 §7 — `runtime.lock` digest-pinned, `CLOUD_SCHEMA_HASH` added, `verify_runtime_lock.sh` unbroken (it was failing on `main` on a stale `CORE_SCHEMA_HASH`) and extended with a non-circular registry digest resolution. The `if: vars.FLY_… != ''` job guards were removed: with deploys no longer push-triggered, a self-skipping job in a `needs:` chain reports success while deploying nothing.

### 2026-08-03 — Revision 5 (fourth-pass review)

Owner sign-offs carried forward: **D7** (release bytes, keep history), **REST delegation** over an MCP contract bump, **D8** direction. All 13 P1 gaps verified; **none rejected**. **D9** added.

| # | Gap | Resolution |
|---|---|---|
| 1 | Cutover still collapses migrations | Phase 0 §5, Phase 2 §1, Phase 7 §2. `nerve-migrate up --to <version>` added, each rollout step pins a target, and — the structural fix — `store.Migrate` at startup (`main.go:50`) becomes `NM_MIGRATE_ON_START=verify` in cloud, with `minRequired`/`maxSupported` compiled per binary. The 1b dual reader declares `[0019,0024]`, remains compatible through the additive rollout, and applies nothing in `verify`; an exact-digest Machines-API gate asserts no pre-1b control-plane instance remains before 1c. |
| 2 | OSS backport does not compile | **D9** — Phase 0 §3. Confirmed: Cloud's `store_threads.go:13-247` declares 14 methods OSS still holds in `store.go:203-428`. Split OSS `store.go` to mirror Cloud's layout in its own behaviour-preserving PR, verified by a `go doc -all` API-surface diff with zero test changes, and enumerate `listing.go`/webhook dependencies in the same PR rather than discovering them mid-backport. |
| 3 | Sync manifest does not express ownership | Phase 0 §4. Replaced one flat list with three: **exact-mirror** (byte-identical, CI-asserted — now including `store.go`, without which Cloud never receives `inTx`, plus `memguard`), **patch-synced** (`internal/cloudapi/**`, which cannot be byte-identical since several handlers are cloud-only), and **cloud-only** (those handlers, the billing/org/token/usage store files, `internal/attachments/**`, `cmd/**`). Triggers are computed from the manifest instead of three hardcoded filters, and `internal/jmap` is **bootstrapped wholesale** rather than diffed against `HEAD~1`, since cloud has none of its files. |
| 4 | Reserve SQL has two correctness races | Phase 3 §2. Both confirmed. **(a)** All CTEs share the statement-start snapshot, so a second same-SHA upload blocking on the usage upsert still sees no blob when it proceeds and hits a unique violation — fixed by splitting into sequenced statements, with `SELECT … FOR UPDATE` serializing the org's quota first so the next statement gets a fresh snapshot. **(b)** `ref_count` moved off the blob statement entirely: it is now driven **only** by `AFTER INSERT`/`AFTER UPDATE OF blob_sha256`/`AFTER DELETE` triggers on the reference rows, so a duplicate ingest (which inserts nothing) cannot bump it. |
| 5 | Outbound enqueue violates FK and dedup | Phase 4 §3. Confirmed — `outbox_attachments` has an immediate FK to `outbox_messages`, and revision 4 inserted refs first. Reordered: resolve-or-insert the parent returning `{id, inserted, fingerprintMatch}`; blobs and refs only when `inserted`; a key reused with a different fingerprint returns typed `idempotency_conflict` instead of the old message; the suppression path (`outbox.go:153`) goes through the same helper and sets `terminal_at`; zero-byte attachments are rejected with typed `attachment_empty` before SQL, so the `size_bytes > 0` CHECK is never reached. |
| 6 | Metadata rollout creates invisible loss | Phase 3 §3, §4. Confirmed: flipping the default to `known` in `0022` meant the old writer — live between migration and control-plane deploy — would create metadata-less rows already marked `known` and invisible to backfill. The rollout default now **stays** `pending_backfill`; the new writer sets `known` explicitly in the ingest transaction. Existing rows are classified three ways so outbound and no-`received_email_id` rows do not strand the gate forever, and the gate became **zero `pending_backfill` in total**. Step 1c's migration target moved to `0022` in the step table itself, not just prose. |
| 7 | D7 has no working release path | Phase 3 §6. Confirmed on both counts. Release retains the row and nulls `blob_sha256`, which an `AFTER DELETE` trigger never sees — added `AFTER UPDATE OF blob_sha256`. Abandon is now a real surface: `POST /v1/admin/outbox/{id}/abandon` alongside the existing routes (`handler_dlq.go:59-66`), scope `nerve:admin.deliverability` (:21), `withTx`, `409` on non-`failed`, idempotent, audited, serialized by `SELECT … FOR UPDATE`. And the impossible criterion is fixed: a `sent` row is **not** in `/failed` (which is failed-only), so its history is verified via `/{id}` and `/{id}/events`, and typed replay failure is verified on an **abandoned failed** row. |
| 8 | Memory gate misses the main consumers | Phase 1 §2. Confirmed. Extended from two sites to four: the **REST download** acquires before `SELECT content` (a `bytea` read materialises the whole blob) and holds until the response is written; the **outbox worker** lazy-loads one message at a time rather than a 10-message batch. `Content-Length` is treated as untrusted — reserve `min(declared, cap)` and true up incrementally, aborting mid-read on exhaustion. And since both servers set only `ReadHeaderTimeout: 5s` (`main.go:214-217`, `app.go:155-158`), `ReadTimeout` plus a body read deadline were added so a trickling client cannot pin the budget. |
| 9 | Reconcile Machine still has no image | Phase 0 §9. Confirmed — `Dockerfile.control-plane` builds only `/app/nerve-control-plane`. Now a multi-binary image (one digest covering both binaries) with a command override on the Machine. And the advisory lock moved off the pool: a session-scoped `pg_try_advisory_lock` taken through `*sql.DB` lands on an arbitrary connection that may be returned mid-run, so it is now held on a dedicated `*sql.Conn` for the run's duration, asserted via `pg_locks`. |
| 10 | Release depends on a not-yet-built SDK | Phases reordered — SDK is now **Phase 6**, release **Phase 7**. The wheel is built once, its SHA recorded, and that exact file is installed by the canary contract suite and published; `publish-python-sdk.yml` changes from `python -m build` to promoting the artifact. `control-plane-deploy.yml` becomes deploy-only, taking a digest instead of rebuilding per target. `min_machines_running = 1` moved into Phase 6 so it precedes the runtime deploy. |
| 11 | Flag-off is not a full rollback | Phase 7 §5. Confirmed. Added a **schema compatibility floor** bound to each allowed artifact digest and enforced by both the deploy job and each binary's verify window; hashes themselves are never ordered. A **drain gate** asserts zero queued attachment-bearing rows before a worker downgrade, and a `2 × TTL` convergence wait follows flag-off. Direct `workflow_dispatch` independently requires the protected-branch gate, `confirm=PRODUCTION`, and a pinned digest; this repository tier has no human-approval gate. |
| 12 | D8 is not an executable contract | Phase 7 §3. Schema moved to **core**, because the runtime is OSS and would not find a cloud-migration table in a self-hosted schema. Specified: two partial unique indexes (a plain `UNIQUE (org_id, flag)` does not constrain NULL `org_id`, so duplicate global rows would make precedence non-deterministic), FK/index/RLS, four-level precedence with a tri-state env override, a `(flag, org_id)` cache key with **fail-closed** DB-error handling, a `2 × TTL + 5s` convergence gate, and `nerve-flags` as the writer — a CLI on the migrate job's DSN rather than a new authenticated endpoint plus a CI-held admin key. OSS-local resolves from env/defaults with **no query**, tested explicitly. |
| 13 | `client.rest` leaks a second pool | Phase 6 §1. Confirmed — `close()` closes only `self._http` (`client.py:382-384`). `client.rest` is a lazy singleton owned by `NerveClient`; `close()` closes both in `try/finally` and is double-close safe; `NerveRestClient` is independently an async context manager. `NerveRestClient`, `NerveConfigurationError` and the three attachment exceptions are added to `__all__` (currently 8 names, `__init__.py:14-23`), with lifecycle and leak tests. |

Structural changes: 8 phases → 9; SDK moved before release; migrations now version-targeted with a compiled compatibility window per binary; refcount became trigger-driven; the sync manifest became three ownership lists.
