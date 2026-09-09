# Gate v3.5: review-debt identity lives in a tested script

Status: approved

## Goal

Move both stubs to gate revision `4d05f6909650381037fee2cfed40c6a5cab591af` (codex-review-gate#15). It carries
one change over the revision this repository pins today.

**What identifies a deferred finding is decided in one tested file.** On a
round past the review budget the gate merges over still-open P1s and files one
issue per finding; the key that makes two findings the same finding was a sed
pipeline inside the workflow's heredoc, and four of the five review rounds on
the per-finding change were spent re-deriving it. The rule now lives in
`scripts/review_debt.py` in the gate repository — fingerprint of the path and
the complete finding line, display title bounded under GitHub's 256-character
limit, in-batch and cross-round dedup — with the policy stated once (no line
number, no comment id, no PR number, and why) and pinned by unit tests over
the cases those rounds found. The workflow fetches that file at ITS OWN commit,
the one this stub pins, so identity and workflow cannot drift apart. Titles
are byte-identical to the previous rule, so issues already filed still dedupe.
The fetch is fail-soft like every read in that block: a failure warns and
merges the finding unrecorded rather than guessing a key; it never holds a
merge the verdict already allowed.

## Files

- `.github/workflows/codex-review-window.yml` — gate stub: re-synced byte for
  byte from the dev-agent fleet template at the new pin. The only change
  outside comments is the pin; the comments now describe the revision the
  stub pins (they were three revisions stale, flagged on NeoMenu#720).
- `.github/workflows/codex-verdict-waker.yml` — waker stub: re-synced the same
  way, in lockstep. The gate's short window and the waker's re-entry are two
  halves of one protocol and must name the same revision.
- `thoughts/shared/plans/2026-09-02-gate-v35-review-debt-identity-script.md`
  — this plan (an applicable plan must list itself).

## Verification

Both stubs pin the same 40-hex revision; the waker remains an active workflow
after merge; the gate check goes green on this pull request through the pinned
revision.
