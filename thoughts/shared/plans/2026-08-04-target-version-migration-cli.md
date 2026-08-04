# Phase 0 §5 Target-Version Migration CLI Plan

## Overview

Complete the OSS side of Phase 0 §5 with bounded, scope-aware migration status
and a dedicated operational command. A staged schema rollout must be able to
inspect real pending versions, stop at an exact version, deploy compatible
readers, and only then apply a later relaxation migration. Applying every
pending migration at process startup collapses those stages and can make a safe
rollout impossible.

This is the OSS-local plan for Phase 0 §5. Shared store migration code is owned
here and mirrored into nerve-cloud. The dedicated `nerve-migrate` command must
also land in OSS so the runtime image can execute core migrations; Cloud keeps
its own command/startup and deployment wiring because `cmd/**` is not an
exact-mirror path.

## Current State

- `internal/store/migrate.go` exposes unbounded core, cloud, and combined
  migration functions plus target-version `MigrateUpToCore` and
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

## Scope of This Branch: Status API and Dedicated CLI

### Existing Store Primitives

- Add `MigrateUpToCore` and `MigrateUpToCloud` over `goose.UpToContext`.
  Validate that the target exists (or is already current) before applying
  anything, then read the scope's database version under the same Goose lock
  and reject the operation unless it exactly matches the requested target.
- Add one-step `MigrateDownCore` and `MigrateDownCloud` over
  `goose.DownContext`.
- Add `CurrentVersionCore` and `CurrentVersionCloud`.
  Version inspection is read-only: a fresh database reports version zero
  without creating either migration bookkeeping table.
- Serialize each complete goose configuration-plus-operation sequence with a
  package-level mutex shared by every new and existing store entry point.
- Keep `Migrate`, `MigrateAll`, `MigrateCore`, and `MigrateCloud` behavior and
  signatures unchanged.

These primitives are already merged and remain the foundation for the command.

### Changes Required

- Add `MigrationStatusCore` and `MigrationStatusCloud`, returning current
  version, available head, and the sorted pending versions collected from the
  actual scoped Goose migration files. Sparse versions must stay sparse; for
  example cloud at version `0001` reports `[0003]`, never a synthetic `0002`.
- Reject a database whose current version is ahead of the available head or is
  a non-zero version absent from the scoped migration files. Status inspection
  must stay read-only on a fresh database and run under the shared Goose lock.
- Add `cmd/nerve-migrate` as the canonical operational interface:

  ```text
  nerve-migrate up     [--scope core|cloud|all] [--to <version>]
  nerve-migrate down   --scope core --steps 1
  nerve-migrate status [--scope core|cloud|all]
  ```

- Parse and validate the complete command before loading configuration or
  opening the database. `up` and `status` default to `all`; `down` requires the
  explicit core scope and exactly one step. Versions are non-empty ASCII base-10
  digits, so `0018` is accepted while signs, whitespace, overflow, and a
  command-local DSN flag are rejected.
- For `up --scope all --to N`, read and validate both scoped statuses before
  mutating either scope. A target is valid only when it equals the current
  version or is one of that scope's real pending versions; zero is therefore a
  no-op only for a fresh scope. Apply core before cloud after the complete
  preflight succeeds.
- Print stable status lines after `status`, successful `up`, and successful
  `down`, with four-digit versions and no synthetic gaps:

  ```text
  core current=0005 head=0010 pending=5 pending_versions=[0006,0007,0008,0009,0010]
  cloud current=0001 head=0003 pending=1 pending_versions=[0003]
  ```

- Build `/app/nerve-migrate` into the OSS runtime image without changing its
  existing entrypoint, and smoke-test the packaged binary with `--help` in CI.
- Keep the existing `cmd/neuralmail` and `cmd/neuralmaild` migration aliases
  unchanged until their callers move to the dedicated command.

### Automated Verification

- A PostgreSQL-backed test proves `MigrateUpToCore` stops at its target, a later
  migration remains pending, migration to head completes, unavailable and
  already-passed targets fail without partial application, and one down call
  rolls back to the exact previous migration version.
- A fresh-database test proves version inspection returns zero without
  initializing migration bookkeeping.
- A concurrency regression test proves a cloud operation cannot replace the
  goose table configuration while a core operation is still running.
- PostgreSQL-backed status tests cover a fresh database without bookkeeping
  tables, a partial core scope, sparse cloud versions, an unknown current
  version, and a current version ahead of head.
- Parser and no-I/O tests cover defaults, all valid scopes, help, invalid input,
  version edge cases, and prove configuration/database access happens only
  after parsing succeeds.
- Command orchestration tests prove both statuses are validated before an
  `all --to` mutation, core runs before cloud, a cloud target gap prevents core
  mutation, rollback is exactly one core step, output is buffered until every
  selected status succeeds, and resources close on success and failure.
- Run focused store and CLI tests, the race-enabled store/JMAP suites, the full
  Go suite, `go vet`, image build and CLI smoke, `gofmt`, and `git diff --check`.

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

- No startup environment mode or compiled compatibility constants.
- No changes to cloud deployment scripts or cloud startup wiring.
- No schema migrations or data changes.
- No multi-step or automatic rollback policy.

## Rollback

Revert the status API, dedicated command, and image/CI wiring. Existing bounded
and unbounded migration entry points retain their prior signatures, the runtime
image entrypoint is unchanged, and existing aliases remain available, so
callers do not need a compatibility shim.
