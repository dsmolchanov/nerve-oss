---
repository: nerve-oss
status: in_progress
source_revision: 3946695
---

# Slice A recipient ledger foundation

User authorized preparation of slice A in parallel with phase 0. This bounded
OSS counterpart implements only the additive org-period accounting foundation
from nerve-cloud's approved 2026-09-08-mcp2-pricing-v2 plan, slice A item 2.

Scope: internal/store/migrations/core/0030_recipient_ledger.sql,
internal/store/recipient_ledger.go, internal/store/recipient_ledger_test.go and
internal/store/migration_test.go, sync-manifest.yaml (new ledger files in
exact-mirror ownership) and this plan. The historical 15-to-29 test
uses an explicit target instead of latest so its existing assertions remain
valid after adding 30. Core head advances 29 to 30; no historical migration changes.

A period is an explicit authority-issued UUID and finite [start,end). Core has
no quota account, offers, Stripe, domain pool or locally renewed paid period.
NULL allowance is unlimited and zero is exhausted. Install replay must match
boundaries and limit exactly; later upgrades need a distinct projection change.
Reservations bind org/outbox to one period and quantity. Unknown stays reserved;
commit and release are terminal and idempotent. Durable ledger rows block org
cascade deletion and down migration, and survive outbox retention. Forced RLS
and tenant predicates follow the existing Core tenant contract.

Transaction-only methods hold the org-period lock until the caller commits or
rolls back. A future enqueue caller must establish org/outbox ownership, apply
live policy/lifecycle/period fences and share the same transaction. No production
callsite is activated by this foundation; policy v1 and legacy units remain.
There is no claim that hosted billing, quota enforcement, inbound/storage,
backfill, or slice A itself is complete. Cloud exact mirror must use an accepted
OSS revision; this branch must not be treated as an accepted source pin.

Verification obligations:
- [x] Fresh migration and populated 29-to-30 upgrade preserve legacy data.
- [x] Empty down works; ledger data refuses down and org deletion.
- [x] 100 concurrent reservations at allowance 10 accept exactly 10.
- [x] Replay/conflicts, caller rollback, unknown across closure/expiry and
      definitive terminal races preserve counters and identity.
- [x] Zero/NULL, invalid temporal data, tenant RLS and composite identity.
- [ ] Existing store and runtime schema-window tests remain green, no SKIP.

Rehearsal/manual acceptance and release activation remain outside this increment.


## Local validation 2026-09-10

PostgreSQL 16 disposable databases: all eight recipient-ledger suites passed
with the race detector and no SKIP, including 100 concurrent requests for ten
remaining recipients. Historical 15-to-29 regression passed after explicit
historical targeting. Startup, neuralmaild, nerve-migrate and release tests passed.
Candidate contract and exact-mirror inventory checks passed.

The full store race run had one failure in the unchanged attachment retention
test comparing the database timestamp to the host clock. Its isolated rerun,
combined with all recipient suites and the historical migration test, passed.
This is recorded as a full-run limitation; no outbox change is included.

These are local schema/primitive tests, not a signed release rehearsal or proof
of active commercial quota enforcement. Migration 30 must be adopted through
accepted source, signed compatible artifacts and phase-0 admission.


Review correction: admission rechecks closure and PostgreSQL time in the final
counter UPDATE after the period lock is acquired. A deterministic lock-wait
regression reproduced a stale pre-lock time projection on the prior predicate:
waiting across ends_at incorrectly succeeded. With the final predicate, all
eight ledger suites pass under the race detector and the waiting reservation
is rejected without changing ledger/counters.


## Candidate consumer compatibility

Additional approved scope: `scripts/release/select_runtime_candidate_contract.sh`
and `scripts/ci/test_runtime_candidate_contract.sh`. The historical v1 candidate
must refuse a source catalog beyond its fixed Core29 window. The generic runtime
manifest generator currently hashes the entire catalog and does not independently
reject Core30 with v1/window29, so this candidate-only guard runs before protected
candidate build/publication. It does not change tag-release behavior or widen
any historical window. Tests prove valid historical29 selection, explicit30
refusal and unchanged v2 B/C selection.

The unchanged optional historical `Validate exact R0 authority` CI check refuses
Core30 by design. It is not a required branch check. Preparing a test-only legacy
workflow boundary was rejected by automatic approval review as outside the user's
legacy scope; those edits were preserved separately for review and removed from
this PR. No legacy verifier, producer or workflow is changed. This report does
not describe every CI check as green.

Candidate guard verification: the updated dispatch suite passes. Running that
same suite against the previous helper in an isolated temporary fixture fails
with `historical candidate accepted Core30 source`, confirming the negative
regression. Existing candidate-version absence/auth-failure probes pass.
