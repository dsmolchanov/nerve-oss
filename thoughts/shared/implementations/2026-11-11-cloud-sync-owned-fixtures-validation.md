# Validation: ordinary Cloud sync owned-fixture cleanup

Plan: `thoughts/shared/plans/2026-11-11-cloud-sync-owned-fixtures.md`.
Validated 2026-11-11 04:46 UTC; source basec57983fbca69d64be8c3a07cc2499a9f872b239e.
Status: local contracts passed; normal exact-head review/required CI pending.

## Result

The ordinary producer now invokes materialized Cloud's existing owned-PG sweeper
under bounded always(), immediately before guarded PR creation. Cleanup failure
blocks publication; older Cloud without sweeper succeeds only without a ledger,
including refusal for dangling ledger symlinks. No prefix removal/prune, foreign
resource deletion or substitute implementation. This prepares the full Cloud
fixture stageB; it does not publish/adopt that incomplete source.

Changed-file output lives in RUNNER_TEMP. A validated adopted source SHA that
differs from HEAD now runs the full existing exact-mirror/collector/build/test
sequence and creates an ordinary reviewed authority-pin PR even without shared
byte changes. An already adopted head/no shared delta remains a no-op. Source-
only commits therefore incur full qualification rather than an early no-op;
this is deliberate exact-head provenance, not an authority override. Signed
runtime/candidate evidence requires matching source SHA, not merely equal code.
Build/apply failures propagate; original uncached full Go argv/timeouts remain.
PR body accurately describes source-only advances and completed cleanup.

## Verification

- New stdlib actual-shell contracts:6 PASS. Covers shared/pin-only/current head,
 resolver/materializer failure, complete Go argv and build/test refusal, exact
 sweeper invocation/failure, old-Cloud compatibility, missing sweeper+ledger
 refusal, unchanged ledger/symlink, actual always()/timeout/order/CI wiring.
- Existing collector contracts:3 PASS, including original 12 setup refusals,
 complete Go argv and stale-port exclusion. Existing sync manifest/resolver/
 materializer contracts PASS, including seven base refusals, 3-way conflict and
 cloud-only ownership refusals. Neither script was weakened or rewritten.
- Both YAML files parse; actual cleanup is penultimate, bounded3m, always(),
 followed by ordinary success-guarded CreatePR; git diff --check PASS.
- Three workflow mutants (source-only branch removed, always removed, sweeper
 call removed) refuse the relevant contract. Current no-call mutant fails the
 precise invocation assertion, with no incidental file-read/compile error.
 Artifacts: /private/tmp/oss-cloud-production-20261110/sync-owned-fixtures-*.
 Initial local path-double failures came from macOS /var versus /private/var;
 canonicalizing only the temporary test root fixed the double's identity.

These tests use explicit command/filesystem doubles for workflow invocation.
They are not new Docker lifecycle, interruption, full Go-suite or production
proof. Actual owned-PG interruption/identity/absence evidence remains in the
Cloud foundation source/report; qualify the complete source in normal required
CI before promotion. No product/API/SQL/AGENTS/ownership/dependency or production
change. Normal review/CI/merge and generated Cloud source acceptance still open.
