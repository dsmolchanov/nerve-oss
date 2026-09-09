# Gate v3.3: the waker re-enters on a formal review

Status: approved

## Goal

A Codex verdict delivered as a FORMAL REVIEW left a stale failed gate run on
the commit: the gate's own re-entry starts a NEW check run while the earlier
run that failed for want of a verdict stays, and GitHub's status rollup counts
the red one — the pull request reports BLOCKED with a green run of the same
context beside it. Cleared by hand four times on 2026-08-31.

Gate revision `ae5d20884828bee1bdec72fcca998d1d6ed1c2c6` (codex-review-gate#9)
adds the `pull_request_review` trigger to the waker, accepts either event shape
in its guard and concurrency group, and drains EVERY stale gate run for the
head — serialized, because the gate's concurrency group is
`cancel-in-progress` and back-to-back re-runs would cancel each other.

## Files

- `.github/workflows/codex-verdict-waker.yml` — waker stub: `pull_request_review`
  trigger, guard and concurrency group accepting either event, pin bump.
- `.github/workflows/codex-review-window.yml` — gate stub: pin bump, in lockstep.
- `thoughts/shared/plans/2026-09-01-gate-v33-waker-review-reentry.md` — this plan (an applicable plan must list itself).

## Verification

Both stubs pin the same 40-hex revision; the waker registers as an active
workflow after merge; a pull request whose Codex verdict arrives as a formal
review goes green without a manual re-run.
