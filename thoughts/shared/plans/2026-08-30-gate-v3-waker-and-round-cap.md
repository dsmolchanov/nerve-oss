---
name: gate-v3-waker-and-round-cap
status: approved
repository: dsmolchanov/nerve-oss
created: 2026-08-30T19:10:00Z
updated: 2026-08-30T19:10:00Z
---

# Gate v3: event-driven verdict wait and the review-round cap

## Current state

This repository's merge gate is the reusable `codex-review-window` from
dsmolchanov/codex-review-gate (see `2026-08-20-reusable-codex-gate.md`),
pinned by SHA in a one-screen stub. The v2 gate held a hosted runner for up to
15 minutes per run waiting for a Codex verdict — measured fleet-wide at 424
billed minutes in one day, 263 of them in runs cancelled mid-sleep — and a
finding that survived the three-round review budget was never recorded
anywhere once the PR merged.

## Desired state

Adopt gate v3 (codex-review-gate pin `6569100561df84a415a4640a380148f78b1c1b90`,
reviewed and merged there as PRs #3/#4/#6/#7):

- The gate takes a 120-second verdict window only where an ACTIVE
  `codex-verdict-waker` workflow exists in this repository — the waker re-runs
  the gate's PR-bound run when a Codex clean-summary comment lands, since a
  comment event alone can never satisfy the required check. Until the waker is
  merged the gate detects its absence and keeps the long 900s window, so this
  very PR is not stranded by its own change.
- Past the three-round budget the gate merges over open P1s and files them as
  ONE review-debt issue per PR (idempotent by title); P0 blocks on every round
  and alerts Slack when stuck past the budget.
- The stub grants exactly what the reusable workflows need and nothing more:
  `actions: read` for the waker probe, `issues: write` for the debt record.

## Scope

This plan authorizes the `.github/` workflow changes on branch
`chore/plaintalk-dev-agent-policy-v2`, applied by
`bootstrap-dsmolchanov-repo.sh --gate-only` from dsmolchanov/dev-agent:

- `.github/workflows/codex-review-window.yml` — pin bump to the v3 revision,
  `actions: read` + `issues: write` grants.
- `.github/workflows/codex-verdict-waker.yml` — NEW consumer stub:
  `issue_comment` trigger (the event fires only in the consumer repository),
  guarded job-level concurrency so unrelated comments cannot cancel a live
  wake, pinned to the same revision in lockstep — the two stubs are two halves
  of one protocol.

Out of scope: every other file; branch protection; the reusable workflow
bodies themselves (reviewed in codex-review-gate, where their executable tests
live).

## Success criteria

- [ ] Both stubs pin `6569100561df84a415a4640a380148f78b1c1b90`.
- [ ] After merge, `gh api repos/dsmolchanov/nerve-oss/actions/workflows/codex-verdict-waker.yml`
      reports `state: active`.
- [ ] A PR whose Codex verdict arrives as a comment after the gate's short
      window goes green without a manual re-run.
