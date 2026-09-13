---
date: 2026-09-13
repository: nerve-oss
status: approved
branch: chore/gate-pin-v37
---

# Pin the review gate at d49bbf8: a debt record carries a label

## What changes

Both gate stubs move their `uses:` pin from
`dbd0d8429c58ec2a444597ae1a08419fa962c163` to
`d49bbf81eca6986f40cf1b05cb7951b7f0034924` (codex-review-gate#18), the revision
`plaintalk-dev-agent` pinned in its own repository in dev-agent#104. Nothing
else changes: the trigger, the guard, the permissions and the concurrency group
are untouched, and the gate's verdict behaviour is identical.

## Why

A review-debt record — the issue the gate files when a round past the review
budget merges over a still-open P1 — carried **no label at all**. The only way
to select one was the `[review-debt] ` prefix in its title, which is a
convention rather than a query. Classifying the fleet's 184 open issues on
2026-09-11 therefore meant pulling every issue through a script and matching
titles, and a cross-repository triage board cannot be built that way.
This repository holds 3 of those records.

The new revision creates the label and applies it in exactly one request per
record. One, not a labelled-then-unlabelled fallback: `-f` makes the call a
POST, so a fallback would file the same finding twice whenever a create
committed on GitHub while the client reported a lost response — two records for
one finding is precisely what the gate's identity policy exists to prevent.

## What this does not carry

The first cut of codex-review-gate#18 also skipped the debt write for
`dependabot[bot]` pull requests. It was withdrawn: at filing time the gate does
not know whether a pull request will merge, and bot pull requests do merge on
green — `livekit-voice-agent` #482 through #486 merged this week and left real
debt behind. Abandoned bot branches are handled where the outcome is actually
known, by the retraction that dev-agent#93 added on an unmerged close.

## Files

| File | Purpose |
| --- | --- |
| `thoughts/shared/plans/2026-09-13-gate-pin-v37-debt-label.md` | This plan |
| `.github/workflows/codex-review-window.yml` | Move the gate stub's pin |
| `.github/workflows/codex-verdict-waker.yml` | Move the waker stub's pin, in lockstep |

## Verification

- The two stubs pin the same revision, and that revision is the one
  `plaintalk-dev-agent` carries in its design lock.
- The gate runs and reaches a verdict on this pull request.
