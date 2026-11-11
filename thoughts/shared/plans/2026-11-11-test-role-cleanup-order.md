---
date: 2026-11-11
repository: nerve-oss
status: in-progress
source_commit: c57983fbca69d64be8c3a07cc2499a9f872b239e
---

# Order fixture relation locks before owned-role cleanup

Ordinary producer42952766974 failed on Cloud DROP OWNED/autovacuum40P01, leaving
one role and84 later fixture refusals. Actual PG16 old/new order experiments
proved relation-first cleanup; a PUBLIC-policy ACL control also reproduced40P01,
so PUBLIC policy is not a safe boundary. Complete Cloud inventory33 sites has
28 Cloud-owned sites and these five OSS exact mirrors. Fix upstream, never
hand-copy shared files or advance Cloud's source pin manually.

## Source scope

- internal/store/fixture_role_cleanup_test.go: private test-only transactional
  helper, stable OID relation-first locking across non-system fixture schemas,
  actual PG16 SELECT/UPDATE/PUBLIC-policy VACUUM regressions, actual wrapper
  catalog barrier with quoted schema/table, failed-DDL atomic rollback.
- internal/store/migration_test.go, feature_flags_test.go, email_tenancy_test.go,
  attachment_migration_test.go, recipient_ledger_test.go: preserve current DSN
  registry/hook, every UUID creation/role guard, restricted connection close,
  callback lifetime and assertions. Group original DROP OWNED + DROP ROLE SQL in
  one helper transaction; propagate cleanup errors previously discarded.
- sync-manifest.yaml: add only the new helper test file as exact-mirror, next to
  migration_test.go; no existing exemptions. Matching plan/validation report.

No product API, migrations, .github, AGENTS, credentials/provider/holder changes.
Keep independent Cloud-owned sibling patch reviewable from its accepted main.
When this new helper is adopted normally, Cloud can delegate its shim to this
shared primitive in a separately qualified integration change; no divergent
manual mirrored helper edit. Full fixture isolation remains separate work.

## Required evidence

Actual PG16 all new lock/rollback cases and the five existing RLS test families,
no failures/skips, all owned DB/roles0|0 and exact owned container absence.
A mutation removing relation-first call must fail the actual wrapper test, not
compilation, while preserving cleanup. go vet/build/diff/manifest validation.
Normal OSS review/required CI/merge, then ordinary generated Cloud source sync
against newly accepted Cloud cleanup source. No retry-only fix, skipped test,
disabled autovacuum, foreign-role deletion or manual pin. Production acceptance
and provider_complete remain false.

## Local verification checkpoint — 2026-11-11 04:32 UTC

Cloud first13-file scope981 PASS; additional8-file/final regressions189 PASS.
OSS five shared sites/current regressions23 PASS. All zero FAIL/SKIP and actual
owned DB/roles0|0/container absence. Both no-lock wrapper mutants fail the exact
runtime assertion and clean their owned resources. Local static checks passed;
normal head-bound review/full required CI/merge and ordinary generated adoption
remain pending. See matching validation report for exact source/run provenance.
