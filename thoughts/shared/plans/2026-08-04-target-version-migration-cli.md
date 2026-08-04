# Phase 0 §5 Target-Version Migration CLI Plan

## Overview

Provide bounded, scope-aware migration primitives in nerve-oss before wiring a
dedicated migration command and bounded startup policy. A staged schema rollout
must be able to stop at an exact version, deploy compatible readers, and only
then apply a later relaxation migration. Applying every pending migration at
process startup collapses those stages and can make a safe rollout impossible.

This is the OSS-local plan for Phase 0 §5. Shared store migration code is owned
here and mirrored into nerve-cloud. The dedicated `nerve-migrate` command must
also land in OSS so the runtime image can execute core migrations; Cloud keeps
its own command/startup and deployment wiring because `cmd/**` is not an
exact-mirror path.

## Current State

- `internal/store/migrate.go` exposes unbounded core, cloud, and combined
  migration functions. This branch adds target-version `MigrateUpToCore` and
  `MigrateUpToCloud`, one-step `MigrateDownCore` and `MigrateDownCloud`, and
  `CurrentVersionCore` and `CurrentVersionCloud`.
- Goose's legacy API stores the dialect and migration table name in package
  globals. Core and cloud calls can run concurrently, so configuration and the
  complete goose operation must be serialized by one package-level mutex.
- `cmd/neuralmail` and `cmd/neuralmaild` currently expose only unbounded
  `migrate-core`, `migrate-cloud`, and `migrate-all` commands.
- Runtime startup is also unbounded. `internal/app.New` migrates all scopes for
  `serve` and `mcp-stdio`, while the `worker` path calls `store.Migrate`
  directly. There is no version compatibility window or verify-only mode.

## Desired End State

1. Store callers can migrate core or cloud to an exact target, roll back one
   step, and query each scope's current version without cross-scope goose state
   leaking between concurrent calls.
2. A dedicated command is the canonical operational interface:

   ```text
   nerve-migrate up     [--scope core|cloud|all] [--to <version>]
   nerve-migrate down   --scope core --steps 1
   nerve-migrate status [--scope core|cloud|all]
   ```

3. Production startup verifies a compiled compatibility window and does not
   silently apply every pending migration. OSS-local development can opt into
   bounded application through an explicit startup mode.
4. Existing migration aliases remain compatible until callers and deployment
   scripts have moved to `nerve-migrate`.

## Scope of This Branch: Store Primitives

### Changes Required

- Add `MigrateUpToCore` and `MigrateUpToCloud` over `goose.UpToContext`.
  After applying, read the scope's database version under the same Goose lock
  and reject the operation unless it exactly matches the requested target.
- Add one-step `MigrateDownCore` and `MigrateDownCloud` over
  `goose.DownContext`.
- Add `CurrentVersionCore` and `CurrentVersionCloud`.
- Serialize each complete goose configuration-plus-operation sequence with a
  package-level mutex shared by every new and existing store entry point.
- Keep `Migrate`, `MigrateAll`, `MigrateCore`, and `MigrateCloud` behavior and
  signatures unchanged.

### Automated Verification

- A PostgreSQL-backed test proves `MigrateUpToCore` stops at its target, a later
  migration remains pending, migration to head completes, unavailable and
  already-passed targets fail, and one down call rolls back to the exact
  previous migration version.
- A concurrency regression test proves a cloud operation cannot replace the
  goose table configuration while a core operation is still running.
- Run focused store tests, the race-enabled concurrency test, `gofmt`, and
  `git diff --check`.

## Follow-up: Dedicated CLI

Add `cmd/nerve-migrate` and make it the canonical interface for migration jobs.
The command will:

- accept `up` with `--scope core|cloud|all` and optional `--to`; for `all`,
  apply core first and then cloud, honoring the target independently per scope;
- accept a deliberately narrow one-step core rollback command;
- report current and pending versions per selected scope through `status`;
- validate commands, scopes, step counts, and non-negative target versions at
  the CLI boundary before opening or mutating the database;
- return a non-zero exit status on invalid input, incompatible state, or any
  migration failure.

Migrate the existing `cmd/neuralmail` and `cmd/neuralmaild` aliases and cloud
deployment scripts in follow-up changes after the dedicated command is tested.
Do not remove an alias until its in-repository callers have moved.

## Follow-up: Bounded Startup

Replace unconditional startup migration with `NM_MIGRATE_ON_START`:

- `verify`: read the applied version and require it to be within the binary's
  compiled `[minRequired, maxSupported]` window; never apply migrations. This
  is the cloud default.
- `apply-to-max`: apply only through `maxSupported`, then verify the resulting
  version. This is available for OSS-local and development startup.
- `off`: neither apply nor verify, for operators that manage schema state
  externally.

Wire the policy consistently through `internal/app.New` and the worker startup
path so `serve`, `mcp-stdio`, and `worker` cannot disagree. Keep operational
migration execution in `nerve-migrate`; startup is a compatibility gate, not a
replacement for the deployment migration job.

## Not in This Branch

- No new command binary or command-line flag parsing.
- No startup environment mode or compiled compatibility constants.
- No changes to cloud deployment scripts, workflow files, or image contents.
- No schema migrations or data changes.
- No multi-step or automatic rollback policy.

## Rollback

Revert the store primitive change. Existing unbounded migration entry points
retain their prior signatures, so callers do not need a compatibility shim.
The plan-only follow-ups have no runtime effect until implemented separately.
