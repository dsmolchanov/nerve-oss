---
date: 2026-11-11
repository: nerve-oss
status: approved
source_commit: 1c2a6a0970c5ffb6bdd0b6ebea0a0004822a2f2a
parent: thoughts/shared/plans/2026-09-08-oss-open-runtime-waves.md
---

# Keep fixture connection identity when Cloud isolates database clusters

The shared Store helper owns a UUID database until t.Cleanup, and some tests
return Stores or nest another database. Cloud's forthcoming complete service
catalog has eighteen fixed cluster-wide roles; database names alone cannot
isolate those roles. Cloud must allocate physically isolated test clusters.

This minimal upstream step supplies only a test-only source seam; it does not
start Docker, change OSS fixture requirements or implement Cloud isolation.
It is part of the authorized full OSS-to-Cloud continuation.

Exact scope:
- internal/store/migration_test.go: an unexported provider variable compiled
  only into this package's test binary, nil by default. withTempDatabase keeps
  its exact default NM_TEST_DB_DSN/default URL behavior when nil, otherwise
  consumes one nonempty source supplied by Cloud's test-only initializer.
  Record each actual opened database handle's fixture DSN in a concurrency-safe
  test-only map until its existing cleanup. Provide a strict unexported getter;
  unknown handles fail rather than borrowing process/global authority.
- internal/store/{migration,feature_flags,email_tenancy,attachment_migration}_test.go:
  the existing restricted-role reconnect consumers use their registered fixture
  DSN before changing the same database path/user/password as before. Preserve
  every tenant/permission assertion and owned role cleanup.
- migration_test.go regressions: wrap the original provider so the same tests
  also exercise the eventual Cloud provider; verify one allocation per fixture,
  nested distinct database identity, reconnection, post-callback lifetime and
  cleanup removal/closed handles after the subtest ends. No t.Setenv/global DSN
  mutation; tests are sequential and restore the temporary provider.
- This plan and a matching validation report under thoughts/shared.

No production file, public API, shared migrate.go, sync manifest, .github,
AGENTS, SQL or dependency change. The four test files already belong to the
exact-mirror manifest; normal producer synchronization is mandatory. No local
Cloud mirror edits or owner exceptions. New source pin/release provenance will
be required after acceptance; old1c2a6a09 candidate proofs do not authenticate a
future source revision.

Verification: real PG16 selected fixture and unchanged tenant/RLS reconnect
cases without skips; nested lifetimes and closed cleanup; go build ./...,
Store vet, diff/manifest scope; normal review/required OSS CI and normal generated
Cloud sync. Cloud-specific Docker fixture, all remaining Cloud DSN consumers,
full timezone isolation/cleanup/capacity, fourteen-component numbered promotion,
Bridge/Target and production rollout remain separate subsequent obligations.
