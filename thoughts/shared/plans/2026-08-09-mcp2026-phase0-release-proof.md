# MCP 2026 Phase 0 Release Proof

## Scope

Land the OSS-authoritative release inputs required by Phase 0 of the approved
Cloud MCP 2026 autonomous-agent onboarding plan:

- the immutable V1 autonomous outbound policy artifact;
- its exact-mirror declaration and runtime-manifest/OCI provenance;
- immutable pins for the official MCP conformance runner and ext-auth
  specification;
- client-credentials-only authorization-server metadata fixtures that reject
  compatibility lies.

This change does not implement the MCP 2026 handler, OAuth issuer, Cloud 0009,
candidate publication, or runtime deployment.

## Implementation

1. Add `configs/policy/autonomous-outbound-v1.yaml` with the approved V1
   limits, evidence, suspension, billing, and domain-readiness rules.
2. Add the policy artifact to `sync-manifest.yaml` as an exact mirror.
3. Extend the runtime manifest, exported workflow outputs, Docker build
   metadata, runtime startup banner, and release workflow with the policy
   version and SHA-256.
4. Extend manifest regression tests to require and validate both fields.
5. Add a CI script that checks out the official conformance runner and
   ext-auth repositories at exact commits, verifies their lockfile digests,
   builds/tests the runner, checks the 2026-07-28 capability, and validates the
   RFC 8414 client-credentials-only metadata fixtures.
6. Run the proof in OSS CI.

The metadata fixture deliberately follows the product decision already
approved in the Cloud canonical plan: RFC 8414's published text requires a
nonempty `response_types_supported`, but a client-credentials-only server has
no truthful authorization response type. Reported Errata 7793 proposes
omission for this case. Nerve therefore omits the member, rejects an empty
array and the fabricated `client_credentials` value, and treats
acceptance by the pinned executable SDK consumer as the compatibility gate.
This OSS script pins the fixture invariants; it does not claim that the
omission passes the uncorrected RFC 8414 document schema.

## Verification

- `go test ./internal/release -count=1`
- `go test ./...`
- `./scripts/ci/test_mcp_conformance.sh`
- `./scripts/sync/verify_exact_mirror.sh sync-manifest.yaml . .`
- `./scripts/ci/test_sync_manifest.sh`
- build the runtime image and run `scripts/ci/verify_runtime_policy_artifact.sh`
  to verify the policy OCI labels and embedded bytes.
