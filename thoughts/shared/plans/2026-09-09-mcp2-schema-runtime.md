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

Tests must exercise the real compiled entrypoints with invalid database/provider endpoints: all HTTP methods/routes return retryable failure; workers stay alive without opening dependencies; invalid mode fails; signal shutdown completes; ordinary startup remains unchanged. No cloud-only changes to exact-mirror files.

## Remaining counterpart deliverables

- [x] Startup quiescence implementation and executable tests. Runtime serve/worker, administrative CLI refusal, invalid mode, ordinary-start negative control and SIGTERM tested with real binaries. CI invocation and actionlint pass.
- [ ] Immutable compiled bridge/target schema windows and native compatibility output bound to actual runtime manifest/executable bytes.
- [ ] Versioned runtime build pipeline, signed image/provenance, source pin adoption in Cloud.
- [ ] Actual Core migrations for pricing A in its own scoped implementation; do not invent future schema heads here.
- [ ] Disposable combined Cloud/OSS rehearsal, shutdown of all old writers, dump/restore and reopening only after archived disabled CAS.

Quiescence alone does not prove shutdown of the old fleet or settle in-flight provider operations. Preserve their unknown intents. No native runtime claim, pricing quota implementation or production gate is marked complete by this plan.
