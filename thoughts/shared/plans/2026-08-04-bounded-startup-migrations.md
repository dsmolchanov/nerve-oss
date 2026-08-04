# Bounded Startup Migrations

## Metadata

- Date: 2026-08-04 18:10:59 CEST
- Baseline commit: `26b2461d0cbf1dbf5da1b3215376eb1a37217fb9`
- Branch: `codex/bounded-startup-migrations`
- Repository: `dsmolchanov/nerve-oss` (worktree `nerve-oss-bounded-startup`)
- Source plan: `nerve-cloud/thoughts/shared/plans/2026-08-02-inbound-events-and-attachments.md`, Phase 0 §5 and §7

## Objective

Complete the bounded-startup follow-up that was deliberately excluded from the
earlier target-version CLI PR. Long-running runtime processes must never apply a
migration newer than the schema window compiled into their binary, and cloud
startup must be read-only by default.

## Scope

1. Add `verify`, `apply-to-max`, and `off` startup policies to the shared store
   migration API.
2. Make `verify` read-only and reject schema versions below `minRequired` or
   above `maxSupported`.
3. Make `apply-to-max` call the existing target-version primitives rather than
   unbounded Goose `Up`.
4. Replace runtime `store.Migrate` calls in both serve/MCP initialization and
   the worker path.
5. Compile the runtime core window as `[18,18]`. In cloud mode the OSS runtime
   verifies core only because Cloud owns its separate schema history. Local OSS
   mode may additionally apply the bundled cloud tree through `0003`.
6. Publish the same core window in `runtime-manifest.json`, validate it before
   exposing workflow outputs, and test that manifest values match the compiled
   constants. Upload the files to a non-replaceable GitHub Release as well as
   the short-lived Actions artifact; Cloud authority must not expire with
   Actions artifact retention.
7. Land OSS-first. The Cloud follow-up mirrors the generic store API, declares
   control-plane windows core `[18,18]` and cloud `[7,7]`, and adds the explicit
   production migration predecessor before enabling verify-only startup.

## Non-goals

- No feature migrations `0019`-`0024`.
- No production deployment or feature activation from this repository.
- No Cloud-owned schema files or control-plane entrypoint changes.
- No change to the explicit `nerve-migrate` CLI contract.

## Verification

- PostgreSQL tests prove `verify` applies nothing, accepts an in-window schema,
  and rejects an older binary whose maximum is below the current version.
- PostgreSQL tests prove `apply-to-max` stops at its declared targets and `off`
  creates no migration tables.
- Unit tests cover default/explicit/invalid modes and core-only runtime scope.
- Manifest tests reject missing, malformed, inverted, or code-divergent schema
  windows.
- `go test ./... -count=1` under UTC and Europe/Prague, `go vet ./...`,
  `go build ./...`, and release-script shellcheck must pass.

## Rollout

Merge this PR, publish the next immutable runtime version from the merge commit,
then pin its digest and published manifest atomically in Cloud. Cloud must not
deploy a verify-only binary until its ordered workflow applies and asserts core
`0018` and cloud `0007` first.
