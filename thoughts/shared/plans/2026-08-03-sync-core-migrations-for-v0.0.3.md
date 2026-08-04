# Sync Core Migrations for Runtime v0.0.3

## Goal

Publish a runtime image whose embedded core schema matches the current
`nerve-cloud` compatibility lock. The existing `v0.0.2` image contains core
migrations only through `0010`, while cloud contains additive migrations
`0011` through `0017`. The repository split runbook assigns core migrations
to `nerve-oss`, so those files must be synchronized before production
promotion.

This release also retains the already-reviewed `compose_email.from_name`
support from `v0.0.2`.

## Scope

- Copy core migrations `0011` through `0017` from the current `nerve-cloud`
  baseline. Correct `0014` OSS-first so its down migration removes the outbox
  notification trigger and function instead of leaving live objects behind
  an unapplied schema version. Correct `0016` OSS-first so its down migration
  refuses with a clear error when pre-provider audit events contain a NULL
  `provider_message_id`; deleting those events or fabricating an id is not an
  acceptable rollback policy. Because production is already at core `0015`,
  `0016` also repairs the missing `ENABLE`/`FORCE ROW LEVEL SECURITY` policies
  on `outbox_events` and `inbox_smtp_configs` while protecting the new
  `suppressions` table. Sync the corrected files back to `nerve-cloud` before
  publishing the runtime so the core trees return to byte identity.
- Correct `0017` OSS-first so active webhook endpoint identity is unique on
  `(org_id, url)` while disabled historical rows remain allowed. Without that
  partial uniqueness, fan-out sends the same event more than once to a URL.
- Normalize the non-executable `0017` subscription comment to `Webhook
  endpoints` in both repositories so the OSS migration-ownership gate does
  not misclassify it as a billing table reference.
- Do not change runtime logic, MCP behavior, providers, cloud-only migrations,
  or dependencies.
- Keep the migration test databases disposable: close each test connection,
  terminate/drop its database while the admin handle is still open, then close
  the admin handle.
- Publish the merged commit as immutable runtime tag `v0.0.3` only after CI
  and Codex review are green.

## Compatibility Evidence

- Core migrations `0001` through `0010` are already byte-identical between
  `nerve-oss` and `nerve-cloud`.
- The synchronized core-schema hash, including the corrected `0014` and
  guarded `0016` down paths, is
  `fd453d13e1b8083e7ede7a3a42711383010dd146fdd14dfeb49c6ea8b876ab73`.
- The MCP contract hash remains
  `1eb62111fc593ec9bc9a8ab7d5a9f52a1f3b4e661ee0dffafe4c60495f5b678b`.
- Migrations `0011` through `0017` are additive on the production upgrade
  path. Production currently reports core migration version `15`, so the
  new runtime may apply `0016` and `0017` before serving.

## Verification

- [x] Migrations `0011`–`0013` and `0015` compare byte-for-byte with
  `nerve-cloud`; corrected `0014` remains OSS-first until the sync PR lands.
- [x] Migration `0014` down removes both `trg_outbox_notify` and
  `notify_outbox_insert()` and records core version `13`.
- [x] Migration `0016` down refuses before changing schema when NULL provider
  message ids exist, remains at version 16, and succeeds to version 15 after
  the blocking rows are explicitly resolved.
- [x] Upgrade from version `15` enables and forces tenant RLS with the standard
  policy on `outbox_events`, `inbox_smtp_configs`, and `suppressions`; rolling
  `0016` back restores the prior RLS state on the surviving tables.
- [x] Migration `0017` rejects a duplicate active `(org_id, url)` and permits a
  disabled historical duplicate.
- [ ] The corrected `0014`, corrected `0016`, and normalized `0017` comment are
  synced to `nerve-cloud`, restoring full core-tree byte identity before
  runtime publication.
- [x] A canonical `v0.0.3` manifest reports the expected MCP and core hashes.
- [x] Fresh PostgreSQL migration succeeds through core version `17`.
- [x] Upgrade from core version `15` succeeds through version `17`.
- [x] `go test ./... -count=1` passes against PostgreSQL.
- [x] `go vet ./...` passes.
- [x] `git diff --check` passes.
- [x] Migration tests leave zero `nerve_test_*` databases behind.
- [ ] GitHub CI and Codex review pass.
- [ ] Docker Publish creates the `v0.0.3` image and release artifacts.

No live email is sent during verification.
