# Shared fixture prerequisite validation

Plan: thoughts/shared/plans/2026-11-11-shared-fixture-due-time-and-core-rollback.md.
Source base1dd051bf. Test-only: production Go/SQL/modules/workflows/manifest
and all existing assertion/deadline bytes unchanged.

Complete OSS Store:276root families/TZ,472named PASS/TZ in UTC/Europe-Prague,
0FAIL/SKIP. Each timezone owns its own PG16 cluster,768MiB/1CPU/128pids,
nonroot/read-only/tmpfs and durable creation intent/CID. Both final inventories
are0extra DBs/roles; both OOM0 and independently absent. Peak163827712B.
Accepted Cloud-owned sweeper independently verifies0remaining using this job's
exact private ledger. Every root runs/passes exactlyonce; launch/end hashes match.
Compile/default vet and git diff --check pass.

Evidence: `/private/tmp/oss-cloud-production-20261110/oss-shared-fixture-store-isolated-992e750456874105861b052bf34b49d1/proof.json`.

Initial harness attempt completed both suites472PASS/TZ but called an absent
Cloud script from OSS and recorded no anchor capacity. It remains a harness
FAIL and is not the qualified resource result above. The corrected driver uses
distinct clusters and direct kernel peak/OOM counters before exact-owned removal.

The scoped helper reads MAX(next_attempt_at) for exact seeded org/message rows;
all scheduling/backoff/exhaustion/stale-lease/blob assertions remain. Core29
refusal cases select only Core and preserve all4seeds/DownTo28/guard strings.
The earlier Cloud claim[] failure does not prove measured clock skew/timezone bug.

Remaining: normal upstream exact-head review/CI/main and normal generated
Cloud source producer/sync acceptance. No production deployment or credentials.
