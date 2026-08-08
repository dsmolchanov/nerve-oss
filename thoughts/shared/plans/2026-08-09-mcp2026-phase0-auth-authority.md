---
title: MCP 2026 Phase 0 Shared Authentication Authority
status: approved
created_at: 2026-08-09
repository: nerve-oss
branch: codex/mcp2026-phase0
base_commit: a794be9f2697e0864d3a31da8f087577e9748f7e
---

# MCP 2026 Phase 0 Shared Authentication Authority

## Context

This branch is the OSS-first authority tranche required by Phase 0.6 of the
approved `nerve-cloud` plan
`thoughts/shared/plans/2026-08-06-mcp-2026-autonomous-agent-onboarding.md`.
Cloud must not author these shared bytes first or advance its source-authority
lock until this branch is merged and green.

## Authorized scope

- Extend `internal/auth/context.go` with explicit legacy, Cloud-key,
  `m2m_onboarding`, and `m2m_org` principal kinds plus immutable machine-client
  and generation identity.
- Extend `internal/auth/verifier.go` with fail-closed PS256 M2M access-token
  verification that remains isolated from the existing HS256 path. Require
  issuer, audience, expiration, issued-at, not-before, JTI, client, generation,
  scope, and token-kind invariants.
- Add `internal/auth/jwks.go` for bounded RSA access-token JWKS parsing and RFC
  7638 thumbprints. Client assertion registry keys remain outside this set.
- Add focused tests for principal typing, algorithm/key/claim confusion,
  temporal-claim requirements, JWKS safety, and stable thumbprints.
- Add `internal/auth/` to `sync-manifest.yaml` as an exact OSS-owned mirror.

No OAuth issuer, client registry, lifecycle mutation, MCP routing, deployment,
or production activation is authorized by this tranche.

## Success criteria

- [x] `go test ./internal/auth -count=1` passes.
- [x] `go test ./... -count=1` passes.
- [x] `./scripts/ci/test_sync_manifest.sh` passes.
- [x] `git diff --check` passes.
- [ ] Required GitHub CI and `codex-review-window` pass for the final commit.
- [ ] The branch is merged before Cloud copies the bytes or advances
      `deploy/cloud/oss-source.lock`.
