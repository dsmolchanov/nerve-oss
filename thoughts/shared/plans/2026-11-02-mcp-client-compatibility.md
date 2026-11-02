---
date: 2026-11-02
repository: nerve-oss
status: approved
authorization: Owner reported strict Claude Code initialization and discovery failures during the authorized MCP production remediation.
---

# Strict client initialization and bounded discovery

Fix the real connection path on the deployed source814cdf0, rather than only
the first rejected field. Preserve authentication, Origin checks, tenant/scope
authorization, session checks, recipient quotas and the 16 MiB wire bound.
No new dependencies, flags, paid capacity, schema migration or ordinary deploy.
The old broad legacy/R0 validation and authority workflow remain excluded;
this narrowly authorized 2025-11-25 wire correction is required by the owner’s
current strict-client report.

## Findings and scope

- `internal/mcp/server.go`: initialize advertises boolean capabilities;
  `notifications/initialized` is not acknowledged and ping is unimplemented.
  The same initialized notification is consumed without a response by the
  sibling `internal/mcp/stdio.go` transport; preserve its existing API.
- `internal/mcp/types.go`: six tool descriptors omit mandatory inputSchema and
  the resource descriptor omits mandatory name. Describe the existing argument
  contracts without changing execution, scopes or tool membership.
- The same successful-call path also returned raw business objects without
  required MCP content. `internal/mcp/legacy_results.go` adds standard tool
  content/resource contents while explicitly retaining the original top-level
  payload fields for the published0.2 SDK. It copies rather than mutates stored
  business objects, rejects reserved-field collisions and leaves business errors,
  invocation, audit, idempotency and tenant access unchanged. Qualify actual
  authenticated cached success, strict SDK decoding and resource URI binding.
- `internal/mcp/router.go`: main already negotiates headerless initialize;
  preserve and qualify that boundary, including hostile Origin/auth failures
  and invalid/repeated/conflicting explicit version headers. Change only if
  reproduction identifies an additional defect.
- `internal/mcp/sdk_server.go` and the shared bounded-reader helper currently
  reserve42 MiB for every modern operation under the default64 MiB budget.
  Reproduce overlapping discovery responses. If confirmed, account two wire
  copies as bounded bytes arrive, then reserve the existing10 MiB decoded
  attachment allowance before dispatching tools/call. No uncharged full-body
  materialization, no removal/increase of the global budget and no wire-limit
  bypass for unknown/understated Content-Length.
  The bounded reader uses one growing `internal/memguard/reservation.go`
  reservation rather than retaining one release closure per tiny read, so
  adversarial chunking cannot turn bounded wire bytes into unbounded accounting
  metadata. Existing Budget Acquire/Limit/Used/Available APIs remain unchanged.
  New reservation tests cover aggregate sharing, concurrent growth/close and
  terminal non-revival; bytewise-reader allocation qualification catches the
  whole metadata amplification class.
- `internal/mcp/client_compatibility_test.go`, existing router/body/SDK tests,
  legacy wire goldens and both protocol-mode fixtures: strengthen the complete
  initialize→initialized→tools/resources/ping path and concurrent discovery.
  Update only the deliberately corrected invalid discovery goldens; preserve
  other business errors, tool calls, sessions and modern profile tests.
- `AGENTS.repo-invariants.md` records the checked discovery shape and budget
  rules; `docs/MCP_Contract.md` records the corrected interoperability contract.

## Validation and delivery

First record failing strict-shape/notification/concurrent-discovery regressions.
Then run the full MCP suite/race and affected memory/config/app checks, source
mirror/contract checks, required remote CI and clean exact-head native review.
Mirror the reviewed upstream source through Cloud's existing exact-source lock
and manifest. Build fresh signed Core32 runtime B/C candidates and use only the
existing protected release rehearsal/apply/readback process, preserving Cloud37,
all data, original Abrolia cohort and self-mail, and current spending limits.
Do not move ordinary runtime.lock or enable public pricing/QA as a side effect.

Production diagnosis is read-only: numeric memory configuration, actual Nerve
RSS/cgroup counters, image/source identity and protected probe. The current
runtime RSS was about20 MiB, cgroup usage about54 MiB and failcnt0; this does
not prove absence of historical overload. No VM enlargement or restart is
justified solely by the application budget's503 response.
