---
repository: nerve-oss
status: in_progress
date: 2026-09-09
source_revision: 95683e821c23354023331653f671a8408c0c202b
parent_repository: nerve-cloud
parent_plan: thoughts/shared/plans/2026-09-08-mcp2-pricing-schema-release.md
---

# Runtime counterpart for the pricing schema transition

This separate OSS deliverable implements the runtime prerequisites of the approved Cloud phase 0. It does not activate pricing or change production. Work is isolated from the user's existing OSS checkout.

## Startup quiescence

Explicitly introduce the operational setting `NERVE_SCHEMA_TRANSITION_MODE`: unset/empty preserves ordinary startup; `quiescent` in Cloud mode denies all network requests with 503 and Retry-After, without constructing the application, opening stores or starting any workers. Unknown values and non-Cloud use fail closed. Worker mode waits for termination without initializing dependencies. MCP stdio and embedded migration commands refuse quiescent mode. A dedicated migrator remains the only migration owner.

Files: new `internal/startup/quiescence.go` and tests; runtime and administrative entrypoints `cmd/neuralmail/main.go` and `cmd/neuralmaild/main.go` and executable tests in `scripts/ci/test_schema_quiescence_executables.py`; the invocation in `.github/workflows/ci.yml` is explicitly in scope. No new dependency, HTTP API, database table or trust principal. This is a startup mode, not a dynamically cached feature flag. The deployment must stop every old Machine, disable autostart/schedules, then start all bridge/target Machines in this mode. It must verify the full inventory and old-token/webhook rejection before starting the dump. Already running old processes are not retroactively fenced by setting an environment variable on a different process.

Tests must exercise the real compiled entrypoints with invalid database/provider endpoints: all HTTP methods/routes/credentials fail at the closed socket boundary; workers stay alive without opening dependencies; invalid mode fails; signal shutdown completes; ordinary startup remains unchanged. No cloud-only changes to exact-mirror files.

## Remaining counterpart deliverables

- [x] Startup quiescence implementation and executable tests. Runtime serve/worker, administrative CLI refusal, invalid mode, ordinary-start negative control and SIGTERM tested with real binaries. CI invocation and actionlint pass.
- [ ] Immutable compiled bridge/target schema windows and native compatibility output bound to actual runtime manifest/executable bytes.
- [ ] Versioned runtime build pipeline, signed image/provenance, source pin adoption in Cloud.
- [ ] Actual Core migrations for pricing A in its own scoped implementation; do not invent future schema heads here.
- [ ] Disposable combined Cloud/OSS rehearsal, shutdown of all old writers, dump/restore and reopening only after archived disabled CAS.

Quiescence alone does not prove shutdown of the old fleet or settle in-flight provider operations. Preserve their unknown intents. No native runtime claim, pricing quota implementation or production gate is marked complete by this plan.

## Review correction — 2026-09-09

Remove the maintenance HTTP handler entirely. Both serve and worker wait only for cancellation, without auth/store/provider initialization or network listeners. External ingress must provide retryable maintenance failure; verify that separately before a dump. No local 503 claim is made. `AGENTS.repo-invariants.md` is explicitly included for the recurring cross-repository auth finding, with deterministic dependency/no-listener tests and a real executable method/path/credential matrix. Ordinary startup, invalid-mode/admin refusal and SIGTERM tests remain required.

Fix validation: targeted `go test -race` and `go vet` for startup/neuralmail/neuralmaild PASS; real executable no-listener matrix, invalid dependency/ordinary-start control, admin refusal and SIGTERM PASS. Remote review/CI remains separate.

## Native report increment — 2026-09-09

After acceptance of PR #93, branch `codex/mcp2-runtime-compatibility` starts at merged `9a220a3`. Scope: `internal/release/runtime_compatibility.go` and tests, `cmd/neuralmaild/main.go`, native executable tests in `internal/release`, and this plan. Add `compatibility --json` before config loading: output the exact existing nine-field runtime manifest from linker metadata and compiled startup constants, plus SHA256 of the running executable. Reject missing/malformed build identity and flags. Report `database_verified=false` and `admission_verified=false`; do not claim signed membership, image provenance or a database probe. No new network endpoint or dependency.

This increment implements the native report prerequisite with the existing frozen Core29 window. Configurable bridge/target windows, versioned image publishing, source-pin adoption and combined DB rehearsal remain unchecked. No future migration number is allocated. Tests must build/run the real command with invalid config/DB environment, compare all manifest fields with the existing generator and hash the binary independently; malformed flags and dev metadata fail without application startup.

Native report validation: `go test -race ./internal/release ./internal/startup ./cmd/neuralmaild -count=1`, targeted `go vet`, and `scripts/ci/test_schema_quiescence_executables.py` PASS. The executable test builds release/dev binaries, compares all nine generator fields, hashes the running artifact independently, and verifies environment overrides cannot supply release identity. The current Core29 window remains frozen; this is not a bridge/target or DB admission claim.

## Native report review and CI follow-up — 2026-09-09

Reject the explicit `unknown` runtime identity as well as `dev`, with negative metadata coverage. The authorized CI fix also includes `internal/mcp/ai_unavailable_test.go`: size only this wire-test fixture budget for its bounded initialize/initialized/tool requests, whose server cleanup may overlap after the SDK receives SSE results. Keep AI error assertions and production memory limits unchanged; existing SDK budget rejection/release tests remain required.

Follow-up validation: `go test -race ./internal/release ./internal/mcp ./cmd/neuralmaild -count=1`, `go test -race ./internal/mcp -run '^TestUnavailableAIModernHTTP$' -count=50 -timeout=90s`, and `go vet ./internal/release ./internal/mcp ./cmd/neuralmaild` PASS. The MCP package run includes shared-budget rejection and release regression coverage.

## Compiled schema window increment — 2026-09-09

Scope: `internal/startup/migrations.go`, new `internal/startup/schema_window.go` and tests; `internal/release/runtime_compatibility.go` and executable tests; `scripts/release/runtime_schema_window.sh` and `generate_runtime_manifest.sh`; `deploy/docker/cortex/Dockerfile`; `cmd/nerve-migrate/main.go`, `cmd/neuralmail/main.go` and their regression tests. No workflow or source-pin activation in this increment.

An explicit `RUNTIME_MANIFEST_VERSION=2` build selects a versioned immutable Core window. The common build helper derives max from the actual SQL catalog; role B requires a prior minimum, role C uses the current catalog head as minimum. Signed successor validation remains responsible for binding the B minimum to the prior source. Historical/default builds preserve Core29 and local Cloud3. Runtime report and database startup read the same compiled window; successor services require verify, embedded administrative migration commands refuse, and the dedicated migrator permits only Core up/status within the compiled bounds, never down. Wire manifest remains the existing nine-field runtime contract; the enclosing signed successor set carries version/role. No future SQL head is allocated or real DB compatibility claimed.

The Alpine builder explicitly installs bash to run the shared build helper. Successor Docker builds require label inputs to equal the computed window; all three bundled commands receive that same linker value.

Compiled-window validation: race tests and vet for startup, release, neuralmail, nerve-migrate and neuralmaild PASS. Tests cover malformed/versioned intervals, environment non-override, helper/manifest agreement against a synthetic catalog, migration-owner restrictions before DB access, and the compiled default migrator target. The successor Docker build stage passes for actual Core29 with role B; its compiled migrator refuses Core down with networking disabled. Database bridge/target matrix, signed image publication and real pricing heads remain unverified and are not completed by these tests.
