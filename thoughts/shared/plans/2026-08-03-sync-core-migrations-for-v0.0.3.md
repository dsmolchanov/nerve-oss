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
  baseline. Correct `0016` OSS-first so its down migration refuses with a
  clear error when pre-provider audit events contain a NULL
  `provider_message_id`; deleting those events or fabricating an id is not an
  acceptable rollback policy. Sync the corrected file back to `nerve-cloud`
  before publishing the runtime so the core trees return to byte identity.
- Normalize the non-executable `0017` subscription comment to `Webhook
  endpoints` in both repositories so the OSS migration-ownership gate does
  not misclassify it as a billing table reference.
- Do not change runtime logic, MCP behavior, providers, cloud-only migrations,
  or dependencies.
- Publish the merged commit as immutable runtime tag `v0.0.3` only after CI
  and Codex review are green.

## Compatibility Evidence

- Core migrations `0001` through `0010` are already byte-identical between
  `nerve-oss` and `nerve-cloud`.
- The synchronized core-schema hash, including the guarded `0016` down path,
  is `76ab78d80d85fff9f12002223c6614bf6d15cdbcc72236a72ededfe3118e73a8`.
- The MCP contract hash remains
  `1eb62111fc593ec9bc9a8ab7d5a9f52a1f3b4e661ee0dffafe4c60495f5b678b`.
- Migrations `0011` through `0017` are additive on the production upgrade
  path. Production currently reports core migration version `15`, so the
  new runtime may apply `0016` and `0017` before serving.

## Verification

- [x] Migrations `0011`–`0015` compare byte-for-byte with `nerve-cloud`.
- [x] Migration `0016` down refuses before changing schema when NULL provider
  message ids exist, remains at version 16, and succeeds to version 15 after
  the blocking rows are explicitly resolved.
- [ ] The corrected `0016` and normalized `0017` comment are synced to
  `nerve-cloud`, restoring full core-tree byte identity before runtime
  publication.
- [x] A canonical `v0.0.3` manifest reports the expected MCP and core hashes.
- [x] Fresh PostgreSQL migration succeeds through core version `17`.
- [x] Upgrade from core version `15` succeeds through version `17`.
- [x] `go test ./... -count=1` passes against PostgreSQL.
- [x] `go vet ./...` passes.
- [x] `git diff --check` passes.
- [ ] GitHub CI and Codex review pass.
- [ ] Docker Publish creates the `v0.0.3` image and release artifacts.

No live email is sent during verification.
