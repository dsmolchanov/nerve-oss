---
date: 2026-08-04T18:46:52+02:00
git_commit: c785117b487aa6f8be96d256b4d9ed0b6cc7b487
branch: codex/sync-manifest-baseline
repository: nerve-oss
source_plan: nerve-cloud/thoughts/shared/plans/2026-08-02-inbound-events-and-attachments.md
source_section: Phase 0 section 4
status: implementing
---

# Shared baseline and ownership-aware OSS to Cloud sync

## Goal

Finish Phase 0 section 4 of the inbound events and attachments plan before
feature migrations begin. OSS is authoritative for shared runtime code, but
the sync must distinguish byte-identical paths, patch-synced paths, and
Cloud-only paths.

## Implementation

1. Preserve the already-reviewed D9 public API while moving Cloud-only store
   methods out of `store.go` into the four Cloud boundary files. Converge the
   remaining shared store layout without changing exported signatures.
2. Backport the shared webhook/store baseline and its tests, including the
   dispatcher, listing, outbox listener, SMTP inbox configuration, and
   delivery/DLQ/suppression coverage.
3. Backport the shared email transport baseline. Keep `internal/jmap` in the
   exact mirror so the JMAP provider dependency cannot be omitted again.
4. Add `sync-manifest.yaml` with the exact-mirror, patch-synced, and
   cloud-only ownership lists specified by the source plan.
5. Replace hardcoded workflow path filters with manifest-driven changed-file
   classification. Copy exact and bootstrap paths wholesale, apply divergent
   shared paths using fatal `git apply --3way`, and never apply Cloud-only
   paths.
6. Byte-compare every exact path after application, then run
   `go build ./...` and `go test ./...` in the staged Cloud checkout before
   opening its sync PR.

## Verification

- `go build ./...`
- `go vet ./...`
- `TZ=UTC go test ./... -count=1`
- `TZ=Europe/Prague go test ./... -count=1`
- `scripts/ci/test_sync_manifest.sh` covers exact replacement and deletion,
  bootstrap copy, Cloud-only preservation, and fatal 3-way conflicts.
- `scripts/sync/verify_exact_mirror.sh` validates the manifest and compares
  staged OSS/Cloud exact paths.
- `actionlint` validates the changed workflows; `shellcheck` and `bash -n`
  validate the sync scripts.

## Cloud follow-through

The automated Cloud PR must contain the exact baseline, pass its full Go
suite, add a Cloud-side exact-mirror CI assertion against OSS main, and map
the repaired active-webhook uniqueness violation (`23505`) to HTTP 409 with a
DB-backed repeated-POST regression test. Those Cloud-only boundary changes
remain outside this OSS PR and are required before Phase 1 starts.
