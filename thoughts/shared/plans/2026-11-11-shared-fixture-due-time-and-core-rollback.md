---
repository: nerve-oss
status: in-progress
source_commit: 1dd051bfdd34a98e919a2be2785cd8d87a6847ac
---

# Shared fixture due time and Core rollback Implementation Plan

## Overview

Correct two shared test fixture assumptions found while qualifying the real
Cloud40 consumer. Production Go/SQL, deadlines and all business assertions stay
unchanged. These files are OSS-authoritative exact mirrors; ship upstream and
consume the normal generated Cloud sync, never edit the downstream lock by hand.

## Current State Analysis

`internal/store/message_attachment_worker_test.go:34` initially claims a newly
persisted row using process time, while `message_attachments.go:108` omits due
time and Core0023 gives it the database now() default. Claim eligibility at
message_attachments.go:270 compares that due time with the supplied time. There
is no ordering guarantee across the process/database samples. One actual Cloud
UTC full-suite claim returned no row; the retained log does not prove a measured
clock delta or timezone defect. Five queue positives have the same assumption.

`internal/store/outbox_policy_fence_test.go:109` installs both streams latest
before rolling Core toward28. Cloud40 legitimately depends on Core30 recipient
reservations, so it prevents reaching the intended Core29 evidence guard. The
four subcases actually failed2BP01 at Core30 in both timezones. The adjacent
empty-Core29 case already selects Core only; audit found no other mixed-stream
Core downgrade consumer requiring adaptation.

## Desired End State

Initial positive attachment claims use the exact maximum due timestamp of their
own seeded org/message rows. Scheduling tests retain explicitly arranged process
times. Core29 refusal tests install only the Core stream they own and reach the
original guards, with all seeds/refusal strings unchanged.

### Key Discoveries

- Exact mirror authority covers all three files in sync-manifest.yaml.
- The schedule/lease test already sets next_attempt_at from its captured clock;
  its schedule and early/future refusals must remain unchanged.
- Existing owned PG16 fixture lifetime/cleanup and required DB gate are retained.

## What We're NOT Doing

- No production behavior/schema, source manifest, workflow, invariant or dependency
  changes; no sleep, future-time allowance, skip or timeout increase.
- No direct downstream mirror or source-lock edits.
- Cloud-only sibling fixtures are recorded for their later Cloud plan, not copied
  into this repository.

## Implementation Approach

Use one test-only scoped due-time helper in the existing shared queue test file.
Keep the Core29 test explicitly Core-only, matching its adjacent qualification.
Publish one bounded upstream PR; normal main producer/sync owns Cloud provenance.

## Phase 1: Make initial positive claims deterministic

### Overview

Arrange the exact eligibility boundary from persisted fixture rows.

### Changes Required

**Files**: internal/store/message_attachment_queue_test.go,
internal/store/message_attachment_worker_test.go.

- Add private attachmentFixtureDueAt(t,ctx,db,orgID,messageID), querying only
  MAX(next_attempt_at) for those two exact identifiers and refusing empty/error.
- Use it for the five positive queue roots and initial worker flow claim.
- Keep exact claim counts, early/retry/exhaustion/stale-lease/blob assertions and
  the original dedicated schedule test.

### Success Criteria

#### Automated Verification

- [x] Complete shared Store suite in UTC/Europe-Prague with required owned PG16,
  count1, finite120m package alarm, no FAIL/SKIP, capacity/OOM/cleanup/absence proof.
- [x] go test -mod=readonly ./internal/store -run '^$' -count=1 compiles and vets.

#### Manual Verification

Not applicable: these are deterministic source fixtures, not a UI/provider action.

## Phase 2: Reach the original Core29 guard

### Overview

Remove the unrelated Cloud stream from this Core-owned historical fixture.

### Changes Required

**File**: internal/store/outbox_policy_fence_test.go.

- Replace only the mixed latest setup in
  TestCoreMigration29DownRefusesPolicyAndProviderEvidence with MigrateCore.
- Preserve all four evidence seeds, DownTo28 and exact guard strings.
- Keep all other historical tests and every migration byte unchanged.

### Success Criteria

#### Automated Verification

- [x] Complete Store qualification above includes the four original refusals and
  original empty rollback success; production/source/module diff is empty.
- [x] git diff --check succeeds.

#### Manual Verification

Not applicable: observed PostgreSQL refusals establish this fixture behavior.

## Phase 3: Preserve normal source provenance

### Overview

Consume the upstream result using the existing guarded producer.

### Changes Required

**Files**: no additional source edits. Normal Sync OSS → Cloud workflow generates
exact mirrors and its source lock after accepted OSS main.

### Success Criteria

#### Automated Verification

- [ ] Normal upstream PR CI/review accepts exact head.
- [ ] Main sync producer completes its full Cloud build/test/owned cleanup.
- [ ] Generated Cloud PR strict source-lock/exact mirrors and normal CI/review pass.

#### Manual Verification

Not applicable: no deployment, credential or production change is included.

## Testing Strategy

### Unit Tests

Retain all existing claim schedule, retry budget, stale lease, completion and
blob assertions. Do not replace them with tests that merely echo helper code.

### Integration Tests

Real owned physical PG16 in both timezones; full Store suites, immutable launch
hashes, durable ownership intents, kernel capacity/OOM, anchor inventory and
independent cleanup verification.

### Manual Testing

Not applicable: test-only change with executable database evidence.

## Performance Considerations

One small tenant/message-scoped aggregate per affected fixture. Production query
performance and all timeout/resource limits are unchanged.

## Migration and Rollback

No migration or production deployment. Ordinary revert restores old test bytes.
Normal source producer must carry matching mirrors and lock together.

## References

- User's active full OSS→Cloud production goal; discovered prerequisite to C.
- Cloud full Store evidence:
  /private/tmp/oss-cloud-production-20261110/compiled-c-58cd-store-full-8fea4a15c83045bfa6e46e18a88fced6/.
- Message attachment schema Core0023 and message_attachments.go:270.
- outbox_policy_fence_test.go:105–124; Core0029 original guard55–66.

Kernel/docs hygiene gate: not_applicable; no installed skill kernel/adapter files
are changed. Repository metadata captured at2026-11-11 14:36WET on source1dd051bf.
