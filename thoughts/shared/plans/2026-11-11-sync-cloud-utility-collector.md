---
date: 2026-11-11
repository: nerve-oss
status: approved
source_commit: eff664a49478bf4680b575617285d8d3bb3a640b
parent: thoughts/shared/plans/2026-09-08-oss-open-runtime-waves.md
---

# Qualify the Cloud utility collector before ordinary source synchronization

The normal producer42947126103 failed eight required credential statistics
cases after Cloud491's full source was accepted. Its service has PG16 but lacks
shared_preload_libraries=pg_stat_statements. Store finished1192.946s; this is
not a timeout or a regression in the accepted test-only DSN contract. No Cloud
PR/pin was produced; do not bypass those tests, hand-copy files or rerun unchanged.

Scope for the authorized complete-plan CI remediation:
- .github/workflows/sync-to-cloud.yml: configure only this job's own PG16 service
  before the full Cloud build/tests; validate its exact service CID and dynamic
  port, await TCP readiness, ALTER SYSTEM preload, restart that exact container,
  read and range-check its actual rebound loopback port, positively qualify
  preload/track/utility and export the actual NM_TEST_DB_DSN through GITHUB_ENV.
  The build step must not override it with the stale service-context port.
  Keep required DB, full uncached30m Go test,45m job, source-base resolver,
  byte-mirror check and normal generated pin/PR ordering. No product/source/SQL
  change or production/provider connection. Bound the setup step to three minutes.
- scripts/ci/test_sync_cloud_utility_collector.py: execute the real setup shell
  with explicit psql/Docker/sleep doubles; check exact container and rebound
  endpoint, full actual Go build/test argv, failure/refusal boundaries and the
  same has_changes guard/order. No third-party test dependency.
- .github/workflows/ci.yml: execute that contract in the existing required
  build-test job; no existing check is removed or weakened.
- This plan and its validation report under thoughts/shared.

Verify actual shell contracts and scoped real credential tests after executing
that same setup against an owned initially unconfigured PG16 fixture. Verify
owned database/role/container cleanup, normal review/all required OSS CI and
exact main-push producer. The actual eight commit/rollback statistics cases and
positive collector control remain mandatory. Preserve the failed run/log.
Normal source-base resolution must still deliver the previously unadopted169
four-test-file delta on the next producer; never replace it with event.before.
No CODEOWNERS or thoughts/shared/tests artifact exists on this source.
