# Hybrid fixture database source validation

Plan: ../plans/2026-11-11-hybrid-fixture-database-source.md
Base: nerve-oss1c2a6a0970c5ffb6bdd0b6ebea0a0004822a2f2a.

The optional source and connection registry exist exclusively in
internal/store/migration_test.go. Nil keeps OSS's existing PostgreSQL selection;
Cloud can assign its provider only in its own package-local _test.go initializer.
Each helper allocates once, registers the actual per-handle URL, and removes it
at the existing t.Cleanup boundary. Unknown handles/empty supplied sources refuse.
All four restricted-role reconnect families use that handle's registered source
before their unchanged database/user/password replacements and tenant assertions.

Actual PG16 checks:13PASS events,0FAIL,0SKIP, exit0. Includes nested allocation,
reconnection, callback-return lifetime and post-subtest closed handles/registry
removal, plus unchanged tenant RLS, feature flags, attachment legacy deny-all,
domain-grant grantee, global canonical conflict and grant-revocation serialization.
The new regression wraps and retains any original Cloud provider; it does not
replace an eventual isolation provider with the ordinary global source.

Own UUID databases/selected test role inventory0|0, exact owned container removed.
Raw fixture-source-db-tests.jsonl and fixture-source-db-proof.json are retained in
/private/tmp/oss-cloud-production-20261110. Store tests compile; Store vet,
go build ./..., git diff --check and exact-mirror manifest membership pass.
Only four existing _test.go files change; no production/public API, source flag,
SQL, dependency, sync manifest, .github or ownership exemption. Normal OSS review/
required CI and generated Cloud synchronization remain pending.

This is a prerequisite contract, not Cloud cluster implementation, numbered
catalog promotion or production acceptance. Cloud still must bind all its own
reconnects, qualify isolated clusters/capacity/nested cleanup in both full timezone
passes, then perform coordinated signed migration/release and real acceptance.
All eighteen fixed role preexistence refusals remain unchanged. Production
acceptance=false/provider_complete=false; no customer data/key/resource action.
