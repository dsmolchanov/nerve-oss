---
date: 2026-11-11
repository: nerve-oss
status: in-progress
source_commit: c57983fbca69d64be8c3a07cc2499a9f872b239e
---

# Keep ordinary Cloud source qualification safe for owned fixtures

Full Cloud catalog stageB introduces job-owned PG16 fixtures and an intent/CID
ledger. The ordinary OSS→Cloud producer executes that materialized Cloud suite,
so it must run the reviewed Cloud sweeper even after test failure/package alarm,
before publishing a PR. This change prepares that consumer; it does not adopt
or publish incomplete Cloud source, start production resources or approve keys.

A workflow-only source commit can currently be skipped by has_changes even
though adopted OSS source SHA differs. Signed runtime B/C/candidate validation
binds exact source/run SHA, so keep normal authority progression reviewable:
if adopted base!=HEAD, run the unchanged exact-mirror/collector/build/test gates
and generate an ordinary qualified pin-only PR even when shared bytes match.
No manual pin change, source override, weakened exact-head check or fake proof.
An already adopted HEAD with no shared delta remains a no-op.

## Source scope

- .github/workflows/sync-to-cloud.yml: replace fixed /tmp changed-file report
  with RUNNER_TEMP job-owned output; fail every apply/build error explicitly;
  has_changes includes validated source advancement; bounded always() cleanup
  before CreatePR. Use only materialized Cloud's existing reviewed sweeper;
  older Cloud without sweeper and without ledger succeeds, but an existing
  ledger without sweeper refuses publication. Never prune/delete by prefix.
- scripts/ci/test_sync_cloud_owned_fixtures.py: run actual extracted shell;
  verify shared-change/pin-only/already-current decisions, fail-closed resolver/
  materializer/build errors, exact full Go commands, cleanup success/failure,
  absent old-compatible script/ledger and missing-sweeper refusal; actual wiring
  and cleanup-before-publication/always/timeout checks.
- .github/workflows/ci.yml: invoke the new bounded stdlib-only contract.
- This plan and matching validation report.

No product/API/SQL/AGENTS/ownership/dependency/timeout change. No rewrite of
existing collector/manifest/resolver/materializer contracts. Do not widen
171/518: this is a separate bounded prerequisite with normal review/CI.

## Required evidence

New actual-shell tests plus existing collector, source-base and sync contracts;
YAML parse/diff checks. Prove publication can only follow successful Go and
successful cleanup, and no-helper+ledger cannot become success. Existing owned
Cloud sweeper's actual PG16 interruption/identity/absence qualification remains
in its own source report; shell doubles here verify invocation and refusal,
not a new claim of actual Docker lifecycle or production acceptance.
Normal review/all required OSS CI/merge; any resulting generated source pin
uses the ordinary full producer and normal Cloud review/required CI.

## Local checkpoint — 2026-11-11 04:46 UTC

Implementation and local shell/YAML verification complete. New6 + existing
collector3 PASS; existing sync resolver/materializer/manifest contract PASS.
All three boundary mutants refuse. No new Docker/full-suite/prod claim; see
matching validation report. Normal review/required CI/merge pending.
