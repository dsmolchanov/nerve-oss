# Validation: shared fixture role cleanup order

Plan: `thoughts/shared/plans/2026-11-11-test-role-cleanup-order.md`.
Validated 2026-11-11 04:32 UTC; base `c57983fbca69d64be8c3a07cc2499a9f872b239e`.
Status: local verification passed; review/required CI/merge not yet run.

## Scope

Actual Cloud producer40P01 and independent PUBLIC-policy control show this class
also reaches ordinary ACL-only role cleanup. Complete cross-repo Store audit:
Cloud28 sites, OSS5 exact mirrors. All five existing shared sites use one private
test-only relation-first transaction. Existing UUID creation, DSN hook/registry,
restricted-session close, database lifetime, original DROP ROLE IF EXISTS where
present and all RLS/functional assertions remain intact. Cleanup failures now
propagate. New helper file is one additional exact-mirror path; no existing
exemption, product API, SQL, workflow, AGENTS, dependency or production change.

## Actual verification

Artifacts in `/private/tmp/oss-cloud-production-20261110/`:

- `oss-role-cleanup-check-56d7fe7f55514203bbd63facae2223f7-proof.json`:
 11 selected families,23 PASS events,0 FAIL/0 SKIP on actual owned PG16.
 Includes five shared RLS fixture families and every openDomainGrantAppStore
 caller, existing nested source/DSN/cleanup regression, SELECT/UPDATE/PUBLIC ACL
 old-order40P01/new-order VACUUM completion, actual wrapper catalog lock barrier
 with a quoted non-public table, failed-statement atomic policy/role rollback.
 Own non-default DB/roles0|0, exact owned container absence independently checked.
- `oss-role-cleanup-no-lock-mutation-proof.json`: explicit Go build overlay
 removes only relation-lock call in new helper. Actual wrapper regression fails
 at expected runtime assertion,0 skips, own DB/roles0|0, exact container absent.
 Workspace unchanged; this is expected RED, not a passing suite.
- go vet ./internal/store and go build ./... PASS. Manifest parses as JSON;
 git diff --check PASS. All five changes limited to cleanup; no DSN registry or
 consumer behavior edit. Private helper/regressions match the parallel Cloud
 implementation byte-for-byte after symbol-name mapping, avoiding drift.
- `role-cleanup-final-oss-source.json` records changed tracked source hashes;
 `oss-role-cleanup-selection.json` records exact original test inventory.

Normal exact-head OSS review/required CI and merge remain required, followed by
ordinary generated Cloud source sync against accepted Cloud cleanup source.
No manual shared-file copy or Cloud pin change. Later Cloud integration can
share this adopted primitive; no generic helper may be hand-copied first.
These local results do not prove full source producer, release or production
acceptance. Full production roadmap/provider-complete drain remain open.
