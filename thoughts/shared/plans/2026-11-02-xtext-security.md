---
date: 2026-11-02
repository: nerve-oss
status: approved
authorization: Necessary security-gate remediation within the owner's authorized MCP fixes and protected deployment; kept separate from the MCP wire change.
---

# Repair existing reachable x/text vulnerability

OSS MCP PR159 security CI reports GO-2026-6629 through Store.OpenDB,
pgx and precis.Profile.String. The Go vulnerability database identifies
v0.41.0 as the first fixed golang.org/x/text release:
https://pkg.go.dev/vuln/GO-2026-6629

Scope: change only the existing indirect x/text requirement from v0.39.0 to
v0.41.0 and its module sums, plus the unavoidable x/sync v0.22.0 minimum declared
by x/text v0.41.0. No new dependency, workflow, toolchain, schema,
authentication or pricing change. The x/text module graph also moves x/tools checksums from v0.47.0 to
v0.48.0; it remains outside the application require block. No unrelated
selected dependency versions change. Tidy removes obsolete checksums.
This independent PR must pass govulncheck, full required CI and clean
current-head native review. Then merge current main into the MCP PR and
rerun its required gates. Cloud delivery mirrors both reviewed source changes
through the existing source lock; no ordinary deployment or capacity increase.

Validate exact module diff, module resolution, affected Store and MCP/auth
packages and full remote CI. Do not waive or weaken the security gate.
