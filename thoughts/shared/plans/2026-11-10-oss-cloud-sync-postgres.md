---
date: 2026-11-10T22:51:56+00:00
repository: nerve-oss
scope_repositories: [nerve-oss, nerve-cloud]
status: in-progress
git_commit: 79b1c6f6af2408b9d30319b6a1a563a1861b30cf
branch: codex/sync-cloud-postgres-fixture
---

# Recover the normal OSS → Cloud sync delivery

This is a bounded delivery repair under the user's instruction to finish the
cross-repository OSS → Cloud plan and deploy it. The parent roadmap is
`nerve-cloud/thoughts/shared/plans/2026-09-08-oss-open-runtime-waves.md`, F/G sync
producer scope. This change does not itself deploy a runtime or change a
production database, credential, permission, provider or source lock.

## Observed failure

[Sync run 42917401590](https://github.com/dsmolchanov/nerve-oss/actions/runs/42917401590)
for merged OSS PR167/source79b1c6f builds Cloud, then fails before PR creation:
billing startup, offer replay and API-role migration tests require PostgreSQL,
but the producer starts none. The same tests must continue running.

A repair-only subsequent push also exposes a lost-delta problem: comparing its
head only to `github.event.before` sees no new shared files, although Cloud still
pins the commit before the failed shared push. Re-running the old run retains
its old workflow definition and does not deliver the repair.

## Exact scope

- `.github/workflows/sync-to-cloud.yml`: isolated PostgreSQL16 service with a
  health check and random host port; the existing Cloud build/test step receives
  `NM_REQUIRE_DB=1`, `NM_TEST_DB_DSN` and UTC. Keep all tests, use Cloud CI's
  30-minute Go timeout and bound the job at45minutes. Derive the diff base from
  adopted Cloud source authority rather than the preceding push. Preserve
  checkout permissions, exact mirror checks and generated PR/source-lock path.
- `scripts/sync/resolve_cloud_base.sh`: read exactly one canonical40-hex
  `OSS_SOURCE_REVISION` from checked-out Cloud. Require both commits to exist and
  the adopted base to be an ancestor of the trusted current OSS head. Unknown,
  malformed, duplicate, missing or divergent authority refuses without reset.
- `scripts/ci/test_sync_manifest.sh`: simulate failed shared delivery followed by
  a workflow-only repair. Prove the old event base loses that delta and the
  adopted base delivers it, preserving Cloud-owned bytes. Cover invalid/missing,
  extra-field, duplicate, unavailable, descendant and unrelated authority.
- This plan records the workflow scope for review.

No dependency, skip, weaker assertion, production SQL, deployment, direct lock
advance or alternative sync path is introduced.

## Verification and delivery

- [x] Existing sync manifest/conflict/ownership regression passes with the new
  real Git-history recovery and refusal cases.
- [x] Shell syntax, parsed YAML/service/DSN binding and whitespace checks pass.
- [ ] Exact-head OSS CI, Security and Codex review pass before merge.
- [ ] Normal repaired push detects the outstanding shared files from Cloud's
  adopted pin and passes the full Cloud build/tests against PostgreSQL.
- [ ] Generated Cloud PR is checked/reviewed and accepted; source authority
  comes only from that normal producer result.

A green source sync is not a published image or production hybrid acceptance.
