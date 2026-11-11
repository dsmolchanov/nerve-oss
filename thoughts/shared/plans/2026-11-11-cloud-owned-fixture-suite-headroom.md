---
repository: nerve-oss
status: in-progress
created: 2026-11-11T09:14:03Z
source_commit: ae35a7e77b8a9582eede66cafb425eaf89fbc6f2
parent_plan: 2026-09-08-oss-open-runtime-waves.md
---

# Bound complete Cloud fixture qualification at every enclosing deadline

## Evidence and purpose

Cloud PR521 head5ffb2704, run42980490844, job130389511521, failed
both Store packages at the finite45m alarm (2700.015/2700.017s).
12514 PASS events, no visible named assertion failures/skips; the suite is
incomplete. The always cleanup removed2 survivors successfully. This is not
acceptance of the full suite or complete capacity evidence.

The same unchanged fixture tree completed locally in2974.667/2972.457s.
The local prefix to the hosted active family took1460.775/1459.791s;
scaling by the observed hosted2700s estimates about5498s (91.6min) for
Store. This is a sizing estimate, not a hosted completion result. Choose a
finite120m per-package alarm,180min build/test step and210min job, leaving
30min outside the test step for setup, checks and unconditional cleanup.
The producer previously had a30m package and45min enclosing job; both
must support the same physical fixture workload as the Cloud caller.

This bounded adjustment follows the user's instruction to finish the plan;
it does not claim separate human approval of these particular numbers.

## Explicit scope

- `.github/workflows/sync-to-cloud.yml`: package120m, build/test step180min, owning job210min.
- `scripts/ci/test_sync_cloud_owned_fixtures.py`: preserve actual shell qualification; update the finite budget expectation and negative mutations.
- `scripts/ci/test_sync_cloud_utility_collector.py`: preserve actual shell qualification; update the finite budget expectation and negative mutations.
- `AGENTS.repo-invariants.md`: recurring whole-suite deadline nesting rule,
  enforced by the existing mandatory Python contract.
- This plan and matching implementation validation report.

No other workflow, dependency, product/context/assertion deadline, migration,
source mirror, lock, holder, provider, credential or production resource change.
No test filtering, cache acceptance, skip acceptance or reused fixture role.
Cloud retains both UTC and Europe/Prague. Producer retains full build and test,
required DB, collector/rebinding, source-only advancement and cleanup-before-PR.

## Execution and acceptance

- [x] Implement the explicit finite nested budgets and recurring rule.
- [x] Execute the existing mandatory contract file(s), including complete shell
      commands, build/test failure propagation, cleanup and publication guards.
- [x] Negative mutations reject absent/zero/shortened package budgets, cached or
      filtered suites, absent/shortened/overlapping enclosing deadlines.
- [x] Diff checks show no unrelated source or contract weakening.
- [ ] Exact-head hosted CI and review accepted through the normal PR gate.

The OSS prerequisite must land first and produce its normal qualified Cloud
source-only sync. Never hand-edit the lock. Compose the Cloud adjustment with
that accepted source before the final Cloud PR521 qualification to avoid
qualifying an obsolete source. Full hosted completion remains required before
claiming stage B complete. New OSS source requires new exact-source B/C runtime
candidates; earlier AE image proofs remain historical, not portable authority.
Production acceptance and the unanswered holder choice remain independent.
