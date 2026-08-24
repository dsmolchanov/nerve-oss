---
title: MCP 2026 and Autonomous Agent Onboarding
status: draft
revision: 21
created_at: 2026-08-06 12:49:07 CEST
enhanced_at: 2026-08-23
repository: nerve-cloud
branch: main
commit: 4c01c66713f35dd35693612f3c44106bcde16cba
runtime_baseline: v0.0.17
runtime_source_revision: a794be9f2697e0864d3a31da8f087577e9748f7e
core_schema_baseline: 0028
core_schema_target: 0029
cloud_schema_baseline: 0009
cloud_schema_target: 0010
---

# MCP 2026 and Autonomous Agent Onboarding Implementation Plan

## Overview

Upgrade the hosted Nerve MCP endpoint from its custom 2025-11-25 implementation to a dual-protocol server that adds the stable MCP 2026-07-28 protocol without breaking the already-published Python SDK 0.2.0.

On the modern protocol, a trusted external machine agent must be able to complete the normal tenant lifecycle without per-tenant human intervention:

1. Authenticate as a pre-registered machine client through OAuth Client Credentials and private_key_jwt.
2. Create exactly one active organization generation.
3. Either provision a fresh mailbox on a Nerve-owned verified domain or claim and verify its own domain.
4. Obtain a new organization-bound access token without receiving or storing a Nerve API key.
5. Receive, read, search, draft, and reply to email immediately.
6. Use compose_email only after a custom domain is fully ready or a real paid subscription is confirmed.
7. Close the generation, cancel any generation-owned Stripe subscription, and reconnect later with a new generation, new external references, and a never-recycled managed address.

The one-time registration of the machine identity, the one-time configuration of Nerve's platform domain, and—only when a managed-mailbox client needs paid compose—the one-time attachment of an SCA-complete, off-session-capable billing mandate remain operator responsibilities. After those prerequisites, normal onboarding is autonomous. A custom-domain agent must separately possess a DNS-provider MCP connector or API credential; Nerve never receives registrar credentials. Nerve never asks the agent to automate a hosted Checkout page or handle cardholder/SCA challenges.

## Approved Product and Architecture Decisions

| Decision | Approved choice | Consequence |
|---|---|---|
| Machine identity | Pre-registered trusted M2M client using OAuth Client Credentials and private_key_jwt | No public dynamic client registration; Nerve stores public JWKs only |
| Tenant cardinality | One active organization generation per machine client | A partner needing concurrent tenants receives distinct client identities |
| Ready mailbox | Fresh server-generated alias on a Nerve-owned verified domain | Reuse the existing owner-to-grantee domain grant model; no mailbox pool or transfer |
| Alias lifecycle | Never recycle an allocated address | A permanent allocation tombstone survives inbox and org cleanup |
| Initial outbound | Inbound, reading, drafting, and replies are available immediately | A reply must target the latest real inbound sender in that tenant-owned thread |
| New-message outbound | compose_email unlocks after verified custom domain or confirmed paid subscription | Tool scope is a ceiling; durable send policy is re-evaluated on every enqueue and inbound traffic never changes trust in V1 |
| Protocol rollout | Hybrid router on one /mcp endpoint | SDK 0.2.0 remains sessionful; MCP 2026 is stateless and uses the official Go SDK |
| Schema ownership | Historical Cloud 0009, forward-only Cloud 0010, and OSS-authority additive Core 0029 | Runtime never reads Cloud onboarding tables; Core 0029 owns only the shared outbox policy fence, while Cloud 0010 owns hosted canonical inbox identity and managed-namespace enforcement |
| OAuth origin | Dedicated https://auth.nerve.email control-plane origin | Avoid dashboard login middleware and path-rewrite coupling |
| DNS automation | Agent uses its own DNS connector | Nerve returns records and verifies them; it never accepts DNS credentials |
| Async readiness | Explicit status and verify tools plus the existing reconciler | Do not require optional MCP Tasks support in the first release |
| Legacy retirement | Not part of this plan | Collect version telemetry and retire sessions only in a later, separately approved release |
| Lifecycle authorization | Scope-selected, renewable onboarding-maintenance token | An active org token never gains lifecycle tools; the same client can reacquire `nerve:onboarding` and manage only its own generation |
| Autonomous compose trust | Verified custom domain or confirmed paid subscription only | Inbound mail cannot earn trust in V1; authenticated reputation is a separate future project |
| Autonomous billing entry point | Org-bound `nerve_billing_subscribe` using a preauthorized client billing mandate | No caller-selected org/generation, hosted Checkout automation, card data, or mid-flow human action; `requires_action` fails closed |
| Close and billing | Immediate asynchronous cancellation of the generation-owned Stripe subscription | No entitlement transfer and no automatic prorated refund; `closed` waits for terminal Stripe evidence |
| Provider workflow | Version-fenced lease workflow with no terminal `failed` state | Every permanent failure passes through `deprovisioning` and proven cleanup before `closed` |
| Outbox shutdown | Provider-start is the send linearization point under a per-org policy epoch | Suspension/close cancels queued work and drains/readbacks already-started provider operations before `closed` |
| Inbox retention | Keep generation-owned inbox rows disabled after close | Do not cascade-delete evidence or payload rows; a specialized autonomous tombstone predicate proves ownership and terminality |

## Current State Analysis

### Production and repository state

- Cloud main is commit 99092688c3213af3cf7dc8e72cc28bd89983f6a1.
- deploy/cloud/runtime.lock pins runtime v0.0.17 from nerve-oss revision a794be9f2697e0864d3a31da8f087577e9748f7e.
- Production is Core 0028 and Cloud 0009 behind proven Artifact A, with M2M issuance disabled. Immutable runtime v0.0.17 is compiled for Core `[28,28]` and cannot start on Core 29. Phase 9 must therefore deploy and prove a distinct legacy bridge R0 with Core `[28,29]`, install feature-complete B on Cloud 9, apply forward-only Cloud 0010, and only then apply Core 0029; the final runtime/C pair contracts to Core 29/Cloud 10.
- Runtime is deployed before control plane after explicit migrations in .github/workflows/deploy.yml. This order means a new runtime may consume only control-plane capabilities that were already deployed in an earlier release.
- The runtime has one active and one stopped Fly Machine. The process-local MCP session map is therefore not safe for more than one active Machine without affinity or shared session state.
- Exact-mirror CI currently derives source authority from deploy/cloud/runtime.lock, which also pins the deployed artifact. That coupling prevents an OSS-first shared-auth tranche from becoming authoritative before a new runtime image exists; source authority and deployed-artifact authority need separate locks.

### MCP transport and contract

- Production internal/mcp/server.go is a hand-built 2025-11-25 server with process-local 24-hour MCP-Session-Id state.
- It does not negotiate protocol versions.
- Six of the eight tools have no input schema, tools have no output schemas, and business maps are returned directly rather than as MCP CallToolResult content and structuredContent.
- The existing resource list/read responses are not fully conformant.
- The Python SDK 0.2.0 requires a session header and interprets the JSON-RPC result as the business object in sdk/python/src/nerve_email/client.py:126-229.
- The Python transport always calls `response.json()`, while MCP Streamable HTTP permits either JSON or SSE and Go SDK 1.7.0 defaults `JSONResponse` to false.
- A single official SDK handler cannot preserve that behavior. The official Go SDK 1.7.0 stateless handler must be a second route behind a protocol-aware router.
- The official Go SDK 1.7.0 requires Go 1.25. Production nerve-oss still declares Go 1.23 in go.mod, OSS CI, security CI, and deploy/docker/cortex/Dockerfile.
- The current Origin check runs before normal bearer/cloud-key authentication and rejects absent-Origin native clients unless they carry the global API key. The hybrid endpoint needs one explicit Origin/auth wrapper shared by both adapters.

### Authentication and tenant provisioning

- Cloud requests currently accept a global X-API-Key bootstrap key, an org-bound X-Nerve-Cloud-Key, or an HS256 service JWT.
- There is no OAuth authorization server, protected-resource metadata, authorization-server metadata, client registry, private_key_jwt validation, or client-credentials token endpoint.
- internal/auth/verifier.go:70-118 accepts only HS256 and rejects tokens without org_id. It cannot represent an unbound onboarding principal.
- An active binding cannot always imply an org token: lifecycle maintenance must remain obtainable after the initial five-minute onboarding token expires.
- Current service-token issuance and revocation do not take the same locks, so `/oauth/token` can race `close` or operator client revocation and leave a newly inserted active token.
- The planned global OAuth issuance switch has no current production enable path. Synthetic exchange must be preceded by an evidence-bound enable transition, and rollback must atomically return it to off before drain.
- internal/cloudapi/handler_orgs.go creates organizations only for the bootstrap administrator and provisions the trial entitlement after the org write, so a partial failure can leave an org without billing state.
- internal/store/email_tenancy.go:32-62 intentionally returns an idempotency conflict when an external_ref points to a tombstoned org. Reusing one stable client-to-org reference cannot support reconnect.
- Cloud API keys reveal their raw secret once. A lost create response cannot safely recover it, so the autonomous flow must not mint a long-lived raw API key.

### Domains and inboxes

- Custom-domain creation already returns DNS records and provider state, but Nerve cannot edit a customer's DNS.
- The existing domain readiness path can mark a domain active after SPF and DKIM while a receiving-enable error is only logged. Autonomous activation must require ownership, SPF/DKIM, inbound MX, provider verification, and provider receiving to be operational.
- core/0024 introduced org_domain_grants and enforces that an active inbox uses either an org-owned domain or a domain actively granted to that org.
- Core 0024 makes only `lower(address)` unique while an inbox remains active. Supported Store paths must additionally serialize and compare the complete canonical identity without rewriting valid legacy bytes, while Cloud 0010 replaces the hosted active-address index with the same functional identity. A separate permanent alias registry is still required to ensure that an address is never reused after cleanup.
- Ordinary inbox create/reactivate paths and inbound catch-all do not consult such a registry, so a retired managed address requires a database-enforced tombstone, not only an application check.
- Current domain locking is org-scoped, does not treat every foreign unexpired pending claim as conflicting, and legacy pending-domain GC deletes rows directly. Legacy and autonomous ownership therefore need one canonical-domain claim and lock protocol.
- A successful Resend domain create followed by a local persistence failure can leave a provider-only domain because internal/cloudapi/handler_domains.go:356-410 removes the local row but does not compensate at the provider. Canonical-name lookup cannot distinguish such an orphan from this workflow's own timed-out create.
- Registering the chosen platform domain does not retroactively protect pre-existing active or disabled inboxes; all existing addresses must be permanently reserved before the allocator is enabled.
- NM_CLOUD_MANAGED_DOMAIN implements an auto-activated tenant subdomain path. It is not the selected platform-mailbox design and must not be repurposed.

### Outbound authorization and abuse controls

- internal/mcp/server.go:628-647 currently maps both send_reply and compose_email to nerve:email.send.
- internal/tools/service.go:306-323 chooses the last message's From address, even if the last message is outbound. A safe reply must choose the latest inbound message.
- internal/entitlements/service.go:104-109 defaults email sending to enabled. A generic cached `false` feature value therefore fails open for autonomous sends and cannot be used as the security decision.
- The current MCP rate limiter is process-local. It is not a durable anti-abuse limit across multiple Machines.
- Resend delivery webhooks already create recipient suppressions for permanent bounces and complaints in internal/cloudapi/resend_webhook.go:613-640. They provide the signals needed to pause an organization's outbound access, but inbound `From` is not authenticated reputation evidence and cannot earn compose trust.
- Existing Core `org_feature_flags` and live org-domain ownership express the effective enqueue decision, but they cannot durably distinguish a merely claimed row from a provider-started operation after the transaction lock is released. Core 0029 therefore adds the shared monotonic epoch and provider-start/resolution fence; Cloud 0009 continues to preserve source evidence and policy history.
- The generic usage reconciler overwrites every counter from `usage_events`; any new durable send meter must emit matching deterministic events or it will be reset.
- `usage_events.replay_id` is globally unique while tool idempotency is tenant/tool scoped, and the current reconciler performs separate SUM and SET operations. An un-namespaced replay ID can collide across tenants, and a concurrent reconcile can overwrite a committed reservation.
- ClaimOutboxMessages changes rows to `sending` without a live autonomous-policy check, and internal/emailtransport/outbox_worker.go:185-303 reaches the provider without rechecking suspension/close. Deleting an inbox is not a cancellation primitive: the worker can retain the payload and send after the cascading row deletion.
- Billing currently exposes checkout, portal, and webhook snapshots but no Stripe cancellation operation. Generation close must add provider cancellation as a fenced cleanup barrier.
- Stripe webhook mutations are performed in internal/billing/stripe.go, not the delegating HTTP handler. A late `invoice.paid` currently maps the subscription back to active after cancellation, and subscription, entitlement, and webhook status are separate commits.

### Verification and release gaps

- .github/workflows/cloud-e2e-matrix.yml calls make cloud-e2e-test, but the Makefile target never enables the e2e build tag and does not run internal/cloudapi/e2e_matrix_test.go.
- The Cloud copy of that tagged test imports internal/mcp, which is OSS-only and absent from Cloud. The named matrix is not a valid gate.
- The control-plane image has no source/schema compatibility provenance labels and deploy accepts a caller-supplied digest not tied to a successful main CI artifact.
- control-plane-deploy.yml does not prove the digest of every active and stopped Machine.
- scripts/deploy/run_migration_machine.sh requires head equal to target and zero pending migrations. It cannot deliberately deploy a Cloud [8,9] predecessor while leaving 0009 pending.
- `.github/workflows/deploy.yml` always migrates before runtime/control-plane deployment, so it cannot perform the required predecessor-before-migration transition.
- `cmd/nerve-reconcile` begins mutating work without the startup compatibility check used by the web binary.
- runtime manifests omit outbound-policy provenance; the OSS tag workflow publishes immediately; and the SDK publish workflow can be dispatched without production combination evidence.
- Runtime deploy verifies a pre-existing Fly mirror but no current workflow creates the candidate mirror before release-set construction. The signed v0.0.17 baseline is valid historical/provenance evidence only: its compiled Core window rejects Core 29. A distinct attested legacy bridge R0 must be built, proven, and embedded as the authorized below-vNext rollback member before the additive migration.
- Cloud CI still rebuilds and publishes every main SHA as Artifact A. Once the Core tree advances to 0029, that creates an impossible manifest (`head=29`, A window `[28,28]`) and would mutate a historical artifact identity. Post-transition CI must instead build a local-only `validation` role that follows the checked-in schema head, is never published or accepted by deploy workflows, and refuses normal process startup.
- Cloud's repository token cannot create OSS tags/releases or retag OSS GHCR, while the current OSS `v*` workflow rebuilds the image. Publication therefore needs an OSS-side no-rebuild workflow and a least-privilege cross-repository handoff.
- scripts/deploy/cloud_deploy.sh and parts of the runbook still contain obsolete Core 19 / Cloud 7 defaults.
- The public dashboard origin rewrites only /v1 paths and its middleware protects other paths with Supabase. OAuth endpoints should therefore use the dedicated auth.nerve.email origin rather than depend on dashboard rewrites.

## Desired End State

### Autonomous managed-mailbox journey

1. An operator registers a machine client's public JWK and allowed modes/scopes. When paid managed-mailbox compose is allowed, the same registration binds a spending-capped, SCA-complete off-session billing mandate. The private key and payment instrument remain outside Nerve application storage.
2. The agent discovers protected-resource and authorization-server metadata.
3. The agent presents a one-minute private_key_jwt assertion, requests only `nerve:onboarding`, and receives a five-minute onboarding-maintenance token bound to the server-selected generation with no org_id.
4. server/discover and tools/list show only the four onboarding tools.
5. nerve_onboarding_start with mailbox_mode managed_mailbox atomically creates:
   - generation N;
   - an org with a generation-specific external_ref;
   - its trial entitlement, usage rows, and autonomous-policy flags;
   - an owner-to-grantee domain grant;
   - a permanent random alias allocation;
   - an active inbox on the platform domain.
6. Replaying the same idempotency key returns the same generation and address.
7. The agent exchanges another assertion for email scopes. Because the generation is active, the token endpoint returns an org-bound token. A separate `nerve:onboarding` exchange remains available for own-generation status, verify, and close.
8. The agent receives mail, reads it, drafts, and replies to its latest real inbound sender.
9. compose_email is absent and denied until the agent calls `nerve_billing_subscribe` with an org token carrying `nerve:billing.subscribe` and a generation-bound subscription is both paid and currently active. Nerve resolves all provenance from the principal and registered mandate; inbound traffic never changes this decision.
10. Closing the onboarding revokes every generation-bound organization/email token and outbound access while leaving short-lived onboarding tokens usable only to poll or repeat close for that same generation. It fences every generation-bound subscription-create/payment/subscription workflow, cancels queued outbound work, drains/readbacks provider-started sends, immediately requests cancellation when a subscription exists or later materializes, retires the alias, disables and retains the inbox, revokes the grant, cleans provider-backed resources, and tombstones the org. The workflow remains `deprovisioning` until outbox, provider, and Stripe state are proven terminal.
11. A new start key creates generation N+1 with new external references and a new address. The old address is never reused.

### Autonomous custom-domain journey

1. A generation-bound onboarding token starts custom_domain onboarding with a canonical domain and requested local part.
2. Nerve creates the org and pending domain and returns all required DNS records.
3. The agent changes DNS through its separate DNS connector.
4. nerve_onboarding_verify_domain and scheduled reconciliation poll idempotently.
5. The onboarding reaches active only when ownership, SPF/DKIM, inbound MX, provider verification, and provider receiving are all operational.
6. Nerve creates the inbox and records domain-scoped compose evidence.
7. A newly exchanged org token includes nerve:email.compose, and runtime still verifies that the selected sender inbox belongs to that same active org-owned domain.
8. The independently renewable onboarding-maintenance token can report status or close the generation without exposing organization email tools.

### Verification definition

The feature is complete only when the production contract workflow proves both journeys, the immutable 0.2.0 wheel still works against the dual runtime, SDK 0.3.0 passes modern and rollback compatibility, and a reconnect test produces generation 2 without reusing any old address or external reference.

## What We Are Not Doing

- No public Dynamic Client Registration.
- No authorization-code, browser-login, or human consent flow.
- No autonomous hosted-Checkout/card-entry/SCA automation. A client that needs managed-mailbox paid compose must arrive with an operator-approved off-session mandate; `requires_action` remains a fail-closed billing state.
- No self-service mutation of a client's registered trust root.
- No simultaneous multi-org tenancy under one client_id.
- No caller-selected alias on the platform domain.
- No mailbox pool, address transfer, or alias reuse.
- No storage or proxying of DNS-provider credentials.
- No direct DNS-provider integration in Nerve.
- No MCP Apps, elicitation, or optional Tasks dependency in the initial release.
- No removal of the 2025-11-25 route or published SDK 0.2.0.
- No change to legacy Cloud API key behavior for existing organizations.
- No blanket migration of existing organizations into the autonomous outbound policy.
- No earned-trust unlock from inbound volume, self-mail, sender diversity, SPF, DKIM, or DMARC in V1. A future reputation design requires a separate plan and approval.
- No transfer of a canceled generation's Stripe subscription or entitlement to a reconnect generation.
- No automatic prorated refund during autonomous close; existing financial obligations remain visible and operator-auditable.
- No promise of unlimited outbound volume; compose means arbitrary new-message recipients within plan and abuse limits.
- No destructive Core rewrite or new autonomous lifecycle table in Core. The approved additive Core 0029 is limited to the shared outbox policy epoch/provider-start fence and must preserve all legacy SQL/data behavior. Actual post-migration binary compatibility is provided by the separately versioned R0 bridge; immutable v0.0.17 remains Core `[28,28]` and is never relabeled or deployed after Core 29.

## Implementation Approach

### Component topology

    External machine agent
      |
      | OAuth discovery and private_key_jwt
      +------------------------------> auth.nerve.email
      |                                  control plane / OAuth AS
      |
      | Bearer token + MCP 2026
      v
    nerve runtime /mcp
      |
      | modern onboarding tool only
      | fixed private URL + signed delegation + original bearer
      v
    control-plane internal onboarding API
      |
      +--> PostgreSQL: Cloud 0009/0010 state plus existing Core resources
      +--> Resend domain API
      +--> Stripe subscription API through a preauthorized client billing mandate
      +--> scheduled nerve-reconcile

    Custom-domain branch only:
    external agent --> separate DNS MCP/API --> DNS provider

The runtime remains the public MCP resource server and OSS authority. It does not import or query any Cloud schema. A small `OnboardingProvisioner` interface in OSS delegates modern onboarding calls to a fixed control-plane endpoint. Each delegated request carries:

- the original bearer token;
- a timestamp and nonce;
- method, path, and body digest;
- a dedicated runtime-to-control-plane HMAC signature.

The control plane verifies the internal signature, rejects stale/replayed nonces, revalidates the bearer token and client state, and resolves all org/platform IDs server-side. The global bootstrap API key never crosses into runtime.

### Protocol routing, Origin, and authentication

Every `/mcp` request passes through one outer chain before either protocol adapter. Origin handling has a pre-auth and post-auth half because a present hostile browser Origin can be rejected immediately, while an absent Origin is safe only after principal classification:

    parse Origin; reject any present non-allowlisted value
      -> authenticate once and place Principal in context
      -> authorize present allowlisted or absent native Principal kind
      -> protocol/version router
      -> legacy adapter or modern Go SDK handler

Hosted Origin policy is exact and fail closed:

- A present hostile, malformed, `null`, prefix-lookalike, or suffix-lookalike Origin receives HTTP 403 before body dispatch, even with valid credentials.
- A present exact HTTPS allowlisted Origin proceeds to mandatory authentication.
- An absent Origin is valid only for an authenticated native principal of kind Cloud key, legacy HS256 bearer, `m2m_onboarding`, or `m2m_org`. This decision is independent of the credential's signing algorithm; issuer-owned PS256 access tokens are valid native credentials. Anonymous production traffic is never admitted.
- An empty allowlist rejects every present Origin. A development bypass is allowed only with explicit dev mode and loopback binding.
- Both adapters consume the same authenticated Principal from context and never re-run a divergent auth path.

| Request header | Handler | Lifecycle | Result shape | Tool surface |
|---|---|---|---|---|
| `MCP-Protocol-Version: 2025-11-25` | Frozen legacy custom handler | initialize plus `MCP-Session-Id` | Existing raw Nerve result | Existing eight email tools only |
| `MCP-Protocol-Version: 2026-07-28` | Official Go SDK 1.7.0 with `Stateless=true` | Request metadata on every call | `resultType`, content, and structuredContent | Principal/state-specific modern profiles |
| Missing version | Reject before handler | None | HTTP 400 with the SDK-defined header-mismatch protocol error | None |
| Unknown/future version | Reject before handler | None | HTTP 400 with the SDK-defined unsupported-version protocol error and both supported versions | None |

Do not use `MCPGODEBUG` session or Origin compatibility switches. They cannot reproduce SDK 0.2.0 and do not provide the shared hosted policy.

Refactor protocol-neutral execution out of internal/mcp/server.go:

    protocol adapter
      -> validate and decode
      -> Invoker
           -> scope and live send-policy authorization
           -> idempotency acquisition
           -> entitlement and durable rate reservation
           -> business tool
           -> audit and replay recording
           -> finalize or release reservations
      -> protocol-specific envelope

Legacy and modern adapters call the same Invoker so quota, idempotency, tenant scoping, and audit behavior occur exactly once. Internal typed errors are translated only at the adapter boundary:

- Legacy preserves `-32040` through `-32043` byte-for-byte for SDK 0.2.0.
- Modern HTTP authentication failures use 401/403 and a correct `WWW-Authenticate` challenge.
- Modern protocol failures use only standard JSON-RPC and SDK-reserved MCP codes.
- Modern business failures return a complete `CallToolResult` with `isError=true` and `structuredContent.error={code,retryable,retry_at?}`; modern responses never reuse the legacy `-32040...-32043` range.

### Principal and tool profiles

Token selection is scope-driven, not implicitly changed by an active binding:

1. `m2m_onboarding`: exactly the four lifecycle tools. It has `client_id`, a server-selected generation, `scope=nerve:onboarding`, no `org_id`, and can be reacquired while a generation is provisioning, DNS-pending, active, or deprovisioning. An already-issued token bound to a generation that reaches `closed` remains usable until expiry only for status and idempotent close of that generation; a later exchange selects the next generation.
2. `m2m_org`, autonomous compose locked, attachments off; `nerve_billing_subscribe` appears only when the token carries `nerve:billing.subscribe` and the registered client has a live billing mandate.
3. `m2m_org`, autonomous compose locked, attachments on; the same billing visibility rule applies.
4. `m2m_org`, compose enabled, attachments off; billing status remains readable but subscription creation is idempotently denied when an equivalent live subscription exists.
5. `m2m_org`, compose enabled, attachments on; the same billing rule applies.
6. Existing legacy-policy org profile, preserving current tools.

Under the client lock, onboarding-token issuance selects the current non-closed generation or `max(generation)+1` when none exists. Every lifecycle tool resolves exactly the generation claim in the principal; it never substitutes the client's live or latest generation. A token bound to a now-closed generation may read its final status or repeat close, but cannot verify it, create its successor, or observe N+1. A fresh assertion exchange selects N+1. `status`, `verify`, and `close` reject arbitrary generation, org, domain, and inbox identifiers. An org token never exposes lifecycle tools. During deprovisioning, email-scope exchange is denied while onboarding-scope exchange remains available for polling. Close revokes only generation-bound `m2m_org` email authority; onboarding tokens are poll/close-only and do not block `closed`. Operator `revoke-client` invalidates both token kinds through the locked live client-status check.

`tools/list` is private-cache scoped with a short TTL and is advisory only. `tools/call` repeats every authorization and policy check. A hidden tool call is denied even when a client cached an older list.

### Durable Cloud 0009 model and Cloud 0010 canonical boundary

Create `internal/store/migrations/cloud/0009_m2m_oauth_and_onboarding.sql` as the durable model transition. Forward-only Cloud 0010 later strengthens canonical inbox identity over these tables without rewriting legacy bytes, and OSS-authority Core 0029 separately owns only the outbox provider fence. No other schema migration is authorized by this plan.

#### OAuth and delegation tables

- `oauth_machine_clients`: Nerve-generated `client_id` primary key, display name, immutable `client_class=operator|synthetic|external`, active/revoked status, allowed onboarding/org scopes, allowed mailbox modes, activation state, activation cohort/approval reference/timestamp, timestamps, and revocation reason. Only the protected registration path can assign `operator` or `synthetic`; normal partner registration always writes `external`, and every classification/activation change is audited.
- `oauth_machine_client_keys`: primary key `(client_id,kid)`, immutable RFC 7638 SHA-256 thumbprint, public JWK, pinned PS256 or RS256 algorithm, active/retired/revoked status, validity window, and timestamps. `kid` must equal the unpadded base64url thumbprint. A permanent global unique constraint on the thumbprint prevents the same public key from being registered under another kid or client, and retired/revoked rows are never deleted or reusable. Symmetric keys and assertion-supplied `jwk`, `jku`, or `x5u` are rejected.
- `oauth_client_assertion_jtis`: primary key `(client_id,jti)` with `assertion_exp` and `retain_until`; `retain_until` is at least `assertion_exp + 30 seconds`, and cleanup may delete only at or after that instant. Insert is atomic with validation to fence replay across Machines throughout the full accepted skew window.
- `oauth_client_rate_buckets`: primary key `(client_id,purpose,bucket_start)` for token exchange and onboarding mutation limits.
- `oauth_machine_billing_profiles`: client ID primary key, Stripe customer/payment-method/mandate references, maximum permitted plan/spend, live/disabled status, verification timestamp, and version. It stores provider references only, never card data; only the protected registration CLI can create or change it after an SCA-complete off-session mandate exists.
- `oauth_issuance_control`: singleton with `enabled=false` by default, control version, exact release-set SHA, enable/disable receipt digest, actor, and timestamp. Token exchange and control mutation share one global issuance lock; enabled issuance additionally requires the DB release-set SHA to equal the injected identity of deployed Artifact C.
- `oauth_activation_approvals`: immutable approval digest primary key, release-set SHA, cohort kind `pilot|broader`, canonical exact client-ID set/hash, approver identity, issued/not-before/expiry timestamps, consumed timestamp, and activation audit reference. Consumption occurs in the same transaction as activation; an exact retry is idempotent, while reuse for any different set/release/cohort conflicts.
- `internal_delegation_nonces`: primary key `(signing_key_id,nonce)` with expiry, consumed once by the control plane.

#### agent_onboardings

- UUID primary key, `client_id`, server-assigned generation, idempotency key, and canonical request SHA256.
- Mode is `managed_mailbox` or `custom_domain`.
- Lifecycle states are only `provisioning`, `dns_pending`, `active`, `deprovisioning`, and `closed`. There is no terminal `failed` escape state.
- Immutable desired fields include normalized org name, canonical custom domain, and canonical local part.
- Result fields include org, domain, grant, inbox, mailbox address, subscription ID, and generation-owned billing reference.
- Workflow fields include monotonic `workflow_version`, `lease_owner`, `lease_expires_at`, provider operation/fence identifiers, attempt count, retry time, bounded last error, terminal reason, and timestamps.
- Unique `(client_id,idempotency_key)` and `(client_id,generation)` constraints.
- Partial uniqueness permits at most one non-closed generation per client. Permanent failures move to `deprovisioning` and must clean up before `closed`.
- Foreign-key behavior preserves tombstoned org identity and tolerates intentional deletion of child provider resources.

Workers claim with `SELECT ... FOR UPDATE SKIP LOCKED` plus CAS over state and `workflow_version`, then lease the operation before an external call. Results apply only when state, version, and lease still match. `close` increments the version, fencing an in-flight verifier. A stale provider success must enqueue a compensating disable/delete, and `closed` requires a provider re-read proving the side effect absent or disabled.

#### managed_mailbox_platform_domains and managed_mailbox_aliases

- `managed_mailbox_platform_domains` records the one configured platform `org_domain_id`, owner org, canonical domain, active/disabled state, and validation timestamps.
- `managed_mailbox_aliases` uses canonical lowercase address as primary key, unique onboarding ID, pre-generated inbox ID, org and platform-domain IDs, and state `reserved`, `active`, or `retired`.
- Platform registration snapshots every pre-existing inbox on the canonical domain and permanently backfills each address as `legacy_reserved_active` or `legacy_reserved_disabled` before writer enable. These rows have no onboarding owner and can never be allocated, reactivated under another inbox ID, or deleted.
- No production path deletes an alias. A retired address remains reserved forever, including for its former inbox ID.
- Allocation generates a lowercase Base32 encoding of 128 random bits with fixed `agent-` prefix, inserts `reserved`, creates the inbox with the pre-generated matching ID, and marks `active` in one transaction.
- Cloud 0009 installs the first inbox alias trigger. Forward-only Cloud 0010 replaces it after a serialized preflight, makes hosted active-address uniqueness use the same byte-preserving canonical equivalence as shared Store paths, rejects new noncanonical storage, and permits a registered managed address to activate only for its matching reserved/active inbox ID; `retired` always fails. This covers ordinary create, ensure, reactivate, direct SQL, and catch-all races without rewriting valid legacy address bytes.
- A database constraint/trigger prevents `catch_all_enabled=true` for a registered platform domain. Inbound fallback also recognizes the platform-domain registry and drops unknown recipients before the generic catch-all branch.

#### domain_ownership_claims

- Canonical domain is the primary key. Each row references the Core org-domain row, org, optional onboarding, `owner_kind=legacy|autonomous`, state `pending|provider_owned|releasing`, lease/version, and expiry.
- A migration preflight must resolve duplicate non-expired pending or provider-owned claims before 0009 is allowed to run; the migration backfills a single claim for each existing live domain.
- Under a temporary global domain-mutation fence, an operational preflight compares the full Resend domain inventory with local canonical domain/provider-ID pairs. Unknown or mismatched provider domains are written to a quarantine ledger and block writer enable until an audited delete or explicit adopt receipt resolves them. The fence remains held from inventory snapshot through Cloud 0009 activation so no provider-only orphan can appear in the gap.
- Every legacy and autonomous create, verify, delete, release, and GC path acquires the same global canonical-domain advisory lock and claim row. Org-scoped locking is not sufficient.
- Every existing foreign pending/provider-owned/releasing claim conflicts, whether or not its lease/expiry has elapsed. Expiry never grants takeover: under the canonical lock an expired pending claim is fenced into `releasing` and its owning workflow into `deprovisioning`; provider lookup and confirmed cleanup must complete before the claim can be released and another owner can bind the domain.
- Autonomous domain rows carry a stable `m2m-onboarding:` external-reference prefix. Generic pending-domain GC never deletes them; only onboarding deprovisioning releases their claim after provider state is proven safe.

#### Generation-bound billing and agent_billing_workflows

- Cloud 0009 adds nullable `onboarding_id` and generation provenance to existing `subscriptions`, with a partial unique constraint for autonomous rows. Legacy subscriptions remain null and are never canceled by an autonomous close.
- `agent_billing_workflows` is keyed by onboarding ID and stores the generation-bound subscription-create/payment/cancellation lifecycle: org ID, client ID, generation, workflow version, billing-profile version, plan, stable Stripe idempotency keys, create state `intent|provider_unknown|incomplete|active|requires_action|terminal`, external subscription/payment references, cancellation state `not_required|requested|confirmed`, attempt/retry fields, bounded error, and requested/checked/confirmed timestamps.
- `nerve_billing_subscribe` is modern-only, requires an active `m2m_org` principal plus `nerve:billing.subscribe`, takes no org/client/onboarding/generation/payment identifiers, and resolves all of them server-side. It acquires the common client/onboarding/billing locks, validates the registered spending cap and live mandate, and persists a version-bound intent plus stable Stripe idempotency key before any Stripe call.
- Stripe subscription creation uses only the preauthorized customer/payment-method references. A returned subscription may attach only by CAS to the same active generation, workflow version, and billing-profile version. `requires_action`, missing mandate, cap violation, or ambiguous payment remains fail closed and never produces paid evidence or a hosted action URL.
- The legacy `/v1/subscriptions/checkout` route rejects autonomous organizations before provider side effects. It remains unchanged for legacy operator-managed organizations and cannot create a generation-unbound autonomous subscription.
- Close increments the workflow version and fences new or in-flight subscription creation before leaving the transaction. It records cleanup required for every nonterminal generation-bound billing object, including provider-unknown, incomplete, `trialing`, `active`, or otherwise nonterminal subscriptions. No cleanup is `not_required` until provider readback proves that no subscription can still materialize or become chargeable.
- A create result or webhook arriving after the fence is attached only to the historical billing workflow for cleanup and immediately enqueues idempotent subscription cancellation; it is never accepted as paid evidence.
- A generation cannot become `closed` while any subscription-create outcome is unresolved or a required cancellation is unconfirmed. Confirmed workflow/evidence rows remain durable for audit.

#### agent_outbound_evidence

- Fields are org ID, source, source reference, effect, status, validity, revocation, policy version, bounded reason, actor, and timestamps; unique `(org_id,source,source_ref)`.
- V1 sources are only `paid_subscription`, `verified_custom_domain`, and `abuse_suspension`. `earned_trust` is deliberately absent.
- `paid_subscription` projects org-wide `email_compose_org_enabled`; verified custom domain remains domain-scoped and is checked from live Core ownership/readiness; suspension projects `email_outbound_suspended`.
- The actual mutation seam is internal/billing/stripe.go plus internal/store/store_billing.go. Under the billing/lifecycle lock, one store transaction atomically applies the subscription snapshot, entitlement, paid evidence, projected flags, and webhook processed marker.
- Payment evidence requires both a qualifying paid invoice/payment and authoritative current provider state. `invoice.paid` never directly maps a subscription to active; stale or ambiguous events trigger a Stripe subscription GET outside the transaction followed by a fenced CAS. A close/cancellation fence is monotonic and always wins over late payment or older subscription events.
- Reconciliation recomputes the same projection and never infers trust from inbound mail.

The Cloud 0009 down migration refuses when any durable client, key, assertion, nonce, activation approval, onboarding, alias, domain claim, rate, billing-workflow, or evidence row exists. Cloud 0010 is unconditionally forward-only because restoring the incomplete trigger/index would reopen the canonical-identity boundary.

### Existing Core state and Core 0029 used by runtime

Core 0029 is an additive OSS-authority migration. Before an org email token can be issued, every autonomous organization must have one `org_outbound_policy_state` row with a positive monotonic epoch and explicit existing feature rows:

- `autonomous_outbound_policy=true`;
- `email_compose_org_enabled=false` initially;
- `email_outbound_suspended=false` initially.

The security decision does not use the generic cached feature resolver. For an authenticated `m2m_org`, enqueue reads one transaction-scoped org policy snapshot and the current epoch directly from the database. Missing rows, malformed values, or read errors deny the send. Legacy principals retain existing behavior and are identified by principal kind, never inferred from a missing flag.

Core 0029 creates `org_outbound_policy_state(org_id, policy_epoch, updated_at)` and adds nullable `autonomous_policy_epoch`, `provider_started_at`, `provider_operation_id`, and `provider_resolved_at` columns to `outbox_messages`. Legacy rows keep all four columns null. Autonomous enqueue copies the locked current epoch. A provider operation ID is stable for the outbox row and provider idempotency key; retry never invents another logical operation. `provider_started_at IS NOT NULL AND provider_resolved_at IS NULL` is durable unresolved-provider evidence even after a worker crash.

Enqueue, outbox claim/provider-start, complaint/bounce suspension, operator suspension clear, and onboarding close use one per-org policy advisory lock and monotonic policy epoch. Enqueue and claim recheck the live epoch; suspension/close increments it and terminalizes all `queued` autonomous rows through the existing `failed` terminal status with bounded reason `policy_revoked`—the status constraint is not expanded. Immediately before a provider call, the worker takes the same lock, proves the saved epoch and live flags still allow the operation, and atomically records `provider_started_at` plus the stable provider operation ID. That commit is the send linearization point. A later suspension may not pretend the already-started operation was canceled: it fences all further starts and remains pending/deprovisioning until `provider_resolved_at` is set by a terminal response or provider idempotency replay/readback resolves an unknown outcome.

Close never calls cascading inbox deletion. It retires aliases, disables the generation-owned inbox, completes the outbox barrier, and then uses a Cloud-only autonomous tombstone operation that proves every remaining inbox is generation-owned and disabled, no `queued|sending` row exists, and every provider-started operation is terminal/read back. Existing legacy `TombstoneOrg` semantics remain unchanged; retained child rows preserve audit and FK integrity.

Runtime policy is:

1. Suspension denies both reply and compose.
2. `send_reply` is allowed only when the tenant-owned thread contains a real inbound message for the selected inbox and targets its latest inbound sender.
3. `compose_email` is allowed only when the selected inbox uses the same active, fully ready, org-owned custom domain, or `email_compose_org_enabled=true` from confirmed paid-subscription evidence.
4. A granted platform mailbox does not gain compose merely because the platform owner's domain is verified.
5. Token scopes remain a ceiling; live policy may deny a stale compose scope.
6. `nerve_billing_subscribe` is not an email send authorization and never bypasses the live policy/evidence checks.

### OAuth and Streamable HTTP wire contract

Public endpoints:

- Runtime: `GET /.well-known/oauth-protected-resource`, `GET /.well-known/oauth-protected-resource/mcp`, and `POST /mcp`.
- `auth.nerve.email`: `GET /.well-known/oauth-authorization-server`, `GET /.well-known/jwks.json`, and `POST /oauth/token`.

Canonical constants are:

- MCP resource: `https://nerve-runtime.fly.dev/mcp`.
- Issuer: `https://auth.nerve.email`.
- Token endpoint: `https://auth.nerve.email/oauth/token`.
- JWKS URI: `https://auth.nerve.email/.well-known/jwks.json`.
- Scopes in canonical order: `nerve:onboarding`, `nerve:email.read`, `nerve:email.search`, `nerve:email.draft`, `nerve:email.reply`, `nerve:email.compose`, `nerve:billing.subscribe`.

Both protected-resource paths return the same document with exact resource, `authorization_servers=["https://auth.nerve.email"]`, the canonical `scopes_supported`, and `bearer_methods_supported=["header"]`. Authorization-server metadata returns the exact issuer/token/JWKS values, `grant_types_supported=["client_credentials"]`, `token_endpoint_auth_methods_supported=["private_key_jwt"]`, `token_endpoint_auth_signing_alg_values_supported=["PS256","RS256"]`, and the same scopes. Because this authorization server supports no authorization-endpoint grant, it omits `authorization_endpoint` and `response_types_supported` under the interpretation proposed by reported RFC 8414 Errata 7793; it never emits a forbidden empty array or falsely advertises `token`/`code`. Phase 0 pins a golden metadata fixture and proves every supported MCP ext-auth client accepts this honest client-credentials-only shape; failure blocks implementation rather than silently inventing a response type. Metadata/JWKS use `Content-Type: application/json`, `Cache-Control: public, max-age=300, must-revalidate`, stable ETags, and conditional 304.

`POST /oauth/token` accepts only `application/x-www-form-urlencoded` with `grant_type=client_credentials`, `client_assertion_type=urn:ietf:params:oauth:client-assertion-type:jwt-bearer`, `client_assertion`, `resource=https://nerve-runtime.fly.dev/mcp`, and one canonical space-delimited `scope` value. `client_id` may be omitted as permitted by the pinned Client Credentials extension: identity is derived from the verified assertion `iss=sub`; when the field is present it must equal that value exactly. JSON, client secret, org ID, generation, duplicate parameters, or caller-selected binding is rejected. Token success/error responses set `Content-Type: application/json`, `Cache-Control: no-store`, and `Pragma: no-cache`. Success returns only `access_token`, `token_type=Bearer`, `expires_in`, and canonical actual `scope`; no refresh token is issued.

Public-input limits are exact and enforced with `http.MaxBytesReader` or the equivalent before form parsing, Base64 decoding, JSON/JWK decoding, signature verification, or any database lookup:

- OAuth wire body: 32 KiB, including chunked requests or a false/missing Content-Length.
- Compact assertion: 16 KiB, exactly three nonempty segments, with encoded header/payload/signature caps of 2,048/8,192/4,096 bytes.
- `client_id`, `kid`, and `jti`: 1-128 ASCII bytes; registered `kid` must additionally equal the 43-character unpadded RFC 7638 SHA-256 thumbprint.
- Canonical scope string: at most 512 ASCII bytes, at most six unique tokens in one non-mixable request, and at most 64 bytes per token.
- Modern lifecycle/delegation request body: 64 KiB; idempotency key 1-128 bytes; organization name 1-160 Unicode scalar values and at most 640 UTF-8 bytes; canonical domain at most 253 ASCII bytes with labels at most 63; custom local part at most 64 bytes under the existing conservative ASCII grammar.

Every limit has `N-1`, `N`, `N+1`, chunked, duplicate-field, false Content-Length, and oversized-segment fixtures. Oversize rejects before cryptography or persistence with a stable non-oracular result.

Assertion validation requires nonempty equal `iss` and `sub`, exact token-endpoint audience, mandatory thumbprint-derived `kid` and `jti`, no more than 60 seconds between `iat` and `exp`, no more than 30 seconds clock skew, a live registered key with matching pinned algorithm, and durable one-time JTI consumption through `retain_until >= exp + 30 seconds`. The verified `iss/sub` value is the client ID used for lookup and locking.

OAuth errors are an exact, non-oracular contract. The RFC 6749 token codes remain: malformed/missing/duplicate parameters use HTTP 400 `invalid_request`; a different grant uses 400 `unsupported_grant_type`; invalid resource uses 400 `invalid_target` from the pinned resource-indicator extension; unregistered, mixed, or disallowed scopes use 400 `invalid_scope`; and absent/invalid/replayed assertion, unknown/revoked client, or invalid key/signature uses 401 `invalid_client`. The versioned Nerve token-error extension in docs/MCP_Contract.md defines only `nerve_rate_limited` at HTTP 429 with integer `Retry-After` and `nerve_server_error` at HTTP 500; clients must treat both as retryable without inferring client/key existence. It never reuses authorization-endpoint-only `temporarily_unavailable` or `server_error` at the token endpoint. A non-form media type receives 415 with `invalid_request`; a non-POST method receives 405 plus `Allow: POST`. Every body is only `error` and an optional bounded stable `error_description`.

Scope selection is exact and non-mixable:

- A request containing only `nerve:onboarding` returns a five-minute `token_use=m2m_onboarding` token with client ID, the generation selected under the client lock, and no org ID, regardless of whether that generation is active.
- Email and optional `nerve:billing.subscribe` scopes return a fifteen-minute `token_use=m2m_org` token only for an active generation and include client ID, generation, org ID, and actual state-derived scopes. Billing scope is issued only when registration permits it and a live billing profile exists; it may be combined with email scopes but never with `nerve:onboarding`.
- Onboarding and email scopes cannot be combined in one token request.
- Org-token JTI rows use actor `oauth_client:<client_id>:g:<generation>` for targeted revocation.

Every access JWT is signed only with the auth.nerve.email issuer-owned PS256 current key and header `kid`; its claims are `iss=https://auth.nerve.email`, `aud=https://nerve-runtime.fly.dev/mcp`, `sub=<client_id>`, `client_id`, integer `generation`, `token_use`, canonical space-delimited `scope`, `iat`, `nbf`, `exp`, and unique `jti`. `m2m_org` additionally has `org_id`; `m2m_onboarding` must not contain `org_id`. The issuer holds separate current/next private keys, publishes only their public JWKs, switches by configured issuer `kid`, retains the prior public key through maximum token lifetime plus skew, and then retires it. Registered client assertion JWKs never appear in AS JWKS, and assertion `alg`/`kid` cannot select the access-token signing key or algorithm. No assertion claim other than the verified client identity selects any access-token claim.

Issuance, active/deprovisioning transitions, targeted generation revocation, and operator `revoke-client` use one transaction helper and lock order: global issuance lock, client advisory lock, org advisory lock when present, then `SELECT ... FOR UPDATE` over control/client/key/onboarding rows. Issuance first proves the global control is enabled for the exact injected release-set SHA, then rechecks client/key, generation, org tombstone, and policy state immediately before insert and returns JWT bytes only after commit. `revoke-client` atomically revokes the client, suspends outbound, moves a live generation to deprovisioning, and revokes its actor tokens; the reconciler completes cleanup.

Every modern POST sends `Content-Type: application/json`, `Accept: application/json, text/event-stream`, `MCP-Protocol-Version: 2026-07-28`, `Mcp-Method`, conditional `Mcp-Name`, matching body method/name, and the two required `_meta` keys `io.modelcontextprotocol/protocolVersion` and `io.modelcontextprotocol/clientCapabilities`. SDK 0.3 additionally sends its fixed optional `clientInfo`, producing this shape alongside any method-specific `_meta` entries:

```json
{
  "_meta": {
    "io.modelcontextprotocol/protocolVersion": "2026-07-28",
    "io.modelcontextprotocol/clientInfo": {"name": "nerve-email-python", "version": "0.3.0"},
    "io.modelcontextprotocol/clientCapabilities": {
      "extensions": {"io.modelcontextprotocol/oauth-client-credentials": {}}
    }
  }
}
```

The header and body protocol versions must match. `clientInfo` is optional for conformant external agents and is never authentication input; omission succeeds, while a present value must satisfy the SDK `Implementation` shape and configured string bounds. Server discovery advertises the same independently versioned extension snapshot. For M2M principals, an absent/malformed required protocol version or capabilities object, a mismatched version, or an absent capability fails before tool dispatch with the applicable SDK `HeaderMismatch` (`-32020`), `MissingRequiredClientCapabilities` (`-32021`), or `UnsupportedProtocolVersion` (`-32022`) error and required structured data. Legacy/Cloud-key principals using the modern protocol must still provide the required base protocol metadata, but do not require the OAuth extension capability.

Invalid/expired bearer responses use HTTP 401 with `WWW-Authenticate: Bearer resource_metadata="https://nerve-runtime.fly.dev/.well-known/oauth-protected-resource/mcp", error="invalid_token"`. Insufficient scope uses HTTP 403 with `error="insufficient_scope"` and the required scope.

SDK 0.3 selects its parser from the base response media type. JSON must contain one matching JSON-RPC response. SSE aggregates multiline `data:` fields, ignores comments and unrelated request notifications, and accepts exactly one final response with the matching ID. Missing/unsupported content type, duplicate final response, wrong ID, malformed JSON, or EOF before the final response is a protocol error. Conformance tests run the Go handler with both `JSONResponse=true` and `false` even if production explicitly chooses JSON responses.

### MCP onboarding tools

All four tools are modern-only and require `m2m_onboarding` with `nerve:onboarding`.

#### nerve_onboarding_start

- Input is bounded `idempotency_key`, normalized organization name, mailbox mode, and custom domain/local part only for custom-domain mode.
- Resolve client ID and generation exclusively from the principal and acquire the common client lifecycle lock.
- Same key and canonical request returns the persisted generation; same key with a different request hash returns a typed conflict.
- The token may create only its preselected generation. Any different non-closed generation is returned as a conflict; after closure, a fresh token plus a new idempotency key allocates generation N+1.
- Result includes onboarding ID, generation, state, mode, address when known, DNS records/checks when relevant, `next_action`, `retry_at`, and `reauthorize`; it never exposes private/provider keys or internal owner/grantee IDs.

#### nerve_onboarding_status

- Resolve only the generation claim in the onboarding principal. Do not substitute the client's live/latest generation or accept arbitrary client/org/domain/inbox lookup.
- Return the same stable state document as start. `reauthorize=true` means a separate email-scope exchange is currently permitted.

#### nerve_onboarding_verify_domain

- Valid only for the caller's own custom-domain generation.
- Poll authoritative DNS and provider state; never accept `verified=true` or equivalent caller assertions.
- Incomplete DNS returns a normal successful complete result with `structuredContent.state=dns_pending`, current records/checks, `next_action=configure_dns_then_verify`, and `retry_at`.
- Never use `input_required`, `inputRequests`, `inputResponses`, or `requestState`; after changing DNS the client makes a new status/verify call with a new JSON-RPC ID.

#### nerve_onboarding_close

- Require an idempotency key and expected generation, both checked against the caller's own state.
- Under the common lifecycle locks, increment workflow version, move to `deprovisioning`, disable outbound, and targeted-revoke only generation-bound `m2m_org` email-token rows; onboarding status/idempotent-close tokens expire normally.
- Fence every generation-associated nonterminal subscription-create/payment/subscription workflow, including provider-unknown, incomplete, `requires_action`, and `trialing`; immediately cancel every materialized subscription with deterministic idempotency keys, no automatic prorated refund, and no requested final invoice. Existing obligations are preserved for audit.
- Provider calls occur outside the DB transaction under workflow fencing. Timeout/unknown outcome remains deprovisioning; reconciliation retrieves by subscription ID and retries. Permanent API failure alerts/DLQs but never falsely closes.
- `closed` requires terminal Stripe evidence (`canceled` or proven absent), terminal mailbox/domain/provider cleanup, retired alias where applicable, tombstoned org, and released live claims. Entitlement is never transferred to the next generation.
- Repeated close returns the same durable progress, including whether cancellation is pending or confirmed.

Every onboarding tool returns `resultType=complete`. Business failures use `isError=true`; durable waiting states such as `dns_pending` and `deprovisioning` are successful structured results.

#### nerve_billing_subscribe

- Modern-only and available exclusively to an active `m2m_org` principal with `nerve:billing.subscribe`; it is not one of the four onboarding lifecycle tools.
- Input is only a bounded plan code and idempotency key. Client, org, onboarding, generation, Stripe customer, payment method, and mandate are resolved from the authenticated principal and protected registration state; any such caller-supplied field is rejected.
- Persist the generation/workflow/billing-profile-version-bound intent before calling Stripe. A canonical replay returns the same workflow/subscription; a changed plan under the same key conflicts.
- Return `active` only after provider readback proves a currently qualifying paid subscription and the same transaction writes paid evidence. `processing`, `provider_unknown`, and `requires_action` are complete structured results with compose still denied; no hosted Checkout URL, card input, or client secret is returned.
- Close/revoke/suspension fencing takes precedence over the create result. A subscription that materializes after the fence is retained only as historical cleanup evidence and is immediately canceled.

### Versioned outbound safety policy V1

Store defaults in `configs/policy/autonomous-outbound-v1.yaml`, record its version/hash on evidence and release artifacts, and mirror the exact bytes into OSS:

- Reply-only organizations: 20 accepted replies per UTC day and 5 per recipient hash per UTC day.
- Compose-enabled organizations: 100 accepted sends per UTC day and 25 first-time recipients per UTC day, further limited by subscription plan.
- A complaint creates immediate `deny_all` suspension.
- A permanent-bounce rate of at least 5 percent with at least 20 attempted deliveries in a UTC day creates suspension.
- Rate exhaustion resets with the next UTC bucket. Suspension requires an audited operator clear.
- Paid evidence requires a successful Stripe invoice/payment and a currently qualifying paid subscription; trialing alone is not payment.
- Verified-custom-domain evidence is domain-scoped and valid only while the selected inbox uses that same active, fully ready, org-owned domain.
- Inbound message volume, sender count, self-mail, SPF, DKIM, and DMARC never create compose evidence in V1.

Use existing `org_usage_counters` for reply, total send, first-recipient, and recipient-hash UTC buckets. Each accepted reservation writes its matching `usage_events` row in the same transaction. The global replay ID is SHA-256 over a versioned, unambiguous length-prefixed tuple `(org_id, tool_name, idempotency_key, meter, dimension)`; recipient-specific meters use the canonical recipient hash as `dimension`, and non-dimensional meters use an explicit empty value. Provider failure does not refund an abuse unit; idempotent replay never consumes a second unit.

Reservation and reconciliation take the same `(org_id,meter,period_start,period_end)` counter-row lock. Reconcile holds it while computing SUM and applying SET in one transaction; a sender either commits its event/counter before the SUM or waits and increments after the reconciled value. Cross-org or cross-tool reuse of one idempotency key cannot collide, and reconciliation cannot erase a concurrent accepted send.

## Phase 0: Repair the Proof, Compatibility, and Artifact Chain

### Overview

Make every later migration, protocol, rollback, and production assertion executable before adding feature state. This phase defines the artifact identities and transition machinery; it does not cross Cloud schema 8 to 9.

### Changes Required

#### 0.1 Make the Cloud E2E gate real

**Files**

- `Makefile`
- `.github/workflows/cloud-e2e-matrix.yml`
- `internal/cloudapi/e2e_matrix_test.go`
- `.github/workflows/ci.yml`
- new `deploy/cloud/oss-source.lock`

**Changes**

- Remove the stale Cloud copy of the tagged runtime matrix after preserving Cloud-only assertions as ordinary control-plane integration tests.
- Bootstrap `oss-source.lock` from the current verified runtime.lock source revision/manifest before changing any shared bytes. This makes the separate authority input available to the first Phase 0 CI run; Phase 0.6 advances it only after the green OSS-first auth tranche.
- Resolve `deploy/cloud/oss-source.lock`, checkout that exact OSS authority revision, run `go test -list`, assert `TestCloudE2EMatrix` exists, then execute the authoritative tagged suite with `-count=1 -v`. Deployed-artifact verification continues to use `runtime.lock`; source tests never reinterpret it as a candidate-source pointer.
- Capture output and fail on any skip. Keep Cloud OAuth/onboarding tests outside build tags.
- Use Go 1.25 consistently in Cloud CI because Cloud `go.mod` already requires it.

#### 0.2 Build once and verify all control-plane binaries

**Files**

- `.github/workflows/ci.yml`
- `.github/workflows/control-plane-deploy.yml`
- `deploy/cloud/Dockerfile.control-plane`
- new `scripts/release/generate_control_plane_manifest.sh`
- new `scripts/ci/verify_control_plane_artifact.sh`
- new `internal/release/release_set.go`
- new `internal/release/release_set_test.go`
- `scripts/deploy/ensure_reconcile_machine.sh`
- `scripts/ci/test_ensure_reconcile_machine.sh`
- `cmd/nerve-control-plane/main.go`
- `cmd/nerve-reconcile/main.go`
- `cmd/nerve-migrate/main.go`
- `cmd/nerve-flags/main.go`
- `cmd/nerve-drill/main.go`
- Phase 1 new `cmd/nerve-oauth-clients/main.go`
- `internal/startup/migrations.go`

**Changes**

- Build the image once in successful main CI and publish its immutable source digest plus a control-plane manifest.
- OCI labels and the manifest pin source revision; Core/Cloud migration hashes and heads; compiled min/max windows; policy hash; build time; and SHA256 for every shipped database-mutating executable: `/app/nerve-control-plane`, `/app/nerve-reconcile`, `/app/nerve-migrate`, `/app/nerve-flags`, `/app/nerve-drill`, and, once added in Phase 1, `/app/nerve-oauth-clients`. CI fails if an executable in the image is absent from the manifest or allowlist.
- Give every shipped database-mutating binary a shared read-only `compatibility --json` entry point before its first listener, lease, provider call, or mutation. The immutable build manifest carries `artifact_role=validation|A|B|C` and `release_set_required` (`validation/A=false`, `B/C=true`). Validation is CI-only and always refuses normal startup; an absent environment marker can never downgrade B/C to transition behavior. The manifest-bound binary reports its own build identity. For B/C deployments, the workflow injects `NERVE_RELEASE_SET_SHA` and `NERVE_RELEASE_SET_ENVELOPE_B64`, whose decoded maximum is 64 KiB and which contains the canonical signed release-set bytes plus offline DSSE/Sigstore verification bundle. The shared verifier caps and decodes the envelope, verifies canonical SHA and signature plus pinned repository/ref/workflow identity using vendored trust material, hashes its own executable/manifest, and proves exact role/image/manifest/binary/window membership before reporting the runtime-bound release-set identity. A SHA without the signed bytes/bundle is never sufficient, and the release-set identity is never compiled into an artifact that the set itself hashes. Environment may tighten verification but cannot disable the manifest requirement.
- Deployment derives the image only from a verified CI artifact, validates its attestation/manifest, and proves the resolved digest and compatibility output for every active and stopped web Machine and the scheduled reconciler Machine.
- Replace free-form control-plane image inputs with an artifact run identity and manifest SHA.

#### 0.3 Separate status verification from migration application

**Files**

- `scripts/deploy/run_migration_machine.sh`
- `scripts/ci/test_run_migration_machine.sh`
- `scripts/ci/verify_cloud_deploy_order.sh`
- `.github/workflows/deploy.yml`
- `.github/workflows/ci.yml`
- new `.github/workflows/cloud-0009-transition.yml`
- new `scripts/deploy/start_control_plane_web_machines.sh`
- new `scripts/ci/test_start_control_plane_web_machines.sh`
- `scripts/deploy/cloud_deploy.sh`
- `docs/REPO_SPLIT_RUNBOOK.md`

**Changes**

- Split the migration runner into explicit read-only `verify-status` and mutating `apply` operations. The precheck must never run `up` implicitly.
- Both operations require exact current version, head, and pending list. Add the intentional pre-state `current=0008`, `head=0009`, `pending=[0009]`; reject every other dirty set.
- Create a dedicated transition workflow using the same `concurrency.group: deploy-${target_env}` as normal deployment. Child workflows receive `caller_holds_deploy_lock=true` and cannot acquire a second lock.
- Make steady-state `deploy.yml` refuse to cross 8 to 9 and require a valid transition receipt once schema 9 exists.
- Remove obsolete Core 19/Cloud 7 defaults and derive targets only from verified manifests.

#### 0.4 Define an A transition bundle, signed release set, and candidate path

**Files**

- new `schemas/mcp2026-transition-bundle.schema.json`
- new `scripts/release/build_mcp2026_transition_bundle.sh`
- new `scripts/ci/verify_mcp2026_transition_bundle.sh`
- new `schemas/mcp2026-transition-receipt.schema.json`
- new `scripts/release/generate_mcp2026_transition_receipt.sh`
- new `scripts/ci/verify_mcp2026_transition_receipt.sh`
- new `schemas/mcp2026-release-set.schema.json`
- new `scripts/release/build_mcp2026_release_set.sh`
- new `scripts/ci/verify_mcp2026_release_set.sh`
- new `scripts/release/generate_mcp2026_release_set_envelope.py`
- new `scripts/ci/verify_mcp2026_release_set_envelope.py`
- new `scripts/ci/test_mcp2026_release_set_envelope.sh`
- new `schemas/mcp2026-runtime-mirror-receipt.schema.json`
- new `scripts/ci/verify_mcp2026_runtime_mirror_receipt.py`
- new `schemas/mcp2026-legacy-runtime-baseline.schema.json`
- new `scripts/release/generate_legacy_runtime_baseline.py`
- new `scripts/ci/verify_legacy_runtime_baseline.py`
- new OSS `schemas/mcp2026-legacy-bridge-receipt.schema.json`
- new OSS `scripts/release/build_mcp2026_legacy_bridge.sh`
- new OSS `scripts/ci/verify_mcp2026_legacy_bridge.py`
- new OSS `.github/workflows/mcp2026-legacy-bridge.yml`
- new `.github/workflows/mcp2026-capture-runtime-baseline.yml`
- new `.github/workflows/mcp2026-rollback-runtime-bridge.yml`
- new `.github/workflows/mcp2026-phase0-rehearsal.yml`
- new `deploy/cloud/oss-source.lock`
- new `deploy/cloud/runtime-candidate.lock`
- new `scripts/ci/verify_runtime_candidate_lock.sh`
- `deploy/cloud/runtime.lock`
- `scripts/ci/verify_runtime_lock.sh`
- `.github/workflows/runtime-deploy.yml`
- OSS `scripts/release/generate_runtime_manifest.sh`
- OSS `.github/workflows/docker-publish.yml`
- `.github/workflows/publish-python-sdk.yml`

**Changes**

- Define an attested `mcp2026-transition-bundle.json` available in Phase 1 before B/C/runtime/SDK exist. It pins only Cloud SHA, Artifact A image/manifest and all six shipped database-mutating binary hashes/windows, Cloud/Core migration hashes/heads, issuance-off state, source CI/workflow identity, and the production target. The Phase 1 transition accepts only its artifact run ID plus SHA.
- Define and verify an attested transition-receipt schema. The receipt binds the transition-bundle digest, exact A manifest/image/binary identities, pre/post schema evidence, every active/stopped/scheduled Machine identity and resolved digest, issuance-off proof, target, workflow identity, and timestamps.
- After the signed A transition receipt exists, stop rebuilding or publishing A from later main SHAs. The ordinary `control-plane-artifact` CI job builds a distinct local-only `validation` manifest/image with windows covering the checked-in migration heads, verifies every binary through `compatibility --json`, uploads only validation evidence, and has no GHCR/sign/deploy output. The validation role is accepted only by offline manifest verification and fails `VerifyStartup` before listener or mutation.
- Define one attested `mcp2026-release-set.json` pinning final Cloud and OSS producer SHAs; a preselected final unused runtime semver; runtime candidate index and linux/amd64 digests; runtime-manifest SHA and MCP/Core windows; a verified pre-release Fly-mirror receipt and resolved Machine digest; the signed historical v0.0.17 baseline receipt; a required distinct signed R0 legacy-bridge receipt and its index/platform/Fly digests; historical A Core `[28,28]`/Cloud `[8,9]`; future B Core `[28,29]`/Cloud `[9,10]`; future C Core `[29,29]`/Cloud `[10,10]`; each A/B/C member's own manifest-derived Cloud source revision, exact Core/Cloud windows, OCI index digest, linux/amd64 digest, equal resolved Fly Machine digest, manifest/binary identities, and protected producer; Core 29 and Cloud 10 migration heads/hashes; exact release-set issuance-off and Cloud 0010 transition specifications plus both protected producer identities; SDK 0.3 filename/SHA; immutable SDK 0.2 SHA; outbound-policy version/SHA; conformance commit; and producing workflow identities. Verification rejects a role whose member source/window differs from its immutable manifest and enforces the exact allowlisted B→C contraction diff.
- The final release set embeds the verified transition-bundle and Phase 1 transition-receipt digests plus immutable protected artifact locators for each: producing run ID, artifact name, object filename, expected SHA, repository/ref/workflow identity, and attestation identity. Later workflows derive and reverify the canonical historical objects from those locators without caller-supplied receipt inputs, so A identity/provenance cannot be substituted later.
- Verify GitHub OIDC/Sigstore attestations, repository/ref/workflow identity, and subject digest. Only Phase 1 transition consumes transition-bundle inputs. Phase 9 and 10 component selection accepts only `release_set_run_id` plus `release_set_sha`—never component overrides—and derives build/deploy components from that set. The Phase 9 Cloud 0010 transition may additionally accept only the fresh independently attested pre-0010 release-set issuance-off receipt run ID/SHA; it derives the historical 0009 receipt from the release set rather than caller input, and its receipt binds both predecessor receipt digests. Phase 10 paths may additionally accept or derive the independently attested 0010 transition, issuance-control enable/post-disable, lifecycle, soak, promotion, or one-use approval evidence required by that action only when each artifact names the same release-set SHA and its expected protected producer/workflow identity.
- Add the policy YAML to OSS exact mirror, runtime manifest, and OCI labels. Control-plane manifest pins the same hash; mismatch is fatal.
- Support a separate pre-tag `runtime-candidate.lock` at a digest-addressed OCI/artifact locator. Before its Phase 8 build, select and prove one final runtime semver unused across git tags, GitHub Releases, and public OCI tags; freeze that value into the candidate manifest/OCI labels and later release set without creating the tag. Production `runtime.lock` keeps its strict released-artifact contract and physically describes v0.0.17 through R0 and candidate deployment, but ceases to select the deployed runtime once protected release-set evidence selects R0. R0 uses its own release-set/receipt identity and never reuses or rewrites the public v0.0.17 lock, tag, version, or digest.
- Remove `runtime.lock` from any automatic rollout path filter. Runtime deployment is called explicitly with verified release-set evidence; changing candidate/public lock metadata alone cannot restart Machines.
- OSS candidate build pushes immutable bytes/digests without a semver tag or GitHub Release. Post-soak promotion retags that already-tested digest and publishes the exact manifest without rebuild.
- Remove direct SDK publish dispatch. SDK 0.3 publication is callable only by post-soak promotion with matching release-set evidence.
- Capture the current v0.0.17 production state before runtime.lock changes: exact source revision, GitHub Release manifest/assets, GHCR index/platform digests, current Fly content-addressed tag/Machine digest, contract/core hashes, and verification workflow identity. The signed baseline receipt is immutable and later embedded in the final release set.
- Build a distinct non-semver R0 legacy bridge from the pinned v0.0.17 source plus one machine-verified allowlisted patch that widens only the compiled Core window from `[28,28]` to `[28,29]` and adds the required build/attestation wiring. A protected OSS workflow must reject every other source, dependency, API, protocol, SQL, policy, or manifest-behavior delta; build reproducibly; and emit a signed receipt binding source, patch digest, exact diff allowlist, index/platform/Fly digests, Core window, and producer identity.
- Run the actual R0 binary with production `NM_MIGRATE_ON_START=verify`: first against Core 28, where its legacy HTTP/MCP/SDK 0.2 and SQL behavior must match the immutable v0.0.17 artifact, then against additive Core 29, where startup, enqueue, claim, delivery, and inspection must pass. A hand-written legacy-SQL fixture proves only schema/statement compatibility and can never substitute for these artifact-level executions.
- The dedicated below-vNext rollback workflow accepts only the final release set, a fresh Phase-10 post-disable issuance-control receipt, and a complete lifecycle-drain receipt. It derives R0 solely from the embedded bridge member, keeps control plane B, deploys the exact content-addressed R0 Fly image, and verifies every Machine; it rejects historical/pre-0010 issuance-off evidence, accepts no raw image/tag/version input, and never depends on later `runtime.lock` contents. Immutable v0.0.17 is not an authorized target after Core 0029.
- Add a protected Phase 0 operator rehearsal under the shared `deploy-<environment>` lock. It accepts only a successful main Artifact A run and exact manifest SHA, verifies its Sigstore identity and manifests, mirrors those exact bytes to Fly Registry, creates a stopped ephemeral Machine, proves its resolved digest before start, executes every manifest-listed `compatibility --json` entry point, validates the non-deploying candidate-locator fixture independently from `runtime.lock`, emits signed evidence, and destroys the exact Machine on every exit. It never deploys a durable service or applies a migration.

#### 0.5 Pin protocol conformance tooling

**Files**

- OSS new `scripts/ci/test_mcp_conformance.sh`
- OSS `.github/workflows/ci.yml`
- Cloud `.github/workflows/ci.yml`
- Cloud new `sdk/python/src/nerve_email/oauth.py`
- Cloud new `sdk/python/tests/test_oauth_metadata.py`
- Cloud new `sdk/python/requirements.lock`

**Changes**

- Pin the official 2026-07-28-capable conformance runner to `81eb1c3edaed87d7fd585d7b80186da7a2960660` and the independently versioned OAuth Client Credentials extension to `modelcontextprotocol/ext-auth@ce15435bf4e35a0ec972dd7cd8ce4c81d609cc3e`.
- Reference code/actions by immutable SHA and cache their lockfiles by digest. Never use `latest` or expected-failure results for advertised capabilities.
- Add a golden client-credentials-only authorization-server metadata fixture that intentionally omits both `authorization_endpoint` and `response_types_supported` under RFC 8414 Errata 7793. Run it against every pinned supported ext-auth client. An empty array, a fabricated response type, or any client rejection blocks the phase and requires an explicit architecture revision rather than a compatibility lie.
- Bring the isolated Python SDK 0.3 OAuth consumer forward into Phase 0: implement the exact AS-metadata parser plus `PrivateKeyJWTAuth` key validation, RFC 7638 `kid` derivation, and one-minute PS256/explicit-RS256 assertion construction now. This is the first pinned supported ext-auth consumer and its golden metadata tests run in Cloud CI. Keep it unexported from the 0.2 public package surface and do not publish or identify a locally rebuilt 0.2 wheel as the immutable published 0.2 artifact; Phase 8 integrates this already-tested module into the complete 0.3 client and builds the final wheel once.
- Pin `PyJWT[crypto]==2.13.0` and `cryptography==49.0.0` with hashes in the Phase 0 test environment. The Phase 0 gate verifies the lock before installing the consumer dependencies.

#### 0.6 Separate OSS source authority and land shared auth before Artifact A

**Files**

- new `deploy/cloud/oss-source.lock`
- `.github/workflows/ci.yml`
- `scripts/sync/sync-manifest.yaml`
- `scripts/ci/verify_runtime_lock.sh`
- `docs/REPO_SPLIT_RUNBOOK.md`
- OSS `internal/auth/**`
- OSS sync manifest and exact-mirror verifier
- Cloud exact mirrors of `internal/auth/**`

**Changes**

- Make `runtime.lock` authoritative only for the deployed runtime artifact and create `oss-source.lock` for the exact OSS source revision/manifest used by shared-code CI. Baseline values are equal; they may diverge only while a verified candidate is under construction and must converge again at promotion.
- In nerve-oss first, add the typed `m2m_onboarding|m2m_org` verifier/JWKS/context foundation, include `internal/auth/**` in the shared exact-mirror manifest, and merge a green OSS authority commit before Artifact A is built.
- Import those exact bytes into Cloud, advance only `oss-source.lock`, and make exact-mirror CI read that lock. Cloud-only assertion issuance, client registry, billing profile, and OAuth control remain in `internal/oauth/**`; no Cloud-first copy of a shared auth file is permitted.
- Every automated OSS-to-Cloud sync PR must atomically advance `deploy/cloud/oss-source.lock` to the triggering OSS commit and the SHA-256 of that commit's `sync-manifest.yaml`. Creating exact mirrors while leaving the prior source lock is a failed sync, not a review-only discrepancy.
- Prove runtime-lock verification still validates the deployed v0.0.17 image/source labels independently. Candidate source movement cannot silently retarget a production deploy.

### Success Criteria

#### Automated verification

- [x] Cloud E2E enumerates and executes the authoritative matrix with no skips.
- [x] Cloud fmt, vet, tests in two time zones, and image build pass on Go 1.25.
- [x] Every database-mutating binary present in each tested image reports the same manifest-bound windows and refuses incompatible schemas before mutation; manifest tests reject an unlisted executable and reserve the six-binary Artifact A inventory checked in Phase 1.
- [x] Missing, wrong, or substituted transition-bundle/receipt identity is rejected, and the Phase 1 transition has no dependency on the not-yet-created final release set.
- [x] Direct-start B/C with no marker/envelope, and every missing, oversized, malformed, wrongly signed envelope, SHA/identity/role mismatch, or absent executable membership fails compatibility before any listener or mutation without rebuilding an artifact; verification passes with network disabled.
- [x] Deploy rejects wrong source, schema, binary, policy, attestation, or stopped-Machine digest.
- [x] Read-only migration verification accepts exactly 8/head9/pending9 without applying it.
- [x] Steady deploy refuses an 8-to-9 transition.
- [x] Candidate-lock verification works without a semver release while independent production runtime-lock verification for v0.0.17 remains green.
- [ ] Release-set schema/build/verification reject omitted or free-form component identities, require the independently proven R0 member, enforce the exact A/B/C/runtime window matrix through Cloud 10, bind the release-set issuance-off and 0010 transition specifications/producers, and freeze a still-unused final runtime semver without changing candidate manifest bytes later.
- [x] `oss-source.lock` and `runtime.lock` are independently verified; exact-mirror CI follows only source authority, deploy follows only artifact authority, and an attempted cross-use fails.
- [x] The OSS-first shared-auth commit and manifest entry exist before Artifact A's source SHA; Cloud copies are byte-identical.
- [x] The client-credentials-only metadata fixture passes every pinned ext-auth client while empty/fabricated `response_types_supported` fixtures fail.
- [x] The signed v0.0.17 baseline receipt resolves the current Release/GHCR/Fly bytes and rejects every digest/source/manifest substitution.
- [x] The protected R0 workflow proves an exact allowlisted v0.0.17-source delta, reproducible distinct bytes, legacy equivalence on Core 28, and actual production-startup compatibility on Core 29; its receipt rejects any substituted patch, source, digest, window, or workflow identity.

#### Operator verification

- [x] An ephemeral Fly rehearsal proves the shared lock, artifact manifests, compatibility commands, and candidate locator.
- [x] The runbook contains no obsolete migration target or unverified free-form image input.

#### Phase 0 production evidence (2026-08-10)

- Legacy runtime baseline: protected main workflow run `31370208433`, Actions artifact `9055729218`, receipt SHA-256 `ab616874e51835aa0e2096e07c52d4c748aaca6105ceec5b5fd609b02e6bb268`. The signed member binds `target_env=cloud-production`, `fly.app=nerve-runtime`, v0.0.17 Release assets, source `a794be9f2697e0864d3a31da8f087577e9748f7e`, GHCR/Fly digest `sha256:eaab11e78806e3ed730367c311b1fc30c1360e5be9897d329ec9208912f81765`, and both production runtime Machine IDs. The protected workflow and an independent downloaded-artifact verification both accepted the exact bytes; negative tests reject substituted environment, app, digest, source, and manifest identities.
- Artifact A authority: protected main Cloud CI run `31366330167`, image `ghcr.io/dsmolchanov/nerve-cloud/control-plane@sha256:cc46c364dd99017d25afd6e6a70350cbebedfebc08eea08e90d5f688d1aaa39b`, manifest SHA-256 `1de234082197d4c94530c80929fa0ffecba0122388c4f12e402596000ab171e6`. The private-repository Sigstore producer completed successfully on main.
- Fly rehearsal: protected main workflow run `31368751660`, Actions artifact `9055204919`, receipt SHA-256 `60c706a844369216ba9182c7873a635328e2f23effc0c76e14a0d6f8435fd8c2`. It held `deploy-cloud-production`, verified the independent production/candidate locator paths and runbook invariants, resolved ephemeral Machine `847559f27d3d78` to the exact Artifact A digest, ran all five manifest-listed compatibility commands successfully, confirmed deletion of that exact Machine, and signed the evidence before upload.

#### Phase 0 R0 bridge evidence (2026-08-20)

- Protected OSS main workflow run `32373481690`, Actions artifact `9408193056`, and receipt SHA-256 `b9e2e3be43c6e8c5b33b67eb40b1c9d4663f58e783f4dc4c798346a9ca6ce985` close the R0 proof gate. The signed receipt binds OSS authority `bb3dda964e14bd38653a608616682064b25c7748`, immutable v0.0.17 source `a794be9f2697e0864d3a31da8f087577e9748f7e`, the sole allowlisted patch `dc62d8ee0cde5d19d152c71b7cafe8d98070fee2372b14fa2546186a447f2cd4`, and Core window `[28,29]`.
- Two independent no-cache builds produced the same linux/amd64 manifest `sha256:74168fcd93e70b5eb896a54dbd595648fb3a865a41a3224c30d4037196ea23d0`; the exact bytes were published as GHCR index `sha256:12d1a1bf3cded708c2892c0f69f1a1d9be235af9613141c5bd4c6680f8c8d129` and mirrored to `registry.fly.io/nerve-runtime:r0-dc62d8ee0cde` with the same platform digest. The protected job signed both the receipt and exact GHCR image and recorded the receipt in Rekor under the main-workflow identity.
- The immutable published SDK 0.2.0 wheel (`sha256:9f0a7d6316bf47eef64236f96d1a7a151b5517641930422b1b16711da8b02540`) produced the same Core 28 transcript for v0.0.17 and R0 (`sha256:dca908101b346ddf24db0bc114fc3a02056ce2c0e62b443ec0909e38c15542da`). The actual R0 image then passed startup, enqueue, claim, fake-provider delivery, inspection, resources, and legacy errors on Core 29 while immutable v0.0.17 was rejected. Independent artifact download re-ran the fail-closed receipt verifier and reproduced its SHA.

**Phase gate:** Do not author or apply Cloud 0009 until every proof above is green.

---

## Phase 1: Cloud 0009, OAuth Foundation, and the 8-to-9 Transition

### Overview

Author the complete Cloud-only durable model, implement OAuth behind issuance-off gates, and cross schema 8 to 9 with a dedicated predecessor-first workflow. Artifact A is only the migration-boundary rollback floor; the final-code rollback artifact is built later.

### Changes Required

#### 1.1 Preflight and add Cloud 0009

**Files**

- new `internal/store/migrations/cloud/0009_m2m_oauth_and_onboarding.sql`
- new `internal/store/oauth_machine_clients.go`
- new `internal/store/internal_delegation_nonces.go`
- new `internal/store/agent_onboardings.go`
- new `internal/store/managed_mailbox_aliases.go`
- new `internal/store/domain_ownership_claims.go`
- new `internal/store/agent_billing_workflows.go`
- new `internal/store/agent_outbound_evidence.go`
- new `internal/store/provider_domain_quarantine.go`
- new `scripts/deploy/canonicalize_provider_domains.go`
- new `scripts/deploy/preflight_provider_domains.py`
- new `schemas/provider-domain-preflight-receipt.schema.json`
- new `schemas/provider-domain-adoption-receipt.schema.json`
- new `schemas/provider-domain-deletion-receipt.schema.json`
- new `scripts/release/generate_provider_domain_adoption_receipt.py`
- new `scripts/release/generate_provider_domain_deletion_receipt.py`
- new `scripts/ci/verify_provider_domain_adoption_receipt.py`
- new `scripts/ci/verify_provider_domain_deletion_receipt.py`
- new `scripts/ci/test_provider_domain_adoption_receipt.sh`
- new `scripts/ci/test_provider_domain_deletion_receipt.sh`
- `scripts/ci/verify_cloud_deploy_order.sh`
- `.github/workflows/ci.yml`
- `internal/domains/canonical.go`
- `internal/store/org_domains.go`
- `internal/cloudapi/handler_domains.go`
- `internal/reconcile/service.go`
- `internal/store/migration_test.go`
- `internal/startup/migrations.go`
- `internal/startup/migrations_test.go`

**Changes**

- Add every table, trigger, constraint, lease/version field, index, and refusal-style down guard specified in the durable model.
- Add a read-only SQL preflight that canonicalizes existing domain rows and fails on ambiguous duplicate non-expired pending/provider-owned claims before migration.
- Add the operational Resend-inventory preflight/quarantine described above. Hold the domain-writer fence continuously from provider snapshot through migration/backfill/writer enable; an unknown or mismatched provider-only domain must be deleted or explicitly adopted with an audited receipt.
- Build one bounded batch canonicalizer from `internal/domains` and require the Python inventory preflight to send every local, provider, and resolution domain through it. Python's built-in IDNA codec is not an identity authority: U-label/A-label, case, trailing-dot, and transitional-character behavior must be byte-identical to the revision-17 Go Lookup/UTS-46 profile, including `straße.de` → `xn--strae-oqa.de`.
- Backfill one ownership claim per existing live domain and enable writers only after the backfill passes.
- Use bounded error/status fields; never persist raw provider bodies or secrets.
- Store APIs for assertions, generation allocation, lifecycle transitions, billing workflows, claim mutation, alias activation, and evidence projection require an explicit transaction.
- Artifact A includes schema-aware legacy domain create/verify/delete/expiry behavior before 0009 is applied. On Cloud 8, those paths take the same canonical-domain advisory lock but preserve the existing Core-row behavior without querying a 0009 object. On Cloud 9, every legacy write must create/lock/update the backfilled claim; no request can create a live Core domain without exactly one claim. Generic expiry is routed into releasing/provider-cleanup rather than direct deletion.
- Compile Artifact A for Core `[28,28]` and Cloud `[8,9]`. On schema 8, issuance remains disabled and no path may query a 0009 object.

#### 1.2 Implement operator-only client lifecycle and common locks

**Files**

- new `cmd/nerve-oauth-clients/main.go`
- new `internal/oauth/registry.go`
- `internal/store/store_tokens.go`
- `deploy/cloud/Dockerfile.control-plane`
- `docs/SECURITY.md`
- `docs/REPO_SPLIT_RUNBOOK.md`

**Changes**

- Commands are register, show, list, add-key, retire-key, revoke-key, set-billing-profile, disable-billing-profile, show-issuance, enable-issuance, disable-issuance, and revoke-client. Key commands accept public JWKs only, derive the RFC 7638 thumbprint, require `kid` equality, enforce permanent global uniqueness, validate key type/algorithm/use/window, and audit every mutation.
- Billing-profile commands accept only Stripe customer/payment-method/mandate references plus an exact plan/spend ceiling, verify the mandate with Stripe before committing, and never accept or print card data. Enable/disable issuance commands are callable only by the protected release/rollback workflows with signed matching evidence; an ordinary operator CLI invocation fails closed.
- Registration defaults irreversibly to `client_class=external` and `activation=pending`. Assigning the reserved `operator` or `synthetic` class requires the protected bootstrap/release context and an exact configured identity; no later command may reclassify a client. External activation is not a general CLI operation and occurs only through Phase 10's protected cohort workflow.
- Centralize lifecycle transactions and the client → org → locked-row order used by issuance, close, targeted generation revoke, key revoke, and client revoke.
- `revoke-client` atomically marks the client revoked, suspends outbound, moves any live generation to deprovisioning, and revokes actor-scoped tokens. It never creates an orphaned live generation.
- Do not expose registration/key mutation through public MCP or unauthenticated REST.

#### 1.3 Implement exact OAuth metadata, JWKS, and token issuance

**Files**

- new `internal/oauth/assertion.go`
- new `internal/oauth/issuer.go`
- new `internal/oauth/metadata.go`
- new `internal/cloudapi/handler_oauth.go`
- `internal/cloudapi/handler.go`
- `internal/config/config.go`
- `internal/config/config_test.go`
- `cmd/nerve-control-plane/main.go`
- `deploy/cloud/env.example`
- `deploy/cloud/fly.control-plane.toml`

**Changes**

- Consume the byte-identical shared verifier/JWKS foundation imported from the Phase 0.6 OSS authority tranche; Phase 1 does not author or modify a Cloud-first shared-auth copy.
- Implement the exact discovery, client-credentials-only metadata fixture, bounded form request, versioned error extension, response/caching, assertion-validation, thumbprint/JTI retention, scope-selected token, and issuance-control rules above.
- Keep asymmetric M2M and symmetric legacy verification in isolated branches with no algorithm fallback.
- Load distinct issuer-owned PS256 current/next signing keys and `kid` values from protected control-plane configuration, validate public/private consistency and unique IDs at startup, and expose only issuer public keys from AS JWKS. Client assertion keys remain registry data and cannot influence JWT output signing.
- Record org-token JTI and exact actor/generation/scopes in existing `service_tokens`; runtime validates claim-to-row equality.
- Generate claims/signatures before the transaction, then recheck all live state under common locks immediately before insert and return token bytes only after commit.
- Add a global fail-closed issuance kill switch plus explicit per-client activation. Neither changes `runtime.lock`.

#### 1.4 Establish the public auth origin

**Operational configuration**

- Add `auth.nerve.email` to the control-plane Fly app, configure DNS/TLS, and pin exact issuer, token endpoint, JWKS URL, and runtime resource.
- Do not route through dashboard Supabase middleware. Direct Fly hostnames cannot be accepted as issuer, audience, or resource aliases.

#### 1.5 Bind issuance-off evidence before the schema transition

**Files**

- new `.github/workflows/mcp2026-issuance-off.yml`
- new `.github/workflows/mcp2026-build-transition-bundle.yml`
- `scripts/ci/verify_cloud_deploy_order.sh`

**Changes**

- Produce protected, signed production evidence that Artifact A reports OAuth issuance disabled before the Cloud 0009 transition, then bind that evidence and the exact Artifact A identity into the attested transition bundle.
- Run the read-only issuance probe in an ephemeral, digest-pinned Fly Machine under the shared production deploy lock. Boundedly await Fly's documented `creating` transition, then treat persistent `created` as an unstarted version: verify its resolved digest, launch it with a bounded `machine update` pinned to that exact digest, use ordinary start only for `stopped|suspended`, and repeatedly prove the digest while documented `starting|restarting|updating|replacing` transitions converge. Reject every other state, digest drift, multiplicity, or timeout before exec.
- Arm exact-name cleanup before creation. Cleanup retries discovery and deletion across Fly's eventual-consistency window, including a create request that returns an error after server acceptance, and fails closed unless a final exact-name listing proves that no probe Machine remains.
- Execute the status CLI from the image's `/app` working directory because `fly machine exec` otherwise starts in `/`; this preserves the same bundled-migration resolution used by production without authoring or applying a migration.
- Every one-shot Cloud CLI Machine that calls common startup compatibility (`nerve-oauth-clients`, `nerve-flags`, and later equivalents) sets `NM_CLOUD_MODE=true` and `NM_MIGRATE_ON_START=verify` explicitly. Fly app secrets are inherited by ad-hoc Machines but app-level non-secret environment is not; relying on implicit defaults can turn a read-only probe or fence command into a migration writer.
- Closed incident note (2026-08-12): issuance-off run `31590944176` applied Cloud 0009 before the dedicated transition because its first revision omitted explicit Cloud/read-only migration environment. The temporary authenticated recovery path was used only to converge forward. Production transition run `31606012064` then completed with exact Artifact A, zero provider findings, public OAuth metadata/JWKS green, issuance disabled, and writer enable restored. Its signed receipt SHA-256 is `06ff49382fe1bb14fd1838c494590ce6b5dd8ebf4100f28a4d86c90478ebc63a`; the receipt, provider preflight, and transition bundle Sigstore identities were independently verified. The one-time marker, incident Machine identities, authorization helper, and executable recovery branch are removed immediately after this evidence. Historical signed `transition_mode=recovery` receipts remain schema-verifiable, but no workflow or generator can create another one; subsequent schema-9 retries accept only exact Artifact A and emit ordinary `resume` receipts.
- Recovery checkpoint note (2026-08-12): run `31594799522` proved the signed incident authorization, quiesced all predecessor writers, installed `domain_writes=false`, and converged every durable Machine to Artifact A, but stopped before provider inventory because `flyctl deploy --now` preserved the maintenance-stopped web state. The transition now explicitly starts only the already verified Artifact A web Machine versions, requires restored proxy autostart, pins the original web ID set through a bounded state wait, and repeats the same proof after the schema-9 redeploy. Unknown roles, digest/ID drift, `created` or undocumented states, missing web/reconciler Machines, or timeout fail while the writer fence remains installed.
- OAuth readiness checkpoint note (2026-08-12): runs `31600768850` and `31601167956` reached exact-A/schema-9 convergence and then failed the first public OAuth discovery assertion immediately after redeploy, while the same strict metadata, JWKS, TLS, and cache checks passed independently against the public endpoint. Keep the writer fence installed and make the complete strict smoke a bounded 60-second post-redeploy convergence gate with last-attempt diagnostics. No assertion is weakened and the fence is released only after one entire attempt passes.

#### 1.6 Bind protected provider-quarantine resolution

**Files**

- new `.github/workflows/provider-domain-quarantine-adopt.yml`
- new `.github/workflows/provider-domain-quarantine-delete.yml`
- new `schemas/provider-domain-adoption-receipt.schema.json`
- new `schemas/provider-domain-deletion-receipt.schema.json`
- new `scripts/release/generate_provider_domain_adoption_receipt.py`
- new `scripts/release/generate_provider_domain_deletion_receipt.py`
- new `scripts/ci/verify_provider_domain_adoption_receipt.py`
- new `scripts/ci/verify_provider_domain_deletion_receipt.py`
- new `scripts/ci/test_provider_domain_adoption_receipt.sh`
- new `scripts/ci/test_provider_domain_deletion_receipt.sh`

**Changes**

- The offline schema/generator/verifier binds the exact open-quarantine snapshot and bytes, an unbound local domain/claim target snapshot and workflow version, a fresh (at most five-minute-old) exact-ID/canonical provider observation, the exact canonicalizer binary SHA, protected approver, bounded reason, source revision, workflow run/attempt, and `domain-writes-global+canonical-domain` lock scope. Missing or substituted protected receipt/canonicalizer SHA, run/source/approver, stale target bytes, rewritten provider IDs, canonical mismatch, resolved quarantine, already-bound target, and stale/future observations fail closed.
- The protected workflow is the only adoption producer. It derives all three snapshots itself while holding the global writer fence plus canonical-domain lock, verifies the signed receipt by its protected run and exact SHA, and atomically rechecks the same quarantine/target versions before binding the exact provider ID and resolving the ledger. It never accepts caller-selected snapshot files, raw provider identity, canonical name, target org/domain, workflow version, or receipt SHA as unverified mutation authority.
- A receipt alone never makes the inventory safe. After adoption commits, the workflow captures a fresh complete provider/local inventory, proves that the adopted exact ID is now the local canonical pair and that no finding remains, then signs the converged preflight receipt before writer enable. A provider-only object with no valid target, any uncertainty, or any changed snapshot remains quarantined.
- Landing the offline receipt contract does not construct or authorize the mutation workflow. Production adoption remains absent until explicit activation approval and protected-environment review.
- The deletion receipt separately binds the exact open quarantine, exact canonicalizer binary, a fenced local-reference snapshot proving zero local provider-ID references, zero active-inbox dependents, and zero provider-owned claims, plus one exact-ID deletion/readback observation. DELETE acknowledgement is non-authoritative: only a fresh same-ID GET 404 after the recorded delete attempt proves absence. Snapshot/hash/identity substitution, nonzero local references, wrong ID/canonical name, readback other than 404, reordered timestamps, and observations older than five minutes fail closed.
- The protected deletion producer derives and rechecks the local-reference snapshot under the same writer/canonical lock before provider mutation, performs DELETE and final exact-ID GET outside database transactions, and resolves the ledger only after the signed receipt and unchanged quarantine snapshot verify. Provider uncertainty retains the open quarantine and writer fence. Landing this offline contract does not construct or authorize provider deletion.

### Dedicated Compatibility Transition

Run `.github/workflows/cloud-0009-transition.yml` under the shared production deploy lock as one resumable state machine:

1. Accept only `transition_bundle_run_id` plus `transition_bundle_sha`, verify its attestation/target/workflow identity, and derive Artifact A from it: Core `[28,28]`, Cloud `[8,9]`, head 0009, issuance off. Raw A image/manifest inputs and final release-set inputs are rejected.
2. Rehearse a production snapshot at exact Cloud `current=0008`, `head=0009`, `pending=[0009]` and Core `current=head=0028`, `pending=[]`; run SQL claim preflight and a recorded provider-inventory fixture containing local, provider-only, and mismatched-ID cases.
3. In production, take the global domain-writer fence, run both the SQL preflight and full Resend inventory comparison through the exact Go Lookup/UTS-46 batch canonicalizer, and abort/quarantine on any unresolved provider-only or mismatched object. Keep the fence through step 7 so inventory and writer enable are one transition.
4. Deploy A web on schema 8, explicitly start every exact A web Machine after the maintenance stop, and prove the pinned web ID set, restored proxy autostart, started state, digest, and window; execute read-only compatibility for `nerve-flags`, `nerve-drill`, and `nerve-oauth-clients` from the same image and prove none queries 0009 or mutates.
5. Converge the scheduled reconciler to A, execute only its read-only compatibility command on schema 8, and prove digest/window.
6. Apply 0009 with `/app/nerve-migrate` from the same A image. Cloud pre-state is exactly 0008/head0009/[0009]; post-state is exactly 0009/head0009/[]. Core remains 0028/head0028/[].
7. Redeploy/prove A web and reconciler on schema 9, repeat the exact bounded web-start/ID/digest/autostart proof, and run read-only compatibility for all six manifest-listed database-mutating binaries.
8. Smoke public metadata/JWKS with issuance off, verify the provider-preflight receipt, then release the domain-writer fence; no client registration or generation creation occurs.
9. Emit and independently verify the Phase 0 transition-receipt schema containing the transition-bundle digest, A digest/manifest and all shipped binary hashes/windows, pre/post schema evidence, provider inventory/quarantine resolution, Machine identities/digests, issuance-off proof, workflow identity, and timestamps.

Retry accepts only `schema8 + old/A` or `schema9 + A` states and converges forward. It rejects schema 9 with an unknown digest. Once 0009 is applied, `[8,8]` is forbidden and A is the temporary boundary floor.

### Success Criteria

#### Automated verification

- [x] 0009 migrates from a production-shaped 0008 snapshot and backfills unambiguous claims.
- [x] Preflight rejects duplicate live canonical-domain claims.
- [ ] Provider inventory uses the exact Go canonical identity for local/provider/resolution rows, rejects/quarantines provider-only and canonical/provider-ID mismatches, rejects IDNA2003/Lookup substitutions, and a two-connection test proves no domain mutation can enter between inventory and writer enable.
- [ ] Adoption/deletion receipt tests bind exact open-quarantine, canonicalizer, protected producer/approver, and snapshot hashes. Adoption additionally binds an unbound target/claim plus fresh exact-ID presence; deletion binds zero local references plus final same-ID GET 404. Substitutions fail, and protected workflows prove exact-CAS resolution plus a fresh zero-finding inventory before writer enable.
- [x] Down succeeds only with no durable rows and refuses for every protected table class.
- [x] Migration tests exercise alias/catch-all triggers, state/uniqueness constraints, and claim serialization.
- [ ] Artifact A on schema 9 exercises legacy domain create, verify, delete, and expiry and proves every live Core domain has exactly one canonical claim; Artifact A on schema 8 never queries the claim table.
- [x] Assertion tests reject wrong identity, audience/resource, key/algorithm/window, replay, and scope mixing.
- [x] Key tests reject reordered/optional-member aliases, another kid, another client, concurrent duplicate registration, and reuse after retirement/revocation; `kid` always equals the RFC 7638 thumbprint.
- [x] JTI boundary tests cover first use and replay at `exp-1`, `exp`, and `exp+29s`, exact `retain_until`, and cleanup only after the accepted skew window.
- [x] Golden token-form tests accept omitted `client_id`, accept a matching field, reject a mismatch/duplicate, and pin every OAuth HTTP status, error body/header, and cache directive.
- [x] Body/field/segment tests cover every numerical boundary, chunked and false Content-Length requests, duplicate fields, and rejection before decode/crypto/DB.
- [x] Metadata golden tests omit `response_types_supported`, reject empty/fabricated values, and pass the pinned client-credentials conformance consumers.
- [x] Decoded JWT fixtures pin PS256/current issuer `kid`, the complete common claim set, exact five/fifteen-minute lifetimes, onboarding absence of `org_id`, and org-token presence of the locked `org_id`.
- [x] JWKS rotation fixtures prove current/next publication, overlap/retirement after lifetime plus skew, no client assertion key exposure, and access-token alg/kid independence from RS256/PS256 client assertions.
- [x] Token endpoint cannot accept caller-selected org/generation or scopes outside registration/state.
- [x] Missing/off/wrong-release issuance control rejects before client lookup; concurrent enable/exchange and disable/exchange tests have one global-lock linearization point.
- [x] Two-connection barrier tests split token authority precisely: close versus email-token issuance leaves no usable email token; close versus onboarding-token issuance may leave only a short-lived token bound to that same generation with status/idempotent-close access; `revoke-client` versus either issuance leaves neither token usable, in both commit orders.
- [x] Client revocation drives pending/active generation toward closed.
- [x] Client-class tests reject reclassification, unprotected synthetic/operator assignment, duplicate synthetic identity, and every activation path outside the protected cohort workflow.
- [ ] Artifact A passes compatibility for all six manifest-listed database-mutating binaries on both schema 8 and 9 before any mutation-specific behavior.
- [x] Normal deploy cannot perform the transition and transition retry rejects unknown states.
- [x] Web-start convergence tests cover stopped/suspended recovery, already-started idempotency, missing web, digest/ID drift, disabled autostart, unsafe `created` state, and timeout; the transition invokes this proof before provider inventory and after schema-9 redeploy.

#### Operator verification

- [x] `auth.nerve.email` metadata, JWKS, TLS, caching, and canonical host behavior work publicly with issuance disabled.
- [x] Every web and reconciler Machine is A before migration and after migration.
- [x] The signed transition receipt is stored with the release evidence.

**Phase gate:** Pause on schema 9 with Artifact A proven and issuance off. Record A as the temporary boundary floor; do not activate a client.

#### Phase 1 token-boundary completion evidence (2026-08-22, local WIP)

- Endpoint-level assertion tests inject the actual acceptance clock at `exp-1`, `exp`, and `exp+29s`, prove first-use acceptance, replay rejection, exact `retain_until=exp+30s`, and deletion only after that retained window.
- A strictly validated optional previous PS256 public JWK remains published with current/next until the fleet-wide stopped-signing timestamp plus the fifteen-minute access-token lifetime and thirty-second skew. Retirement changes the JWKS ETag; partial configuration, unsafe material, thumbprint mismatch, and duplicate keys fail startup.
- Real-PostgreSQL two-connection fixtures cover enable and disable against token exchange with both exchange-first and control-first commit orders. These local bytes pass the affected package tests; remote merge/release evidence remains pending.

---

## Phase 2: OSS-First Dual MCP Runtime and Exact Wire Contract

### Overview

Upgrade nerve-oss to Go 1.25, isolate the frozen 2025 adapter from the 2026 Go SDK adapter, and prove Origin, auth, errors, JSON/SSE, and resource contracts before enabling onboarding mutations.

### Changes Required

#### 2.1 Upgrade the toolchain and pin protocol dependencies

**OSS files**

- `go.mod`, `go.sum`
- `.github/workflows/ci.yml`
- `.github/workflows/security.yml`
- `.github/workflows/sync-to-cloud.yml`
- `deploy/docker/cortex/Dockerfile`

**Changes**

- Set Go 1.25 consistently and pin `github.com/modelcontextprotocol/go-sdk` v1.7.0.
- Build, vet, test, run targeted race suites, vulnerability scan, and build the production image on that toolchain.

#### 2.2 Add the shared Origin/auth wrapper and version router

**OSS-first/shared files**

- new `internal/mcp/router.go`
- new `internal/mcp/origin.go`
- `internal/auth/verifier.go`
- new `internal/auth/jwks.go`
- `internal/auth/context.go`
- `internal/config/config.go`
- `internal/app/app.go`
- `docs/SECURITY.md`

**Changes**

- Implement the exact split Origin precheck/auth/post-auth principal-kind chain from the implementation approach.
- Authenticate once, distinguish `m2m_onboarding`, `m2m_org`, legacy HS256 bearer, Cloud key, and the explicitly supported legacy bootstrap credential, then put a typed principal in context.
- Keep M2M PS256 access-token verification and registered-client RS/PS assertion verification isolated from HS256. Origin never infers principal kind from `alg`. Unknown access-token kid forces one bounded refresh then fails; stale cache never authorizes an unknown key; planned current/next rotation overlaps.
- Route by exact protocol header, compare trusted routing metadata to parsed body metadata, and reject mismatch before execution.

#### 2.3 Split adapters and common Invoker

**OSS files**

- `internal/mcp/server.go`
- `internal/mcp/types.go`
- `internal/mcp/stdio.go`
- new `internal/mcp/legacy.go`
- new `internal/mcp/sdk_server.go`
- new `internal/mcp/invoker.go`
- new `internal/mcp/catalog.go`
- new `internal/mcp/errors.go`

**Changes**

- Preserve legacy initialize, session, raw result, resource, and `-32040...-32043` behavior byte-for-byte for SDK 0.2.0.
- Configure modern SDK with `Stateless=true`; implement deterministic discovery/list ordering, private cache metadata, complete JSON Schema 2020-12 success/error outputs, and conformant resources/list/read.
- Translate common typed failures separately: HTTP auth, modern tool error, or legacy JSON-RPC code only at the adapter boundary.
- Bind parsed method/name, principal, generation, profile, and body; never authorize from `Mcp-Name` alone.
- Put scope, policy, idempotency, entitlement, audit, rate reservation, execution, and finalize behavior in the common Invoker.

#### 2.4 Implement Streamable HTTP and capability negotiation

**Files**

- `internal/mcp/sdk_server.go`
- `internal/mcp/router.go`
- new `internal/mcp/streamable_contract_test.go`
- `.github/workflows/cloud-e2e-smoke.yml`

**Changes**

- Pin/advertise the OAuth Client Credentials extension and reject required-but-absent per-request capability before tool execution.
- Require the exact two mandatory per-request MCP 2026 metadata keys—protocol version and client capabilities—on every modern request; compare body/header version and body/header method/name before dispatch. Accept omitted `clientInfo`; validate its SDK-defined bounded shape if present, and pin the SDK 0.3 value only in SDK-specific fixtures.
- Test Go handler with `JSONResponse=true` and `false`; both must satisfy one exact contract.
- Cover JSON, multiline SSE, comments, related notifications, cancellation, malformed/truncated stream, wrong ID, duplicate final response, and EOF without final response.
- Update scheduled smoke with an explicit 2025 legacy leg and a separately authenticated 2026 M2M leg; never rely on a missing protocol header.

#### 2.5 Preserve bounded memory behavior

**OSS files**

- `internal/mcp/router.go`
- `internal/memguard`
- `internal/mcp/server_body_test.go`
- `internal/config/config.go`

**Changes**

- Keep the 16 MB wire cap and reserve for two wire copies plus maximum decoded attachment allocation before dispatch.
- Chunked requests reserve worst case before reading. Return 413 for wire size and 503 plus Retry-After for shared-memory exhaustion.
- Release reservations on parse error, cancellation, panic, and every failure.
- Keep one active runtime Machine while legacy sessions exist unless shared session routing is separately approved.

#### 2.6 Synchronize authority and provenance

**OSS files**

- `docs/MCP_Contract.md`
- `docs/MCP_TRANSPORT.md`
- `sync-manifest.yaml`
- `configs/policy/autonomous-outbound-v1.yaml`
- `scripts/release/generate_runtime_manifest.sh`
- `internal/release/runtime_metadata.go`
- `internal/release/manifest_script_test.go`

**Changes**

- Document both profiles, error partition, Origin matrix, scope-selected tokens, extension capability, and JSON/SSE responses.
- Keep `internal/mcp` runtime-only; mirror shared auth, policy, store, and contract bytes OSS-first.
- Add policy and dual-contract hashes to runtime manifest and OCI labels. Core stays 0028/[28,28].

### Success Criteria

#### Automated verification

- [x] Immutable SDK 0.2.0 passes all golden legacy tool/resource/error fixtures byte-for-byte.
- [x] Modern requests are stateless across handler instances and pass pinned conformance.
- [x] JSONResponse true/false pass JSON/SSE final-response and failure fixtures.
- [x] A translation table proves no modern response emits `-32040...-32043`.
- [x] Route matrix covers both versions, all credentials, and absent/allowed/hostile/null/lookalike Origin; hostile Origin never reaches a handler.
- [x] Valid native auth with absent Origin succeeds; allowed Origin without auth returns 401; hostile Origin with valid auth returns 403.
- [x] Phase 2 proves typed `m2m_onboarding` and `m2m_org` principals route independently, but lifecycle dispatch remains absent until Phase 3 registration; calling or listing an unregistered lifecycle tool cannot succeed.
- [x] Missing extension capability fails before dispatch.
- [x] Missing/malformed `protocolVersion` or `clientCapabilities`, malformed-present `clientInfo`, absent OAuth extension for M2M, and every header/body version or method/name mismatch fail with the exact modern code before dispatch; omitted `clientInfo` succeeds for a conformant external agent.
- [x] HS256/RS256 confusion and unknown-kid/stale-cache cases fail closed.
- [x] Memory and 413/503 tests are deterministic and leak-free.
- [x] Runtime/policy/contract hashes match exact-mirror sources.

#### Manual verification

- [x] Explicit 2025 returns the frozen shape; explicit 2026 returns modern JSON or SSE.
- [x] SDK 0.2.0 and a native M2M client use the same `/mcp` URL concurrently.
- [x] Modern `tools/list` changes with principal/state while cache scope remains private.

#### Phase 2.4 dual-profile endpoint evidence (2026-08-20)

- OSS authority PR `dsmolchanov/nerve-oss#51` merged as `4458b1abe6d4fb66e73ab29b3fb14da67af97ccc`. Its build-tagged artifact test and `scripts/ci/test_mcp_dual_profile_artifact.sh` download the immutable published SDK 0.2.0 wheel, require SHA-256 `9f0a7d6316bf47eef64236f96d1a7a151b5517641930422b1b16711da8b02540`, and run it against the real hybrid router.
- The manual proof ran the immutable SDK and the native MCP 2026 Go client concurrently against one test-server `/mcp`. A rendezvous barrier held dispatch until both explicit protocol profiles arrived; the legacy leg authenticated through the real Cloud-key path, while the modern leg presented a valid PS256 bearer that passed the real M2M verifier, issuer/audience/claim validation, and durable service-token binding before `tools/list` returned a private non-empty catalog.
- The proof remains manual in Phase 2 as planned; it is not a PyPI-dependent required CI gate. Phase 8 owns promotion of exact SDK artifacts into the mandatory candidate contract matrix. Post-merge OSS-to-Cloud sync run `32379539516` reported `no shared paths changed`, as expected for OSS-only proof code.

#### Phase 2.4 explicit wire-shape evidence (2026-08-20)

- OSS authority PR `dsmolchanov/nerve-oss#52` merged as `f4974bcf0587ef5a5731d47271ae5f01bdf4f188`. `TestExplicitProtocolProfilesReturnFrozenLegacyAndModernWire` drives the real shared `NewRouter` with explicit protocol selection for both profiles.
- The 2025 leg requires HTTP 200, a nonempty `MCP-Session-Id`, and the exact frozen initialize bytes for protocol `2025-11-25`. Through the same router, the explicit 2026 leg exercises both `application/json` and `text/event-stream`, validates one complete final JSON-RPC response, requires private `cacheScope`, and rejects any legacy protocol marker in the modern response.
- Targeted, full-suite, vet, race, and diff checks passed; required GitHub CI and `codex-review-window` were green before merge. Post-merge OSS-to-Cloud sync run `32410709432` succeeded and reported `no shared paths changed`, as expected for the OSS-only contract proof. No runtime behavior, dependency, workflow, release tag, or deployment changed.

#### Phase 2.4 dynamic private-catalog evidence (2026-08-20)

- OSS authority PR `dsmolchanov/nerve-oss#53` merged as `022f0d2542216af24fffee63fc3c64274ca52b7b`. `TestModernToolsListChangesWithPrincipalAndPolicyStateAndStaysPrivate` reuses one real modern handler and issues successive `tools/list` requests as an onboarding principal, a read-only org principal, a compose-capable org in an allowed state, and the same compose-capable org after policy denial.
- The observed catalogs change from empty to `get_thread,list_threads`, then to `compose_email`, then back to empty. Every response independently requires `cacheScope=private` and `ttlMs=5000`, proving the list remains advisory and cannot become shared while principal or live policy state changes.
- Targeted, full-suite, vet, race, and diff checks passed; required GitHub CI and `codex-review-window` were green before merge. Post-merge OSS-to-Cloud sync run `32412378739` succeeded and reported `no shared paths changed`, as expected for the OSS-only proof. No runtime behavior, dependency, workflow, release tag, or deployment changed.

#### Phase 2 immutable SDK 0.2.0 golden evidence (2026-08-20)

- OSS authority PR `dsmolchanov/nerve-oss#54` merged as `1a6c29cb600d88c69f472e1fafaa7e7b2fb67175`. It keeps the immutable `nerve-email==0.2.0` wheel pinned to SHA-256 `9f0a7d6316bf47eef64236f96d1a7a151b5517641930422b1b16711da8b02540` and reuses one set of exact newline-terminated golden bytes between the real legacy handler tests and the artifact-level SDK consumer proof.
- The wheel's real `NerveClient` consumes the frozen initialize, `tools/list`, and `resources/list` responses and requires the exact ordered catalog and resource values. Separate fresh sessions consume all four frozen business-error fixtures: exact `NerveQuotaError/-32040`, `NerveSubscriptionError/-32041`, `NerveRateLimitError/-32042` with SDK and wire retry `12`, and exact generic `NerveError/-32043` with raw wire retry `3`. The last distinction is intentional and honest: immutable SDK 0.2.0 has no dedicated idempotency exception and does not expose that retry field on its generic exception, while the frozen wire metadata remains verified byte-for-byte.
- Artifact-tagged SDK execution, legacy wire goldens, full-suite, vet, race, and diff checks passed. Codex's first review caught the imprecise idempotency assertion; fix commit `bd6bf84c066d519a295328e452e3c6526fff967d` requires the exact generic type and independently inspects raw retry metadata. All required gates were green before merge. Post-merge OSS-to-Cloud sync run `32414830385` succeeded and reported `no shared paths changed`; no runtime behavior, dependency, workflow, release tag, or deployment changed.

#### Phase 2 cross-instance stateless and pinned conformance evidence (2026-08-20)

- OSS authority PR `dsmolchanov/nerve-oss#55` merged as `f37149df623163858965088c29cf0e211d45642d`. A single modern SDK client sends its `server/discover` probe and subsequent `tools/list` request through a round-robin boundary to two separately constructed `NewSDKHandler` and runtime instances; each instance receives exactly one request, so the second request cannot depend on process-local bootstrap or session state.
- The same tagged test then runs the official server consumer from pinned conformance commit `81eb1c3edaed87d7fd585d7b80186da7a2960660` twice at protocol `2026-07-28`, once against each instance, and requires every `tools-list` and wire-schema check to pass without an expected-failure baseline. The gate imports the pinned runner module directly because that snapshot's aggregate CLI imports a Node-22-only tier-check helper even though repository CI intentionally runs Node 20; this keeps the official scenario/validation implementation while avoiding an unrelated CLI import failure.
- Full pinned conformance/ext-auth, Go full-suite, vet, MCP race, shell-path, and diff checks passed locally and in required GitHub CI. Two reviews found the same relative override-path class; fix `07e5c17bd395230b5962ddd3b65c6297840e0350` canonicalizes both checkout overrides, adds relative/absolute regression coverage, and records the permanent repository invariant. The final `codex-review-window` passed before auto-merge. Post-merge OSS-to-Cloud sync run `32420125208` succeeded and reported `no shared paths changed`; no runtime behavior, dependency, release tag, or deployment changed.

#### Phase 2 JSON/SSE final-response and failure evidence (2026-08-20)

- OSS authority PR `dsmolchanov/nerve-oss#56` merged as `41bdc0405e08cdace8926d0121b53c4eedfb0d03`. `TestModernContractJSONResponseModesEmitOneFinalResponse` drives the real modern handler with `JSONResponse=true` and `false`; each mode must emit its expected base media type and exactly one matching final JSON-RPC response for both a successful `tools/list` and a schema-rejected `compose_email` tool result with `isError=true`.
- `TestModernJSONAndSSEContractFixtures` provides the parser-side matrix for both media types. It accepts result and JSON-RPC error finals, including multiline SSE with comments and an unrelated notification, and rejects malformed or truncated JSON, wrong IDs, missing result/error members, duplicate finals, malformed SSE fields, EOF without a final response, and missing or unsupported content types.
- Targeted and full-suite Go tests, vet, MCP race, and diff checks passed locally; required GitHub CI and `codex-review-window` were green before auto-merge. Post-merge OSS-to-Cloud sync run `32421456346` succeeded and reported `no shared paths changed`, as expected for OSS-only contract tests. No runtime behavior, dependency, workflow, release tag, or deployment changed.

#### Phase 2 modern error-partition evidence (2026-08-21)

- OSS authority PR `dsmolchanov/nerve-oss#57` merged as `5befd381c764deb0b416617b923cc677242bce83`. The explicit translation table maps quota, inactive subscription, rate limit, idempotency-in-progress, and unexpected failures to modern string codes and exact retryability metadata; none can reuse the frozen legacy JSON-RPC range `-32040...-32043`.
- The real modern SDK handler is exercised for quota, subscription, rate, and idempotency failures in both `JSONResponse=true` and `false` modes. A recording transport captures the actual request ID, base media type, and raw JSON or SSE response bytes; each raw response must contain exactly one matching final JSON-RPC response, a `CallToolResult` with `isError=true`, the expected structured error and retry metadata, and no legacy code anywhere on the emitted wire.
- Codex review correctly rejected the first proof's re-marshalling of the SDK-decoded result as insufficient raw-wire evidence. Fix `8a8294f7c0fdf02505362f31516bc50a405bc415` added the recording transport and two-mode wire matrix; targeted and full-suite tests, vet, MCP race, required GitHub CI, and the final `codex-review-window` passed. Post-merge OSS-to-Cloud sync run `32423156914` succeeded and reported `no shared paths changed`; no runtime behavior, dependency, workflow, release tag, or deployment changed.

#### Phase 2 protocol, credential, and Origin route-matrix evidence (2026-08-21)

- OSS authority PR `dsmolchanov/nerve-oss#58` merged as `98651082e15e16285748c949e9a8d0b5be7b4477`. `TestRouterProtocolCredentialOriginMatrix` executes the full cross-product of legacy `2025-11-25` and modern `2026-07-28`; the five typed principals produced by the supported bootstrap, Cloud API key, legacy JWT, M2M onboarding, and M2M organization credential paths plus missing authentication; and absent, allowed, hostile-scheme, `null`, suffix-lookalike, and prefix-lookalike Origin states.
- Every valid native principal with absent or allowed Origin authenticates exactly once, retains the same typed principal in context, receives the exact routed protocol marker, and reaches only the selected adapter. Missing authentication with absent or allowed Origin returns 401 without adapter dispatch. Every hostile, `null`, or lookalike Origin returns 403 before authentication and before either adapter, including cases whose configured authenticator would otherwise return a valid principal.
- Targeted and full-suite tests, vet, MCP race, diff checks, required GitHub CI, and `codex-review-window` passed before auto-merge. Post-merge OSS-to-Cloud sync run `32423671705` succeeded and reported `no shared paths changed`; no runtime behavior, dependency, workflow, release tag, or deployment changed.

#### Phase 2 typed-principal lifecycle-absence evidence (2026-08-21)

- OSS authority PR `dsmolchanov/nerve-oss#59` merged as `6582721bce82c1c6bc6fb88ab68eb679ef99da3c`. `TestSDKServerPhase2TypedM2MProfilesCannotListOrCallLifecycleTools` drives separately typed `m2m_onboarding` and `m2m_org` principals through the real modern handler. The onboarding profile lists zero tools while the organization profile retains the existing eight-tool catalog, proving that the principal kinds select independent profiles before Phase 3.
- Neither profile can list any of `nerve_onboarding_start`, `nerve_onboarding_status`, `nerve_onboarding_verify_domain`, or `nerve_onboarding_close`. Direct `tools/call` attempts for all four names under both principals must return HTTP 400 with exact JSON-RPC `-32602` and `unknown tool`; no lifecycle callback is registered or reachable.
- Targeted and full-suite tests, vet, MCP race, diff checks, required GitHub CI, and `codex-review-window` passed before auto-merge. Post-merge OSS-to-Cloud sync run `32424308026` succeeded and reported `no shared paths changed`; no runtime behavior, dependency, workflow, release tag, or deployment changed.

#### Phase 2 missing-extension pre-dispatch evidence (2026-08-21)

- OSS authority PR `dsmolchanov/nerve-oss#60` merged as `1c5a6ff81638400559a9ce41d71f7943aefc0716`. `TestModernContractMissingOAuthExtensionFailsBeforeToolDispatch` sends a schema-valid modern `list_threads` call as a typed `m2m_org` principal with the required read scope but without the OAuth client-credentials extension.
- The real modern handler must return HTTP 400 with exact SDK `MissingRequiredClientCapabilities` (`-32021`) and the stable `missing required client capabilities` message. A deliberately failing entitlement gate remains at zero calls, proving capability negotiation rejected the request before authorization or tool dispatch, and the shared memory budget returns to zero.
- Full-suite tests, vet, targeted MCP race, diff checks, required GitHub CI, and `codex-review-window` passed before auto-merge. Post-merge OSS-to-Cloud sync run `32424934000` succeeded and reported `no shared paths changed`; no runtime behavior, dependency, workflow, release tag, or deployment changed.

#### Phase 2 modern metadata and header matrix evidence (2026-08-21)

- OSS authority PR `dsmolchanov/nerve-oss#61` merged as `956cc62686c87d3f96a14b0482867f97628cf4ca`. The 35-case `TestModernContractRejectsMetadataAndHeaderFailuresBeforeDispatch` matrix drives the real Cloud router and modern handler with a typed `m2m_org` principal and a schema-valid `list_threads` call whenever tool dispatch is applicable.
- The matrix covers missing, duplicate, and unsupported protocol headers; missing, null, non-string, empty, and header-mismatched protocol metadata; missing or non-object capabilities; absent or malformed OAuth extension/settings shapes; malformed and over-bound `clientInfo`; and missing, duplicate, malformed, or mismatched method/name headers and body fields. Every failure requires the applicable exact SDK `-32020`, `-32021`, or `-32022` code and exact structured version/capability data, zero entitlement dispatch, and a fully released memory budget.
- The companion tests retain the conformant omitted-`clientInfo` success path, prove the extension is M2M-only, and cover the inverse legacy initialize header/body version mismatch before session creation. Full-suite tests, vet, targeted MCP race, diff checks, required GitHub CI, and `codex-review-window` passed before auto-merge. Post-merge OSS-to-Cloud sync run `32425806766` proved base `1c5a6ff81638400559a9ce41d71f7943aefc0716` to head `956cc62686c87d3f96a14b0482867f97628cf4ca` and reported `no shared paths changed`; no runtime behavior, dependency, workflow, release tag, or deployment changed.

#### Phase 2 authentication algorithm isolation evidence (2026-08-21)

- OSS authority PR `dsmolchanov/nerve-oss#62` merged as `2d284544b451fc0e7d4d8aa976f4cedf586dd1b2`. `TestAuthenticateRequestM2MRejectsAlgorithmAndClaimConfusion` proves issuer access tokens accept only PS256, reject RS256 and unknown `kid`, and fail closed on principal/claim confusion. `TestRemoteJWKSRefreshAndBoundedStaleUse` proves an unknown `kid` receives one bounded refresh and is never authorized from stale cache, while a known key is usable only inside the explicit stale bound.
- The same authority change closes the downgrade path in the legacy verifier: `TestAuthenticateRequestRejectsHS256M2MClaimDowngrade` rejects M2M `token_use`/`token_kind`, `client_id`, and `generation` markers on an otherwise valid HS256 token while retaining the documented legacy HS256 `token_use=service` compatibility path. Full-suite tests, vet, targeted auth race, required GitHub CI, and `codex-review-window` passed before auto-merge.
- Cloud sync PR `dsmolchanov/nerve-cloud#124` merged as `6ecd1c0fd9ff7035ea4bfe717722446e7937f104` with source lock `2d284544b451fc0e7d4d8aa976f4cedf586dd1b2`. Its non-required conformance job exposed that the Cloud-consumed script was not anchored to the OSS checkout and was absent from the exact-mirror manifest. OSS follow-ups `#63` (`46f85b1b9910d372afee216364f334bbc0f4efd4`) and `#64` (`5dcc69168792394c56bfdf25892594d6a3f16fd6`) fixed both boundaries. Successor Cloud sync PR `#125` merged as `1d8705acf73d6fdcd1a3b299e78b07b3058b3f21` with that exact OSS source lock; `exact-mirror`, `go-checks`, `mcp-conformance`, and the fail-closed review window all passed. No release tag or deployment changed.

#### Phase 2 bounded-memory failure evidence (2026-08-21)

- OSS authority PR `dsmolchanov/nerve-oss#65` merged as `666d681fc4255cd5b7d305eb1a63fdeadfe43102`. `TestSDKHandlerChunkedAndExceptionalReadsReleaseWorstCaseReservation` drives the real modern handler with unknown-length bodies and observes the full `maxModernRequestMemoryBytes` reservation before the first read.
- The matrix proves an over-limit chunked body returns HTTP 413 and that normal parse failure, `context.Canceled`, and a panicking body reader all return or unwind with `MemoryBudget.Used()==0`. The existing deterministic shared-budget matrix retains the complementary HTTP 503 plus `Retry-After` exhaustion path and successful-release/reacquisition proof.
- Full-suite tests, vet, the targeted MCP/memguard race suites, conformance, runtime-manifest generation, Docker policy-artifact verification, required GitHub CI, and `codex-review-window` passed before auto-merge. Post-merge OSS-to-Cloud sync run `32428834350` proved base `5dcc69168792394c56bfdf25892594d6a3f16fd6` to head `666d681fc4255cd5b7d305eb1a63fdeadfe43102` and reported `no shared paths changed`; no Cloud source lock, runtime behavior, release tag, or deployment changed.

#### Phase 2 exact-mirror runtime hash evidence (2026-08-21)

- OSS authority PR `dsmolchanov/nerve-oss#66` merged as `f51cc9682568a40ec966e6a527117dfa5d27a1e4`. `TestGenerateRuntimeManifestScript` now recomputes the MCP contract and autonomous outbound policy SHA-256 values directly from the repository bytes, compares them to the generated runtime manifest, and proves both source paths remain members of `sync-manifest.yaml`'s `exact-mirror` set.
- The first review correctly found that ambient source-path overrides could make the default-source proof nondeterministic. The merged head isolates all generator inputs, explicitly binds the default proof to repository contract/policy/Core paths, and adds `TestGenerateRuntimeManifestScriptHonorsSourceOverrides` to exercise contract and policy overrides together. A local regression invocation with all three ambient source paths set to nonexistent files passed, proving the tests neither ignore supported explicit overrides nor inherit unrelated process configuration.
- Full-suite tests, vet, targeted race suites, exact-mirror validation, conformance, runtime-manifest generation/export, Docker build, policy artifact verification, required GitHub CI, and the fresh-head fail-closed review window passed before auto-merge. Post-merge OSS-to-Cloud sync run `32429670477` proved base `666d681fc4255cd5b7d305eb1a63fdeadfe43102` to head `f51cc9682568a40ec966e6a527117dfa5d27a1e4` and reported `no shared paths changed`; the already-proven Cloud source lock remains `5dcc69168792394c56bfdf25892594d6a3f16fd6`, and no runtime artifact, release tag, or deployment changed.

**Phase gate:** Do not tag the runtime. Onboarding lifecycle, provider fencing, mailbox creation, and outbound enforcement must enter the same tested candidate.

---

## Phase 3: Delegated Onboarding and Atomic Organization Graph

### Overview

Connect the four generation-bound modern tools to Cloud 0009 through one replay-safe internal boundary and create the organization, entitlement, usage, and explicit policy graph atomically. External provider execution and cleanup fencing are completed in Phase 4.

### Changes Required

#### 3.1 Add the OSS onboarding interface and modern tools

**OSS files**

- new `internal/mcp/onboarding.go`
- `internal/mcp/catalog.go`
- `internal/mcp/sdk_server.go`
- `internal/app/app.go`
- `internal/config/config.go`
- new `internal/onboarding/client.go`

**Changes**

- Define `OnboardingProvisioner` methods Start, Status, VerifyDomain, and Close with protocol-neutral typed inputs/results, including the normal complete waiting states and structured business errors.
- Register the four tools only on the `m2m_onboarding` profile.
- Implement a fixed-base-URL HTTP client; reject redirects and caller-controlled destinations.
- Sign each delegation request with the dedicated internal key and include nonce/timestamp/body digest.
- Bound control-plane timeouts below the outer MCP deadline.
- Preserve idempotency across transport timeout; a timeout result tells the client to poll the same generation/key.
- Pass the original bearer and generation-bound principal; never accept client, generation, or org authority from tool input.
- After registration, assert the complete profile contract: `m2m_onboarding` lists exactly the four lifecycle tools, while every `m2m_org` profile lists none of them and direct hidden calls are denied.

#### 3.2 Add the internal control-plane boundary

**Cloud files**

- new `internal/cloudapi/handler_agent_onboarding.go`
- `internal/cloudapi/handler.go`
- new `internal/onboarding/service.go`
- new `internal/onboarding/state.go`
- new `internal/onboarding/delegation_auth.go`
- `internal/config/config.go`
- `cmd/nerve-control-plane/main.go`

**Changes**

- Register a non-public-contract internal path for the four operations.
- Require both a valid delegation signature and the original `m2m_onboarding` bearer.
- Configure current and next delegation HMAC keys with explicit key IDs so rotation can overlap; never fall back to the OAuth or bootstrap signing keys.
- Consume the signed nonce in internal_delegation_nonces before executing a mutation.
- Recheck client/key status and load only the lifecycle row named by the authenticated token generation for every call; never replace it with the client's current/latest row.
- Resolve all client/org/platform identifiers from authenticated state.
- Return bounded typed errors; never return provider bodies or secrets.
- Treat a nonce as consumed even when the business call returns a durable waiting state; business idempotency handles retry.

#### 3.3 Create the organization graph atomically

**Cloud/store files**

- `internal/store/agent_onboardings.go`
- `internal/store/store_orgs.go`
- `internal/store/store_billing.go`
- `internal/store/store_usage.go`
- `internal/store/attachment_usage.go`
- `internal/store/feature_flags.go`
- `internal/onboarding/service.go`

**Changes**

- Under the common client lifecycle lock, insert onboarding, org, trial entitlement, MCP and attachment usage rows, and all explicit autonomous-policy flags in one DB transaction.
- Enforce the implementation-approach lifecycle limits at both MCP decode and internal delegation boundaries before normalization or database work; Cloud repeats validation rather than trusting runtime.
- Derive external references from immutable client ID plus server-selected generation. Never reuse a prior generation's org external reference.
- Do not call the existing handler sequence that can commit org before entitlement.
- Do not mint a Cloud API key.
- Record audit entries with hashes of normalized input/output.
- Roll back the entire graph on any pre-provider failure. Once external intent exists, failures enter the fenced lifecycle in Phase 4 rather than deleting evidence.

### Success Criteria

#### Automated verification

- [x] Concurrent same-key start creates one graph and returns one onboarding ID.
- [x] Same key with different normalized input is a typed conflict.
- [x] Different key while a generation is live cannot create a second org.
- [x] A DB failure at every graph step rolls back org, entitlement, usage, and flags.
- [x] Lost MCP/internal HTTP response followed by status returns the persisted graph.
- [x] Onboarding token cannot submit or override client ID, org ID, owner org ID, domain ID, inbox ID, or generation.
- [x] Delegation rejects bad signature, stale timestamp, nonce replay, redirect, or mismatched bearer/client.
- [x] After the first token expires, a new onboarding-scope exchange selects the same live generation and can status/verify/close it.
- [x] A token bound to closed generation N cannot start N+1; a fresh assertion exchange plus new key can.
- [x] With N+1 live, a still-unexpired N token can read only N's closed result, verify is rejected for closed N, and idempotent close returns only N; it cannot observe, verify, close, or otherwise affect N+1.
- [x] `m2m_org` never exposes lifecycle tools and mixed onboarding/email scope exchange is rejected.
- [x] After Phase 3 registration, `m2m_onboarding` lists exactly Start/Status/VerifyDomain/Close, hidden lifecycle calls from `m2m_org` fail, and every profile assertion is cache-private.
- [x] Reconciler retries and assertion-JTI cleanup are exercised with a real PostgreSQL service.

#### Manual verification

- [ ] A canary onboarding client sees only four tools and creates the durable graph without any API-key response.
- [ ] Email-scope exchange is denied before active while onboarding-scope exchange remains available.

#### Phase 3.1 OSS onboarding interface and lifecycle-tool evidence (2026-08-21)

- OSS foundation PR `dsmolchanov/nerve-oss#67` merged as `2a69ea3aabdebc0d3a9fb810713767006d5b6edb`. It added the fixed-origin, no-redirect, HMAC-signed delegation client; bounded request/response envelopes; original-bearer and generation-bound authority forwarding; timeout-as-outcome-unknown behavior; closed/redacted business-error decoding; and client-level coverage for all four operations. Eight fail-closed Codex review generations completed before merge, including fixes for post-dispatch transport ambiguity, presence-aware response decoding, canonical non-nil UUIDs, and the closed public error enum.
- OSS registration PR `dsmolchanov/nerve-oss#68` merged as `a079350e444fbb7a9da86df7c8af49f1d2c87307`. The production app now constructs the bounded delegation client only from a complete onboarding configuration, fails startup on partial or unsafe configuration, and registers it on the real MCP server before the handler is built. An unconfigured runtime remains fail-closed with no lifecycle tools.
- The real modern HTTP handler proves that `m2m_onboarding` lists exactly Start/Status/VerifyDomain/Close with private caching, preserves the original bearer and typed principal for delegation, and rejects authority-bearing input. Every `m2m_org` profile lists none of the lifecycle tools and direct hidden calls cannot reach the provisioner. Semantic failures and untyped failures from every provisioner method are redacted into codes admitted by each tool's published output schema.
- Targeted and full-suite Go tests, vet, MCP race, conformance, runtime-boundary and manifest checks, Docker build, policy-artifact verification, and migration image smoke passed. The first review of PR `#68` found the missing production wiring and an unclosed error schema; fix `67b810be54522bec26e378b73df39e0abf8d90c8` closed both classes in one commit, and the second review returned no P0/P1 before auto-merge. No runtime tag or production deployment changed.

#### Phase 3.2 Cloud delegated boundary and atomic graph evidence (2026-08-21)

- Cloud PR `dsmolchanov/nerve-cloud#129` merged as `55f6f072b075799bbaacf4a33dd7f427c028cd89`. It added the four-operation internal HMAC boundary, durable one-use delegation nonces, strict presence-aware input/result validation, generation-exact service dispatch, and the single-transaction organization/trial/usage/attachment/policy graph with rollback injection at every mutation seam. No Cloud API key is minted.
- The first Cloud review found that a five-minute onboarding bearer did not identify the client assertion key that minted it. OSS authority PR `dsmolchanov/nerve-oss#69` merged as `759a99514b026147cfd8758a3b7cc25f3cf3cee2`; four OSS review generations closed canonical raw-base64url thumbprint validation, whitespace normalization, and the build-tagged dual-profile producer. Cloud `#129` then atomically advanced `oss-source.lock`, emitted the mandatory `client_kid` claim, rejected revoked keys before nonce consumption, and rechecked the same key under the client lifecycle lock before every mutation.
- The final Cloud head proved all four operations reject a bearer whose issuing client key was revoked without consuming the delegation nonce or reaching the executor. Exact-mirror, MCP conformance including the immutable SDK 0.2 dual-profile artifact, full Go/PostgreSQL, vet, race, Cloud E2E, security, and artifact checks passed before auto-merge. Automated sync PR `#130` was closed only after its four blobs were proven byte-identical in `#129`.

#### Phase 3 token-maintenance completion evidence (2026-08-22, local WIP)

- A real OAuth/JWT/PostgreSQL matrix expires the first onboarding token, reacquires the same live generation, closes N, rejects stale-N start, creates N+1 only from a fresh assertion exchange, and proves a still-live N token can observe or idempotently close only N while every attempted N+1 override is rejected without changing its row.
- Scheduled reconciliation now removes assertion-JTI rows in bounded committed batches, reports a follow-up when the batch budget is exhausted, and retries the remaining backlog on the next run while retaining not-yet-expired rows. The focused token/reconciler/CLI packages and vet pass on the integrated local bytes; remote merge and operator-canary evidence remain pending.

---

## Phase 4: Fenced Provider Lifecycle and Billing Closure

### Overview

Run every external operation through a lease/version fence and make cleanup—including Stripe cancellation—a required barrier. Reconnect is possible only after the previous generation is provably closed.

### Changes Required

#### 4.1 Lease and fence every external operation

**Cloud files**

- `internal/store/agent_onboardings.go`
- `internal/store/agent_onboarding_leases.go`
- `internal/store/agent_onboarding_leases_test.go`
- `internal/onboarding/service.go`
- `internal/onboarding/state.go`
- `internal/reconcile/service.go`
- `cmd/nerve-reconcile/main.go`

**Changes**

- Workers discover bounded batches without row locks, acquire client advisory and parent-row locks in deterministic client order, then recheck and claim with `SKIP LOCKED`; lease update and audit remain atomic. This preserves the lifecycle parent-before-child lock order used by close and revocation and prevents an audit-FK deadlock.
- Before an external call, persist provider intent, stable operation ID, and workflow version, commit, then call without holding a DB transaction.
- Every claim increments a monotonic claim attempt used as a per-lease token. Persist, defer, rotate, and result application require that exact attempt in addition to state, workflow version, provider operation, owner, and live lease, so reclaim by the same logical worker still fences every stale handle.
- Apply a result only through the explicit forward lifecycle matrix. In particular, deprovisioning can never return to provisioning, DNS-pending, or active even when every supplied fence otherwise matches.
- Lease expiry permits takeover by provider lookup and stable identity rather than blind recreation.
- `close` and client revocation increment workflow version, clear any earlier-stage retry delay, and enter deprovisioning, fencing every verifier/provisioner already in flight while making cleanup immediately claimable.
- A stale success that created or enabled a resource schedules compensating disable/delete and cannot reactivate the generation.
- Permanent failures store a bounded terminal reason and enter deprovisioning. There is no `failed` state outside live uniqueness and cleanup.

#### Phase 4.1 fenced provider lifecycle evidence (2026-08-21)

- Cloud PR `dsmolchanov/nerve-cloud#131` merged as `18b04e8b3fe024d769bc9507d7a44a90597f7737`. It added bounded PostgreSQL-clock leases, stable provider operation identities, per-claim monotonic attempt fences, exact-result CAS, provider-unknown deferral/readback, fenced cleanup-intent rotation, and atomic audit evidence.
- Four fail-closed review generations closed: cleanup identity rotation after a workflow-version transition; same-owner lease takeover; the explicit forward-only provider transition matrix; immediate cleanup after an earlier retry delay; and deterministic client-parent-before-onboarding lock order that removes the audit-FK deadlock with close/revoke. PostgreSQL barrier tests cover both lifecycle parents, every stale mutation handle, and `SKIP LOCKED` behavior.
- Full Cloud Go/PostgreSQL, race, vet, exact-mirror, MCP conformance, SDK 0.2 compatibility, Cloud E2E, security, artifact, and release-lock checks passed before auto-merge. No deployment or activation state changed.

#### 4.2 Linearize token shutdown and cleanup start

**Cloud files**

- `internal/onboarding/service.go`
- `internal/store/agent_onboardings.go`
- `internal/store/agent_onboarding_close_test.go`
- `internal/store/oauth_lifecycle_barrier_test.go`
- `internal/store/store_tokens.go`
- `internal/oauth/issuer.go`
- `internal/oauth/token_test.go`
- `cmd/nerve-oauth-clients/main.go`

**Changes**

- Close, token issuance, targeted generation email-authority revoke, key revoke, and client revoke use the common lock helper and state recheck defined in Phase 1.
- Close atomically sets deprovisioning, increments the workflow version, disables effective outbound authorization, and revokes only `m2m_org` rows for `oauth_client:<client_id>:g:<generation>` before any provider call.
- Existing/reacquired onboarding tokens remain bound to that same generation and authorize only status plus idempotent close while deprovisioning/closed; they never authorize email or N+1. They expire normally and do not block `closed`.
- `revoke-client` performs the same lifecycle transition, marks the client unusable, and thereby invalidates both onboarding and org tokens before the reconciler finishes cleanup.
- No email-scope token can be issued after the transition commits.

#### Phase 4.2 token-shutdown evidence (2026-08-21)

- Cloud PR `dsmolchanov/nerve-cloud#132` merged as `1504bc8877eaa35b059d39c244ecde2170416d66`. Close, key revoke, client revoke, onboarding-token issuance, and email-token issuance now share the client/generation lock order and recheck authority under the same transaction.
- Two-connection PostgreSQL barriers cover close and client revocation against both onboarding- and email-token issuance in both commit orders. Close leaves at most a same-generation poll/close token, while client/key revocation leaves no newly issued usable token; targeted org-token revocation and outbound suspension commit before later provider cleanup.
- Full Cloud Go/PostgreSQL, exact-mirror, MCP conformance, SDK, Cloud E2E, security, artifact, runtime-lock, and fail-closed Codex review gates passed before merge. No production deployment or activation state changed.

#### 4.3 Fence subscription creation and cancel generation-owned Stripe state

**Cloud files**

- `internal/onboarding/service.go`
- `internal/billing/stripe.go`
- `internal/billing/stripe_test.go`
- `internal/store/store_billing.go`
- `internal/store/agent_billing_workflows.go`
- `internal/store/agent_billing_workflows_test.go`
- `internal/store/agent_onboardings.go`
- `internal/store/agent_outbound_evidence.go`
- `internal/store/oauth_machine_clients.go`
- `internal/reconcile/service.go`
- `cmd/nerve-reconcile/main.go`

**Changes**

- Autonomous `nerve_billing_subscribe` takes the common lifecycle/billing locks, is permitted only for the same active authenticated generation and live registered mandate, and first persists a generation/workflow/billing-profile-version-bound intent plus stable Stripe idempotency key. It writes client ID, org ID, onboarding ID, and generation into Stripe metadata and the local subscription row; caller-supplied provenance is rejected.
- Subscription-create/payment results attach only through CAS against the persisted workflow and billing-profile versions. A concurrent close fences new creation, marks every provider-unknown/materializing intent for cleanup, and cannot conclude `proven_absent` until provider lookup proves no subscription can appear.
- Stripe webhook snapshots validate all generation metadata. A matching result that arrives after close is associated only with the historical billing workflow and queues immediate cancellation; it cannot attach as a live paid subscription or produce compose evidence. Legacy or foreign subscriptions are never canceled by this workflow.
- Under the lifecycle transaction, snapshot every nonterminal subscription-create/payment/subscription object explicitly associated with this onboarding generation and persist deterministic cancellation/readback intents before any Stripe call.
- Request immediate cancellation with a stable provider idempotency key, `invoice_now=false`, and `prorate=false`. Nerve performs no automatic refund or final invoice and does not erase existing obligations.
- A successful response, webhook, or later provider GET may confirm subscription `canceled` or prove no object materialized. Timeout/unknown outcome remains requested/deprovisioning and is reconciled by stable intent/subscription identity.
- Revoke paid evidence when close starts. Never transfer subscription, quota, evidence, or entitlement to generation N+1.
- Permanent Stripe API failure raises an alert and DLQ record but cannot produce a false `closed` state.

#### Phase 4.3 generation-owned billing evidence (2026-08-21)

- The Cloud implementation persists the generation-, workflow-, billing-profile-, and exact resolved-price-bound Stripe intent before dispatch; the versioned stable create identity carries the bounded price ID so catalog rotation cannot alter an outcome-unknown replay. Direct subscription creation uses only the registered customer/payment-method mandate, validates the registered plan/spend/currency ceiling, writes the exact four authority metadata fields, and returns only bounded local state.
- Common lifecycle locks and workflow/profile CAS linearize create against close in both commit orders. Close revokes exact paid evidence and converts every attempted or materialized create into durable cancellation work; only a never-dispatched intent or an authoritative provider 4xx create rejection is locally proven absent. Local configuration failures and malformed successful responses are not absence proof. Duplicate observation of the same already-attached Stripe object is idempotent and does not spuriously fence a live workflow. Profile replacement or disable is fenced until the historical workflow is terminal and cancellation-confirmed.
- Immediate cancellation uses `invoice_now=false` and `prorate=false`; timeout and HTTP 408/409/429/5xx remain provider-unknown and retry with the original idempotency key. DELETE 404 requires authoritative GET readback; canceled or double-404 confirms cleanup. Metadata mismatch and permanent failures become bounded durable non-retry workflow records, remain operator-visible, and cannot create a false terminal proof. The non-retry disposition applies to every dispatcher: scheduler, active subscription snapshots, known-subscription invoices, and metadata-resolved invoices; none may accelerate another provider call until explicit operator recovery, while an exact-ID canceled webhook may still confirm cleanup.
- Autonomous subscription and invoice webhooks validate exact client/org/onboarding/generation metadata. Subscription plan validation accepts only the registered plan lookup key or the exact Stripe price ID embedded in the persisted create identity, so webhook correctness does not depend on Stripe returning an optional lookup key. Late active or trialing snapshots remain historical, cannot mint entitlement or paid evidence, and accelerate cancellation. Unknown autonomous invoices cannot fall through to a legacy customer mapping, and replacement subscriptions clear stale autonomous provenance so cleanup cannot target foreign state.
- Before an unbound autonomous webhook can attach or confirm an object, Nerve requires an actually attempted `provider_unknown` create, replays the persisted create identity, and accepts the webhook only if its subscription ID matches that provider-authoritative result. A never-attempted or confirmed-absent workflow rejects the webhook before any Stripe POST, including after billing-profile replacement. A malformed successful create retains its materialized ID in monotonic quarantined mandatory-cancellation state across unknown/permanent cancellation results and late snapshots; an exact-ID canceled provider response or signed webhook can confirm cleanup without trusting the rejected metadata, while every active/entitlement path still requires the full metadata proof.
- The scheduled reconciler now resolves bounded batches of provider-unknown creates and requested cancellations and exports resolved/pending/permanent counters. Its cleanup client is constructed without mutable price-catalog discovery: exact-price create readback comes from the persisted key and DELETE requires no catalog, so a catalog-only outage cannot block billing or unrelated maintenance. PostgreSQL integration tests cover exact replay/conflict, mandate immutability, cap rejection rollback, delayed create, pre-attachment sibling webhooks, post-absence webhook rejection with zero provider calls, malformed initial/readback materialization followed by both unknown/permanent cancellation and exact-ID webhook confirmation, permanent non-retry across all three webhook acceleration consumers, created/updated/deleted snapshots carrying the persisted price ID without a lookup key, authoritative rejection through both close and client revoke, duplicate provider observations, both lock orders, paid-evidence revocation, create and cancel timeout replay, late active/trialing/invoice events, catalog-independent cleanup, provider readback, and foreign-provenance isolation. Targeted PostgreSQL regressions and compile checks pass locally; full CI/review evidence remains the merge gate for this change.

#### 4.4 Complete cleanup and reconnect

**Cloud files**

- `internal/onboarding/service.go`
- `internal/reconcile/service.go`
- `cmd/nerve-reconcile/main.go`
- `internal/store/agent_onboardings.go`
- `internal/store/agent_onboarding_leases.go`
- `internal/store/agent_onboarding_cleanup.go`
- `internal/store/outbox.go`
- `internal/emailtransport/outbox_worker.go`

**Changes**

- Close takes the per-org policy lock, increments the policy epoch, forbids new outbox claims/provider starts, and terminalizes generation-owned `queued` rows as `policy_revoked`. Every `sending` row must either be fenced before `provider_started_at` or drain to a terminal/readback-resolved outcome after its earlier linearization point.
- Retire a managed alias and disable—but never cascading-delete—the generation-owned inbox in one locked transaction, then revoke its grant; Cloud 0009's deployed inbox trigger requires the disabled inbox write to execute before the alias state becomes `retired`, while the atomic commit exposes neither intermediate state. Transition a custom-domain claim to releasing only after the inbox is disabled, then reconcile provider removal.
- Use the specialized Cloud-only autonomous tombstone predicate only after proving every retained inbox belongs to this onboarding generation, is disabled, has no queued/sending outbox row, and has no unresolved provider-started operation. Existing legacy tombstone behavior remains unchanged.
- `closed` requires terminal Stripe evidence for every generation billing workflow (`canceled` or proven absent), retired/released mailbox and domain state, the outbox barrier, provider readback proving disabled/deleted, no live `m2m_org` generation token, and a tombstoned org with retained disabled inbox/audit rows. Short-lived onboarding status/close tokens do not block this state.
- A repeated close returns the same progress. An old start/close idempotency key returns its persisted generation.
- Only a fresh onboarding token and new start key after `closed` can allocate N+1, with a distinct org external reference and no inherited resources.

### Success Criteria

#### Automated verification

- [x] Verify-versus-close and verify-versus-reconcile races cannot overwrite a newer workflow version or restore active state.
- [x] Expired-lease takeover resumes by stable provider identity without duplicating resources.
- [x] Stale provider success schedules compensating cleanup.
- [x] Permanent provisioning failure remains uniquely live in deprovisioning until cleanup completes.
- [x] Two-connection barriers prove close versus email issuance leaves no usable email token; close versus onboarding issuance leaves at most a poll/close-only token for N; and `revoke-client` versus either issuance leaves neither token usable, in both commit orders.
- [x] Close revokes email tokens and outbound permission before the first Stripe/provider call.
- [x] Subscribe-versus-close tests cover both transaction commit orders, a delayed create response, provider-unknown lookup, `requires_action`, and a webhook that materializes `trialing`/active after close; all converge to cancellation without paid evidence or a false `closed`.
- [x] Stripe timeout remains deprovisioning and retry uses the same idempotency key.
- [x] `closed` is impossible while a subscription-create outcome remains unresolved, an in-flight/provider-started email is unresolved, or a required subscription cancellation/provider cleanup is unconfirmed.
- [x] Two-connection barriers cover enqueue, claim, provider-start, complaint, and close in both commit orders: no provider start occurs after the new policy epoch, queued rows terminalize, and an earlier provider start keeps close pending until terminal/readback.
- [x] A worker holding a payload cannot race inbox cleanup into an untracked send; inbox rows remain disabled/retained, MarkSent detects zero affected rows, and the autonomous tombstone predicate refuses any nonterminal outbox state.
- [x] FK/retention tests prove close preserves outbox/message/audit evidence and never invokes cascading DeleteInboxForOrg.
- [x] Generation 2 receives distinct org/trial state and inherits no subscription/evidence.

#### Operator verification

- [ ] Killing a reconciler after each external call converges to active or fully closed without duplicate provider resources.
- [ ] Closing a paid canary confirms Stripe cancellation before tombstone and permits a clean reconnect.

#### Repository implementation evidence (2026-08-21)

- The first close now advances the org policy epoch exactly once even when complaint handling had already set `email_outbound_suspended=true`; repeated close remains idempotent. Existing Core 0029 claim/provider-start CAS and recovery paths provide the queued/provider-start drain barrier.
- Scheduled reconciliation prepares managed cleanup in one locked transaction (disable exact inbox, irreversibly retire its alias, then revoke the grant), moves exact autonomous custom-domain claims to `releasing`, deletes the persisted Resend domain identity outside the transaction, and removes the claim/Core domain only after idempotent provider absence proof.
- The Cloud-only tombstone predicate refuses close for nonterminal Stripe workflow state, unresolved onboarding provider intent/lease state, live generation tokens, ownership claims, queued/sending or unresolved provider-started outbox work, any foreign/active inbox, an unretired managed alias, an active grant, or an attached custom domain. It soft-tombstones the org while retaining the exact disabled inbox and its mailbox/outbox/audit evidence, then exposes N+1 onboarding with a distinct org and billing reference.
- PostgreSQL regressions cover already-suspended close, provider-start drain, stale `MarkSent` claim loss, Stripe confirmation, managed alias/inbox/grant cleanup, custom-domain provider proof, retained inbox/message/outbox/audit evidence, idempotent close, and clean N+1 reconnect. Reconciler and CLI metrics report confirmed versus pending onboarding closures.

---

## Phase 5: Fresh Nerve-Managed Mailbox

### Overview

Provision a ready mailbox from a protected preverified platform domain and make every reserved or retired address impossible to create through another API, catch-all, or SQL path.

### Changes Required

#### 5.1 Register and validate the platform domain

**Cloud files**

- `deploy/cloud/oss-source.lock`
- `internal/config/config.go`
- `internal/config/config_test.go`
- `internal/store/managed_mailbox_aliases.go`
- `internal/store/agent_onboarding_cleanup.go`
- `internal/store/inboxes_manage.go`
- `internal/store/inboxes_manage_test.go`
- `internal/store/email_tenancy.go`
- `internal/store/store.go`
- `internal/store/store_orgs.go`
- `internal/store/cloud_m2m_migration_test.go`
- `internal/store/cloud_email_tenancy_test.go`
- new `internal/store/inbound_forward_replay.go`
- new `internal/store/migrations/cloud/0010_managed_mailbox_canonical_addresses.sql`
- `internal/startup/migrations.go`
- `internal/startup/migrations_test.go`
- `internal/cloudapi/attachment_read_test.go`
- `internal/cloudapi/handler_inboxes.go`
- `internal/cloudapi/handler_domains.go`
- `internal/cloudapi/handler_test.go`
- `internal/cloudapi/resend_webhook.go`
- `internal/cloudapi/resend_webhook_test.go`
- `internal/reconcile/service_test.go`
- `cmd/nerve-migrate/main_test.go`
- `scripts/release/generate_control_plane_manifest.sh`
- `scripts/ci/test_control_plane_manifest.sh`
- `deploy/cloud/runtime.lock`
- `deploy/cloud/env.example`
- `docs/REPO_SPLIT_RUNBOOK.md`
- `cmd/nerve-drill/main.go`
- new `cmd/nerve-drill/managed_mailbox.go`
- `cmd/nerve-drill/managed_mailbox_test.go`

**OSS prerequisite files**

- `AGENTS.repo-invariants.md`
- `internal/store/inboxes_manage.go`
- `internal/store/inboxes_manage_test.go`
- `internal/store/email_tenancy.go`
- `internal/store/store.go`
- `internal/store/store_orgs.go`
- `internal/tools/outbound_policy.go`
- `internal/tools/outbound_policy_test.go`

**Changes**

- Configure enabled state, owner org ID, and platform org-domain ID; keep production identifiers outside the repository.
- Insert or validate `managed_mailbox_platform_domains` only after proving the owner org is live, the domain is active/fully verified, receiving is enabled, and catch-all is disabled.
- Under the platform-domain advisory lock and with allocation disabled, snapshot every existing active and disabled inbox on the chosen canonical domain. Permanently backfill each address/inbox pair into the alias registry as legacy-reserved with its observed state, then prove a second snapshot has no unregistered address before enabling the platform writer. Never attach these reservations to an onboarding generation.
- Fail closed on missing/mismatched configuration or readiness.
- Disabling new allocation leaves the registered namespace and all retired aliases protected.
- Land address canonicalization at every shared inbox boundary OSS-first, merge it, and atomically advance `oss-source.lock` with the byte-identical Cloud mirror. All supported Store reads and writers use one canonical-equivalence predicate for case, complete outer whitespace, and one trailing domain dot. Every identity-creating, reactivating, disabling, deleting, or cleanup Store path pre-discovers both address-candidate and linked-domain namespaces, takes every distinct canonical-domain transaction lock in bytewise sorted order before org-policy/row locks, re-reads the same identities under lock, detects ambiguity, and preserves valid legacy noncanonical stored bytes. New Store-created bytes are canonical. Add the recurring invariant and deterministic six-boundary plus lifecycle PostgreSQL matrices in OSS. Arbitrary direct SQL against a standalone Core database is outside this tranche; extending that guarantee requires a separately approved Core migration and release graph.
- Canonicalize loaded legacy address bytes before Cloud HTTP replay/self-forward checks, inbound forwarding comparisons, outbound-domain authorization, and provider `From` construction. Stored evidence remains byte-identical; comparisons and emitted provider identities use the canonical value. Invalid or ambiguous legacy bytes fail closed. Artifact B on Cloud 9 deliberately retains Artifact A's forwarding validation, key, and provider-address bytes during rolling overlap. Each B forwarding/config transaction takes the shared Cloud-schema-boundary lock and selects behavior from the migration version under that lock: schema 9 uses exact A behavior, while schema 10 uses canonical replay/provider identity. Cloud 0010 takes the exclusive side, so it drains all schema-9 B transactions before enabling canonical behavior; the transition additionally proves every A Machine is gone before 0010 begins.
- Add forward-only Cloud 0010 after deployed 0009. Under its upfront inbox/table fences, preflight the entire hosted active-address set for canonical collisions, replace Core 0024's lower-only partial index with a byte-preserving functional unique index over the exact shared equivalence, and reject new noncanonical address storage while still allowing unrelated updates and status-only reactivation of a unique legacy row. Then preflight the managed namespace and install the replacement alias trigger so linked platform identity or the canonical final address domain catches trailing-dot, extra-`@`, whitespace, foreign-link, and other bypass spellings. No address bytes are rewritten.
- Widen only the nondeployable validation control-plane compatibility manifest through Cloud 0010 and refresh only the Cloud schema member of the runtime lock during implementation. Historical A remains immutable at Core `[28,28]`/Cloud `[8,9]`, as do the signed 0009 transition bundle/receipt and its A-specific issuance-off evidence. B and C are future release-set artifacts, not historical fixtures: Phase 9 constructs B at Core `[28,29]`/Cloud `[9,10]` and C at Core `[29,29]`/Cloud `[10,10]`.
- Cloud 0010 is implementation-ready but not production-authorized until the final release set contains B plus exact protected `release-set-issuance-off` and `cloud-0010-transition` producer specifications. The first producer derives B from the set and emits fresh release-set-bound issuance-off evidence. The transition derives B, migration bytes, and the immutable 0009 receipt only from the release set; accepts only that fresh issuance-off receipt run ID/SHA as additional caller evidence; verifies the Core28/Cloud9 prestate; keeps issuance off; and emits a separately attested post-deploy 0010 receipt binding both predecessor receipt digests and the same release-set SHA. The existing A/schema-9 issuance-off workflow and historical 0009 evidence are never widened or reinterpreted.

#### 5.2 Allocate under database-enforced namespace ownership

**Cloud files**

- `internal/store/managed_mailbox_aliases.go`
- new `internal/store/managed_mailbox_allocation_test.go`
- `internal/store/agent_onboardings.go`
- `internal/store/email_tenancy.go`
- `internal/store/email_tenancy_test.go`
- `internal/store/inboxes_manage.go`
- `internal/cloudapi/resend_webhook.go`
- `internal/cloudapi/handler_agent_onboarding_test.go`
- `internal/onboarding/service.go`

**Changes**

- Generate the random local part and inbox UUID only on the server.
- In one transaction insert a `reserved` alias naming that exact inbox UUID, ensure the owner-to-grantee grant, create that inbox, and transition the alias `active`.
- Reuse existing locked grant/inbox helpers; do not add a second public provisioning implementation.
- Retry only a cryptographic collision; do not expose an address-choice loop to the client.
- Persist the winning address before returning.
- Retiring cleanup disables the exact generation-owned inbox before changing its alias to `retired` in the same transaction, matching the deployed Cloud 0009 and replacement Cloud 0010 trigger order; it never deletes or reactivates either identity.
- Cloud 0009 installs the first guard; Cloud 0010's replacement triggers and functional index provide the final boundary. They reject ordinary create/ensure/address update/reactivate/direct SQL unless a registered address maps to the same reserved/active inbox ID; retired rejects even its old ID.
- Database enforcement prevents catch-all enablement for the platform domain. Inbound routing drops/rejects unknown or retired recipients before the generic catch-all path.

### Success Criteria

#### Automated verification

- [x] Platform owner/grantee confusion and inactive-domain cases fail closed.
- [x] Registration refuses any unaccounted pre-existing platform inbox; active and disabled rows are permanently backfilled, cannot be allocated/reactivated under another ID, and survive later inbox status changes.
- [x] Parallel allocations never share an address.
- [x] Injected failures after grant, alias, or inbox insert roll back the entire graph.
- [x] Same-key replay returns the same address.
- [x] Cleanup plus generation 2 produces a different address.
- [x] Ordinary create, ensure, address update, reactivate, catch-all, direct SQL, and concurrent races cannot claim a reserved or retired alias.
- [x] No non-alias inbox can activate under the platform domain and catch-all cannot be enabled.
- [x] Inbound to an unknown or retired platform address creates no inbox or thread.
- [x] Tenant can read/use the granted domain but cannot mutate or delete the owner's domain.
- [x] Inbound Resend delivery resolves to the correct grantee inbox/org.

#### Phase 5.1 platform-domain registration evidence (2026-08-22)

- Deployment configuration now carries only the allocation gate, owner-org UUID, and platform org-domain UUID; production identifiers remain outside the repository. Enabled-without-identity, partial identity, and malformed UUIDs fail config load. Disabling allocation retains the configured identity so the database namespace remains addressable and protected.
- The Cloud-only registration transaction takes the canonical-domain advisory lock, locks the live owner and exact domain, requires canonical/active/fully DNS-verified/receiving-enabled/catch-all-disabled readiness, inserts new platform rows disabled, backfills every active or disabled inbox as an immutable legacy reservation, and proves a second snapshot contains no missing or foreign mapping before enabling allocation. Revalidation is idempotent and does not rewrite a legacy reservation when its inbox later becomes disabled.
- Existing exact registrations may always move to disabled even after domain or owner readiness degrades; enabling and first registration still require the full readiness proof. Namespace triggers continue rejecting unknown active and disabled inboxes while the allocation state is disabled.
- `nerve-drill managed-mailbox configure|status` operates on deployment configuration. Configure requires an operator actor and atomically records hashed input/output evidence plus a replay ID with the registration transaction. The runbook documents the disabled-first activation sequence and permanent-reservation invariant.
- PostgreSQL regressions cover foreign owner, inactive/unverified/receiving-disabled/deleted-owner refusal, active and disabled legacy snapshot backfill, permanent refusal to allocate or reactivate a legacy-disabled address under another inbox, conflicting foreign reservation rollback, concurrent ordinary inbox creation behind the advisory lock, idempotent revalidation after inbox status change, degraded emergency disable, and continued namespace protection.
- UUID identities are normalized to their canonical semantic spelling at configuration and store boundaries. Reservation additionally requires the exact live, active-client, provisioning managed-mailbox onboarding and grantee identity with no prebound mailbox resources.
- OSS and Cloud canonicalize every address-based create, ensure, lookup, default-inbox, inbound-resolution, and reactivation boundary. Equivalent case/outer-whitespace/trailing-dot inputs converge under one lock; all six read/create/ensure surfaces plus external-reference replay preserve valid legacy bytes, refuse semantic duplicates, and fail closed on ambiguous historical state.
- Cloud 0010 globally rejects duplicate active canonical identities, atomically replaces the lower-only active index with the shared byte-preserving functional index, and rejects new noncanonical storage. It also closes the deployed 0009 managed-trigger gap for trailing-dot, extra-`@`, whitespace, linked-only, and other spellings. Candidate and linked canonical namespaces use the same deterministic bytewise order in Go and SQL (`COLLATE "C"`) before advisory acquisition. Its schema-9 preflight is serialized in both commit orders, rejects already-committed bypass/collision rows before the trigger swap, accepts exact unique active/disabled legacy reservations, preserves legacy bytes, and is forward-only.
- B on Cloud 9 emits the same forwarding idempotency key and provider `To`/`From` bytes as A. A deterministic boundary test holds a schema-9 forwarding transaction, proves Cloud 0010 waits on the exclusive schema-boundary lock, then proves the first post-0010 transaction uses canonical replay/provider identity. Historical A forwarding rows remain replayable after a semantically equivalent config spelling is touched, while true different destinations remain independent.

#### Phase 5.2 atomic managed-mailbox allocation evidence (2026-08-22, local WIP)

- The server generates exactly `agent-` plus lowercase unpadded Base32 of 128 cryptographically random bits and a separate UUID inbox identity. One authority-locked transaction creates the graph, permanently reserves that exact alias/inbox tuple, ensures the owner-to-grantee grant, creates the exact-ID inbox, activates the alias, and persists the winning address while advancing the onboarding to active. Only alias/inbox cryptographic uniqueness collisions retry the complete transaction.
- Twelve parallel real-PostgreSQL allocations produce twelve distinct canonical addresses. Same-key replay returns the exact onboarding/address/inbox/grant graph, while failure injection after alias, grant, and inbox creation rolls back the organization and every allocation child. Close/cleanup disables the inbox before irreversible alias retirement; N+1 receives a different address and inbox, and the retired recipient no longer resolves.
- The public delegated boundary proves collision retry is invisible to the client, inbound delivery lands in the exact grantee org, and unknown or retired recipients create no inbox, message, or thread. An app-role fixture with actual `UPDATE` and `DELETE` privileges proves write RLS exposes zero owner-domain rows to the grantee while its active grant remains readable and usable. An independent final P0/P1 review of the repaired local bytes is clean; remote source-authority, review, and operator evidence remain pending.

#### Manual verification

- [ ] Canary receives a real external email at its generated platform address.
- [ ] The message appears through modern list_threads/get_thread within the established ingestion SLO.
- [ ] Mail to an unallocated sibling address creates no inbox or thread.

---

## Phase 6: Autonomous Custom-Domain Ownership and Readiness

### Overview

Serialize legacy and autonomous ownership through one canonical-domain claim and make provider provisioning resumable, fenced, and releasable. Activation still requires the complete mail path.

### Changes Required

#### 6.0 Freeze the Phase 6 source-authority inventory

The Phase 6 dependency direction and repository ownership are part of the
security boundary. The following inventory is normative; a source-lock advance
is invalid unless both `sync-manifest.yaml` copies contain the same
classification and every exact-mirror path is byte-identical:

| Authority | Files | Manifest treatment |
|---|---|---|
| OSS-first exact mirror | `internal/emailaddr/**`; `internal/domains/canonical.go`; `internal/domains/canonical_test.go`; `internal/store/store_orgs.go`; `internal/store/org_domains.go`; `internal/store/org_domains_test.go`; `internal/store/domain_ownership_claims.go`; `internal/store/legacy_domain_lifecycle.go`; `internal/store/legacy_domain_lifecycle_test.go`; `internal/emailtransport/providers/resend/resend_domains.go`; `internal/emailtransport/providers/resend/resend_domains_test.go`; `docs/MCP_Contract.md`; `sync-manifest.yaml` | Explicit `exact-mirror` entries. The complete email-address package and tests share one authority. The lifecycle test uses an OSS test-local Cloud-9 schema fixture rather than making OSS depend on Cloud migrations. |
| OSS/runtime only | `internal/mcp/**`, including onboarding and billing tool registration, catalog, invoker, protocol adapters, and runtime-only tests | Intentionally absent from every sync list. Runtime code must not import Cloud lifecycle or schema packages. |
| Cloud only | `internal/cloudapi/handler_domains.go`; `internal/cloudapi/handler_domains_legacy_test.go`; `internal/cloudapi/handler_agent_onboarding_test.go`; `internal/store/provider_domain_quarantine.go`; `internal/store/agent_onboarding_domains.go`; `internal/store/agent_onboarding_domains_test.go`; `internal/store/agent_onboarding_domain_projection_test.go`; `internal/onboarding/service.go`; `internal/onboarding/service_test.go`; `internal/onboarding/state.go`; `internal/onboarding/provider_worker.go`; `internal/onboarding/resend_domain_provider.go`; `internal/onboarding/resend_domain_provider_test.go`; `internal/domains/instructions.go`; `internal/domains/verification.go`; `internal/domains/verification_test.go`; `internal/reconcile/service.go`; `internal/reconcile/service_test.go`; `internal/reconcile/onboarding_provider_worker_test.go`; Cloud command wiring under `cmd/**` | Explicit `cloud-only` entries where a same-name or patch-synced path could otherwise cross the boundary; all other listed Cloud-only paths remain non-mirrored. Provider/quarantine orchestration and its handler/scheduler tests never become runtime authority. |

`internal/emailaddr` is the lower-level dependency. It must not import
`internal/domains`. It may use the pinned IDNA profile only to validate an
already-ASCII A-label; it never converts a U-label. The higher-level
`internal/domains/canonical.go` exclusively performs U-label-to-A-label
conversion with the pinned IDNA Lookup profile, then delegates the ASCII
domain validation downward to `internal/emailaddr`. Copying only the
canonical-domain wrapper would recreate the `emailaddr`↔`domains` cycle or
give the two repositories different claim identities. `store_orgs.go` is
shared compatibility behavior and is therefore reclassified from Cloud-only
to exact mirror. The shared legacy lifecycle file owns only typed Store
intent/CAS/absence-proof and cleanup due/claim/defer state; all provider I/O,
quarantine mutation, HTTP handling, scheduling, and command construction stay
Cloud-only. The shared MCP contract is the exact union of the OSS runtime
onboarding/billing surface and the Cloud delegation semantics.

#### 6.1 Put every domain path behind one claim

**OSS-first shared files**

- `internal/emailaddr/emailaddr.go`
- `internal/emailaddr/emailaddr_test.go`
- `internal/domains/canonical.go`
- `internal/domains/canonical_test.go`
- `internal/store/store_orgs.go`
- `internal/store/org_domains.go`
- `internal/store/org_domains_test.go`
- `internal/store/domain_ownership_claims.go`
- new `internal/store/legacy_domain_lifecycle.go`
- new `internal/store/legacy_domain_lifecycle_test.go`
- OSS and Cloud `sync-manifest.yaml`

**Cloud-only files**

- new `internal/store/agent_onboarding_domains.go`
- new `internal/store/agent_onboarding_domains_test.go`
- new `internal/store/agent_onboarding_domain_projection_test.go`
- `internal/store/provider_domain_quarantine.go`
- `internal/cloudapi/handler_domains.go`
- new `internal/cloudapi/handler_domains_legacy_test.go`
- `internal/cloudapi/handler_test.go`
- `internal/onboarding/service.go`
- `internal/onboarding/state.go`
- `internal/reconcile/service.go`

**Changes**

- Author canonical-domain and shared Store lock/finalization changes in OSS first, mirror them byte-for-byte into Cloud, and advance `oss-source.lock` only after the OSS authority merge. Use the same IDNA Lookup profile for U-label/A-label, case, and trailing-dot convergence in both repositories.
- Acquire one global transaction advisory lock derived only from canonical domain; never include org ID.
- Under that lock create/reconcile `domain_ownership_claims` for legacy REST and autonomous onboarding alike.
- Treat every foreign pending, provider-owned, or releasing claim as a conflict regardless of claim expiry. Under the canonical lock, an expired pending claim is first fenced into `releasing` and its owner workflow into `deprovisioning`; it cannot be overwritten or rebound.
- Give autonomous Core domain rows a stable `m2m-onboarding:` external-reference prefix.
- Replace direct pending deletion: generic `ExpirePendingDomains` excludes autonomous rows and cannot delete any claimed domain.
- Legacy expiry/delete transitions its claim to releasing and uses the same confirmed provider-cleanup path.
- Release a claim only after provider readback proves safe removal and no live Nerve resource depends on it.

#### 6.2 Run the provider workflow under lifecycle fencing

**OSS-first shared files**

- `internal/emailtransport/providers/resend/resend_domains.go`
- `internal/emailtransport/providers/resend/resend_domains_test.go`
- new `internal/store/legacy_domain_lifecycle.go`
- new `internal/store/legacy_domain_lifecycle_test.go`
- `internal/store/org_domains.go`
- `internal/store/org_domains_test.go`
- `internal/store/domain_ownership_claims.go`

**Cloud-only files**

- `internal/onboarding/service.go`
- `internal/onboarding/provider_worker.go`
- new `internal/onboarding/resend_domain_provider.go`
- new `internal/onboarding/resend_domain_provider_test.go`
- `internal/store/provider_domain_quarantine.go`
- `internal/domains/instructions.go`
- `internal/domains/verification.go`
- new `internal/domains/verification_test.go`
- `internal/store/agent_onboarding_leases.go`
- `internal/store/agent_onboarding_leases_test.go`
- `internal/store/agent_onboarding_cleanup.go`
- `internal/store/agent_onboarding_close_test.go`
- `internal/store/agent_outbound_evidence.go`
- `internal/reconcile/service.go`
- `internal/reconcile/service_test.go`
- `internal/reconcile/onboarding_provider_worker_test.go`
- `internal/cloudapi/handler_domains.go`
- new `internal/cloudapi/handler_domains_legacy_test.go`
- `cmd/nerve-control-plane/main.go`
- `cmd/nerve-control-plane/main_test.go`
- `cmd/nerve-reconcile/main.go`
- `cmd/nerve-reconcile/main_test.go`

**Changes**

- Author the shared Resend client in OSS first with repository-neutral Nerve transport identity, then mirror it byte-for-byte into Cloud; Cloud-only lifecycle orchestration may consume it but must not fork its retry, redaction, or exact-ID semantics.
- Persist pending Core domain and ownership claim before external work.
- Perform provider create/lookup/verify/receiving-enable/disable/delete outside DB transactions using stable operation IDs.
- Unknown outcomes remain resumable only through the exact persisted provider-domain ID or a Phase 1 explicitly adopted inventory record. Resend domain creation has no provider-native idempotency key or operation/fence metadata, so a provider object found only by canonical name after an uncertain create cannot match the workflow's intent cryptographically: it is quarantined and never auto-adopted or recreated. An explicit protected adoption receipt is the only path that may bind that object to the workflow.
- Persist complete DNS records/checks and transition to `dns_pending`.
- Apply results only under the current workflow version and lease. Close/expiry fences in-flight verification and moves the claim to releasing.
- A stale create/enable success initiates compensating disable/delete. Permanent provisioning errors enter deprovisioning; no state escapes cleanup.
- The scheduled reconciler consumes the legacy cleanup due/claim/defer Store
  substrate only through an injected provider interface. For an exact identity
  it commits the cleanup claim, performs exact-ID GET, receiving-disable,
  DELETE, and final exact-ID GET outside every Store transaction, and releases
  local state only on authoritative same-ID 404. Provider uncertainty retains
  the exact identity and clears the lease for bounded retry; a present final
  readback remains failed/releasing. `awaiting_provider_proof` performs bounded
  canonical inventory and quarantine only, never adoption or mutation, and
  defers fairly. An open quarantine prevents provider mutation and cannot
  relabel the already durable exact local identity as provider-only. Command
  construction remains a separate approval-gated activation boundary: a nil
  provider leaves the consumer inert and claims no rows.
- Reconcile every active autonomous custom domain at least once per fifteen minutes. A confirmed ownership, provider-verification, MX, or receiving-capability loss demotes it to `dns_pending`, revokes effective compose evidence, advances the policy epoch, and retains the generation-owned inbox for recovery. Unknown transport outcomes do not revoke readiness until an authoritative readback succeeds.

#### 6.3 Require complete live readiness and instructions

**OSS-first contract file**

- `docs/MCP_Contract.md`

**Cloud docs/files**

- `docs/TENANT_GUIDE.md`
- `internal/onboarding/service.go`
- `internal/cloudapi/handler_agent_onboarding_test.go`
- `sdk/python` examples

**Changes**

- Synthesize the authoritative MCP contract in OSS as a union: preserve the complete Phase 7 billing tool contract and the complete Phase 6 delegated-onboarding contract, then mirror those exact bytes into Cloud. Neither section may overwrite or omit the other.
- Activate only after ownership challenge, SPF, DKIM, inbound MX, provider verified status, and receiving-enabled readback all succeed.
- Never accept caller-supplied verified state.
- Create the requested inbox transactionally after provider checks. Keep legacy public domain semantics but use the stricter autonomous predicate.
- Return record type, name, value, TTL where applicable, purpose, observed state, and retry guidance in ordinary complete results.
- State that the agent must use another DNS connector.
- Never ask for registrar/API credentials in a Nerve tool.

### Success Criteria

#### Automated verification

- [x] Canonical case/IDN/trailing-dot variants resolve to one claim.
- [x] Legacy-versus-autonomous and autonomous-versus-autonomous pending races produce one winner.
- [x] Migration preflight rejects ambiguous existing claims.
- [x] Legacy pending GC cannot delete autonomous work or bypass provider cleanup.
- [x] Expired-pending create-versus-GC and unknown-provider-outcome races never rebind the claim until the old provider identity is proven absent/removed and the old claim is released.
- [x] Provider timeout and lease takeover do not duplicate provider domains.
- [x] A provider-only orphan or mismatched provider ID can never be adopted by canonical-name lookup without the explicit preflight/adopt receipt; writer enable remains blocked while quarantine is unresolved.
- [x] Verify-versus-close cannot restore active state; stale success is compensated.
- [x] SPF/DKIM without MX/receiving remains dns_pending.
- [x] Only full readiness creates the inbox and domain-scoped compose permission.
- [x] Domain revocation or receiving failure removes effective domain compose permission.
- [x] A claim is reusable only after confirmed provider cleanup and release.

#### Manual verification

- [ ] Canary workflow publishes records through a separate DNS-provider credential.
- [ ] Nerve never logs or receives that credential.
- [ ] Real inbound and outbound delivery work on the custom domain.

---

## Phase 7: Reply Safety, Paid Compose, and Durable Abuse Limits

### Overview

Separate reply from compose, make autonomous policy fail closed, remove inbound-derived trust from V1, and make every rate counter reconstructible from durable events.

### Changes Required

#### 7.1 Split scopes without breaking legacy tokens

**OSS files**

- `internal/mcp/catalog.go`
- `internal/mcp/invoker.go`
- `internal/mcp/invoker_test.go`
- new `internal/mcp/billing.go`
- new `internal/mcp/billing_tools_test.go`
- `internal/mcp/errors.go`
- `internal/mcp/sdk_server.go`
- `internal/mcp/sdk_server_test.go`
- `internal/mcp/server.go`
- `internal/auth/context.go`
- `docs/MCP_Contract.md`
- `docs/SECURITY.md`

**Changes**

- Modern send_reply requires nerve:email.reply.
- Modern compose_email requires nerve:email.compose.
- Add `nerve_billing_subscribe` as a modern `m2m_org` tool requiring `nerve:billing.subscribe`; it delegates through a narrow `BillingProvisioner` to the control plane and rejects caller-supplied provenance/payment fields.
- Legacy 2025-11-25 continues to recognize nerve:email.send.
- Existing orgs without autonomous_outbound_policy retain existing visibility and behavior.
- Autonomous org tools/list omits compose while locked; direct calls are still denied.
- Principal kind identifies autonomous M2M behavior; a missing flag never silently reclassifies `m2m_org` as legacy.

#### 7.2 Evaluate reply and compose against a transaction-scoped policy snapshot

**Delivery-side items are a separate OSS-first change from the adapter work.** The MCP 2026 adapter already carries the enqueue-time policy snapshot, shared policy-writer lock, latest-inbound reply selection, and server-side final-content evaluation. The remaining claim/provider-start/drain half lands with Core 0029 because it must survive worker crashes and lock release. It is not complete until the migration, runtime worker, suspension/close writers, reconciler, and PostgreSQL race fixtures land together and sync to Cloud.

**OSS-first/shared files**

- new `internal/store/migrations/core/0029_outbox_policy_fence.sql`
- `internal/tools/service.go`
- new `internal/tools/outbound_policy.go`
- `internal/store/store_threads.go`
- `internal/store/outbox.go`
- `internal/store/outbox_test.go`
- new `internal/store/outbox_policy_fence_test.go`
- `internal/store/feature_flags.go`
- `internal/emailtransport/outbox_worker.go`
- `internal/emailtransport/outbox_worker_test.go`
- `internal/entitlements/service.go`
- `configs/policy/autonomous-outbound-v1.yaml`
- `sync-manifest.yaml`
- `scripts/release/generate_runtime_manifest.sh`

**Changes**

- send_reply selects the latest direction=inbound message in the tenant-owned thread and rejects outbound-only threads.
- Recipient, sender inbox, org, and thread are resolved server-side.
- For `m2m_org`, read explicit autonomous policy inside the enqueue transaction. Missing row, malformed value, or DB error denies; do not use the generic cached resolver.
- compose_email evaluates suspension, selected-domain ownership/readiness, and confirmed paid projection before enqueue.
- A granted platform domain is never treated as org-owned custom-domain evidence.
- Re-evaluate final content with server policy; do not trust a submitted needs_human_approval=false value.
- Perform send-policy decision, durable rate reservation, suppression check, idempotency, and outbox enqueue in a transactionally consistent order.
- Replay returns the prior result without consuming another daily unit.
- Use the common per-org policy lock/epoch for enqueue, claim, provider-start, complaint/suspension, clear, and close. Claim cannot select a suspended/deprovisioning autonomous org; the pre-provider transaction rechecks epoch/policy and records the durable provider-start fence.
- Suspension/close terminalizes queued autonomous rows and prevents any later provider start. An already-started call is drained/read back and keeps suspension cleanup or onboarding close nonterminal until its outcome is known; no DB lock is held across the network call.

#### 7.2a Core 0029 migration and provider-start state machine

- Author Core 0029 in nerve-oss first and mirror it byte-for-byte to Cloud. Create the RLS-protected `org_outbound_policy_state` table keyed by org, with a positive monotonic `policy_epoch`, and add the four nullable fence columns specified in the implementation approach to `outbox_messages`.
- Keep legacy rows untouched and all new outbox columns null. The migration is additive at the SQL/data boundary: frozen legacy statement fixtures must continue to enqueue, claim, deliver, and inspect legacy rows. Do not claim immutable v0.0.17 binary compatibility on Core 0029; its startup window remains `[28,28]`, and the Phase 9 R0 bridge supplies the actual post-migration legacy runtime.
- The down migration succeeds only while the epoch table is empty and every new outbox column is null; otherwise it refuses rather than discarding provider-start/drain evidence.
- Seed the epoch row in the same transaction as the autonomous organization graph. Every suspension, clear, or close writer takes the org-policy lock, increments the epoch exactly once for a real policy transition, updates its evidence/flags, and terminalizes stale queued rows atomically.
- Autonomous enqueue copies the current locked epoch. Claim admits legacy rows unchanged, but admits an autonomous row only when its saved epoch equals the live epoch and the complete fail-closed policy snapshot still permits sending.
- Immediately before the network call, CAS the claimed row under the org-policy lock using outbox ID, worker ownership, status, saved epoch, and unresolved state. Record one stable provider operation ID and `provider_started_at`; only the transaction that commits this CAS may call the provider.
- If policy changed before that CAS, terminalize the row as `failed/policy_revoked` without a provider call. If policy changes after it, suspension/close observes unresolved provider-start evidence and waits for terminal response or idempotent replay/readback.
- Apply provider success, permanent failure, retryable known failure, timeout, and crash recovery without clearing start evidence. Set `provider_resolved_at` only when the logical provider outcome is known; an unknown outcome remains unresolved and may be retried only with the same provider idempotency/operation identity.
- Keep database locks out of the network call. The reconciler claims unresolved starts with `SKIP LOCKED`, resolves them through provider idempotency replay/readback, and uses CAS so a stale worker cannot overwrite a later resolution.

#### 7.2b Threat model for outbound content policy

The caller is the tenant's own authenticated `m2m_org` agent sending from that tenant's inbox. Content policy is a guardrail between the tenant and its own model, so ordinary model encodings such as entities and inline markup are rendered to visible text before matching. Deliberate token-holder evasions such as homoglyphs, zero-width joins, CSS-hidden text, or nested encodings are outside this guardrail: that same tenant already controls the channel. This boundary does not relax memory limits, tenant isolation, authorization, scope checks, or the enqueue/provider-start fence, which continue to treat the caller as hostile because they protect other tenants and the platform.

#### 7.3 Project only paid and abuse evidence

**Cloud files**

- `internal/cloudapi/handler_billing.go`
- `internal/billing/stripe.go`
- `internal/billing/stripe_test.go`
- `internal/cloudapi/handler.go`
- `internal/cloudapi/resend_webhook.go`
- new `internal/cloudapi/resend_webhook_abuse_test.go`
- `internal/store/store_billing.go`
- `internal/store/agent_paid_projection_test.go`
- `internal/store/agent_billing_workflows.go`
- new `internal/store/agent_abuse_projection.go`
- new `internal/store/agent_abuse_projection_test.go`
- `internal/store/agent_outbound_evidence.go`
- `internal/store/feature_flags.go`
- `internal/reconcile/service.go`
- `internal/reconcile/service_test.go`
- `cmd/nerve-reconcile/main.go`
- `cmd/nerve-reconcile/main_test.go`

**Changes**

- Implement `nerve_billing_subscribe` at the actual control-plane billing service. Resolve principal-bound generation and registered billing profile server-side; the legacy checkout route rejects autonomous orgs before calling Stripe.
- Record paid evidence only after a successful Stripe payment/invoice and authoritative readback of a currently active qualifying subscription; trialing or `requires_action` is not paid.
- Under the common billing/lifecycle lock, one store transaction applies subscription, entitlement, evidence, projected feature flag, and webhook processed marker. `invoice.paid` never directly assigns active.
- Persist provider event time/identity and use authoritative Stripe GET for payment-granting or ambiguous/out-of-order transitions. The deprovisioning/cancellation fence is monotonic and a late `invoice.paid` or older subscription snapshot cannot reactivate the subscription or compose evidence.
- Revoke paid evidence when the subscription ceases to qualify or onboarding close starts.
- A complaint or threshold hard-bounce signal writes deny evidence and email_outbound_suspended=true in the same transaction.
- Add an audited operator clear that records reason and actor; do not delete history.
- Remove the proposed trust command, earned-trust source, inbound-count projector, and earned-trust tests.
- Treat inbound From/headers as message data only; they never mutate authorization.
- A bounded Store reconciler recomputes paid/suspension projection from durable
  authority and detects drift under the shared lock order. Constructing the
  suspension repair from the scheduled production command is part of the same
  explicit tenant-wide abuse-policy activation as the signed-webhook projector.
  Before that activation, the scheduled command may consume only the read-only
  drift list and export its bounded count.
- The scheduled paid-subscription readback consumer is constructed through a
  separate injected read-only provider interface. A nil provider is inert and
  does not list or mutate due workflows. When injected, it loads exact
  generation/profile/workflow authority, performs Stripe GET outside every
  Store transaction, captures the observation fence from PostgreSQL after the
  provider call, validates exact subscription/customer/generation/price
  provenance, and applies the existing Store CAS in a fresh transaction.
  Exact 404 and non-qualifying authoritative status revoke; transport/auth/rate
  uncertainty preserves only live unexpired proof; evidence expiry revokes;
  and exact-GET provenance disagreement revokes compose authority without
  rewriting the stored subscription to mismatched provider bytes. Close or
  profile/version change wins the Store CAS. The command exports bounded
  candidate/active/demoted/unknown/fenced metrics but deliberately does not
  construct the provider until production billing activation is separately
  authorized.

#### 7.4 Reserve durable limits with matching events

**OSS-first/shared files**

- new `internal/store/outbound_limits.go`
- `internal/entitlements/rate_limiter.go`
- `internal/tools/outbound_policy.go`

**Cloud files**

- `internal/store/store_usage.go`
- `internal/reconcile/service.go`
- `internal/reconcile/service_test.go`

**Changes**

- Use PostgreSQL UTC buckets and the common `(org_id,meter,period_start,period_end)` counter-row lock across Machines. Reservation and reconciliation take the same lock and derive the effective `period_end` from the row held under that lock.
- `internal/store/store_usage.go` remains Cloud-only under `sync-manifest.yaml`, so Cloud provides `RecordUsageEventAt` as the compatibility boundary required by the OSS-first shared `outbound_limits.go` implementation.
- Keep ordinary `RecordUsageEvent` on PostgreSQL's authoritative `DEFAULT now()` path by omitting `created_at`; use `RecordUsageEventAt` only when an explicit authoritative timestamp is part of the operation, including reservation, backfill, and deterministic tests.
- For every reply, total-send, first-recipient, or recipient-hash reservation, insert a matching successful `usage_events` row in the same transaction as idempotency and outbox enqueue.
- Derive the global replay ID as SHA-256 over the versioned length-prefixed `(org_id,tool_name,idempotency_key,meter,dimension)` tuple so retry cannot increment twice, and cross-org/cross-tool keys cannot collide.
- Reconciler locks the counter before SUM and SET and performs both in one transaction. A concurrent reservation is therefore wholly before the SUM or waits and increments after SET.
- Count an attempt when accepted for enqueue regardless of later provider outcome; do not refund abuse units.
- Keep the existing process-local MCP RPM limiter only as a cheap front-line shedder until a durable MCP RPM replacement is proven.
- Garbage-collect expired rate buckets without removing audit or delivery history.
- **Resolved follow-up (2026-08-19):** Cloud PR #105 now re-reads the complete counter period under `FOR UPDATE` and its real PostgreSQL fixtures prove reservation-first and reconcile-first orderings. OSS PR #47 plus Cloud sync PR #107 prove org/tool/meter replay namespaces against the database and advance the source lock atomically. The earlier accepted period-boundary risk is closed.

### Success Criteria

#### Automated verification

- [x] Onboarding token cannot call any email tool.
- [x] Locked autonomous org can read/draft/reply but cannot list or call compose.
- [x] Autonomous policy missing/read-error/malformed denies M2M send while legacy behavior is unchanged.
- [x] Reply on an outbound-only thread fails and enqueues nothing.
- [x] Reply after multiple outbound messages still targets the latest real inbound sender.
- [x] Custom-domain evidence enables compose only for that owned domain.
- [x] Confirmed paid evidence enables org-wide compose, including managed mailbox.
- [x] Trial entitlement alone does not enable compose.
- [x] Spoofed From, self-mail, arbitrary Authentication-Results headers, and inbound volume never unlock compose.
- [x] Suspension overrides every scope/evidence path immediately, including a previously issued compose token.
- [x] Enqueue/claim/provider-start versus complaint/close barriers in both orders prove no provider start after the new epoch and correct drain/readback of the earlier linearization point.
- [ ] Core 0028→0029 migration preserves every legacy outbox row, leaves its fence columns null, and frozen legacy SQL-shape enqueue/claim/delivery fixtures remain green; artifact-level Core 29 compatibility is separately proven by R0 before production migration.
- [x] Core 0029 down succeeds only with no epoch rows or outbox fence evidence and refuses independently for a saved autonomous epoch, provider start, provider operation identity, or provider resolution timestamp.
- [x] Claim-versus-suspension and provider-start-versus-suspension/close two-connection fixtures cover both commit orders; a pre-fence row never calls the provider, while a post-fence row keeps cleanup nonterminal until resolved.
- [x] Crash after claim, after provider-start commit, after provider response, and before terminal persistence converges through the same provider operation identity without duplicate logical delivery or false `closed`.
- [x] One complaint suspends; bounce threshold obeys sample size and rate.
- [x] Multi-Machine concurrent sends cannot exceed durable limits.
- [x] Every counter increment has a matching event and a generic reconcile preserves the value.
- [x] Cross-org same-key, cross-tool same-key, and two-connection reserve-versus-reconcile fixtures prove no replay collision or lost update.
- [x] Concurrent period-boundary update versus reconcile cannot exclude newly in-period events; a two-connection fixture proves both lock orderings.
- [x] Idempotent replay consumes one applicable unit/event and creates one outbox row.
- [x] Closing a paid generation removes compose immediately and never transfers it to reconnect.
- [x] Caller manipulation of needs_human_approval cannot bypass policy.
- [x] Autonomous billing rejects the legacy checkout route, caller org/generation/payment fields, missing/disabled mandate, cap violations, and `requires_action`; exact tool replay returns one subscription workflow.
- [ ] Stripe tests cover `subscription.deleted -> late invoice.paid`, external cancellation while active, close versus invoice in both orders, provider readback disagreement, and crash between mutation and event processed marker; cancellation always wins.

#### Manual verification

- [ ] Managed canary replies to a real sender but cannot compose until payment is confirmed.
- [ ] Custom-domain canary composes after verification without a manual Nerve action.
- [ ] Synthetic complaint/suspension removes compose from tools/list and blocks a cached call.

---

## Phase 8: Python SDK 0.3.0, Contracts, and Agent Documentation

### Overview

Build the immutable runtime and modern client candidates exactly once while preserving verified rollback and legacy-consumer compatibility. Do not publish either candidate in this phase.

### Changes Required

#### 8.1 Implement SDK 0.3.0 and both response modes

**Cloud files**

- `sdk/python/pyproject.toml`
- `sdk/python/src/nerve_email/__init__.py`
- `sdk/python/src/nerve_email/client.py`
- `sdk/python/src/nerve_email/exceptions.py`
- `sdk/python/src/nerve_email/tools.py`
- new `sdk/python/src/nerve_email/protocol.py`
- `sdk/python/src/nerve_email/oauth.py`
- `sdk/python/requirements.lock`

**Changes**

- Bump package, module, and client version to 0.3.0.
- Send the exact modern headers/metadata/capability and unwrap `content` plus `structuredContent`.
- Parse by base media type: one matching JSON response or request-scoped SSE with multiline data/notifications and exactly one matching final response.
- Implement exact protected-resource/AS discovery, form-encoded private-key JWT exchange, early refresh, and exactly one retry after a valid 401 challenge.
- Export the Phase 0 `PrivateKeyJWTAuth(client_id, private_key_pem, kid=None, algorithm="PS256")` through the 0.3 public API and add an optional `auth=` constructor argument. Preserve its already-gated behavior: accept unencrypted RSA PKCS#8 or PKCS#1 PEM bytes/string only, never an implicit filesystem path; reject non-RSA, encrypted, malformed, or less-than-2048-bit keys. Exactly one of API key, static bearer, or private-key JWT auth may be configured.
- Derive the public RSA JWK and RFC 7638 SHA-256 thumbprint locally. Default `kid` to that thumbprint and reject any supplied mismatch. PS256 is the default; RS256 is permitted only as an explicit registered compatibility override. Assertions use `iss=sub=client_id`, the discovered token endpoint as exact `aud`, `iat`, `exp=iat+60`, a fresh UUID `jti`, and header `alg/kid/typ`.
- Pin direct runtime dependencies `PyJWT[crypto]==2.13.0` and `cryptography==49.0.0`; generate a hash-locked `requirements.lock` for all build/test environments. Authorization is generated per exchange/request and private material is never installed as a static httpx header.
- Keep onboarding and org tokens in separate cache entries keyed by resource, client, selected generation, and canonical scope set; refreshing one must not replace the other.
- After a closed generation, a fresh onboarding exchange selects N+1; never reuse the token bound to N.
- Never log a private key, assertion, bearer token, or full JWK.
- Support a deliberate legacy mode/fallback for ordinary email tools against immutable v0.0.17 on Core 28 and the behavior-equivalent R0 bridge on Core 28/29.
- Do not pretend onboarding works after fallback to a runtime older than vNext.
- Continue accepting static Cloud API keys and bearer tokens for existing clients.

#### 8.2 Lock both client artifacts

**Files**

- `.github/workflows/ci.yml`
- `.github/workflows/deploy.yml`
- `.github/workflows/publish-python-sdk.yml`
- `sdk/python` tests

**Changes**

- Download the published nerve_email 0.2.0 wheel and verify SHA-256:

      9f0a7d6316bf47eef64236f96d1a7a151b5517641930422b1b16711da8b02540

- Run that exact wheel against the vNext hybrid runtime.
- Build SDK 0.3.0 once in successful main CI, retain it as an immutable candidate, record its SHA, and install only those bytes in every contract job.
- Remove direct publish dispatch; publication is callable only by the Phase 10 post-soak workflow with signed matching evidence.
- Test 0.3.0 against vNext modern mode, immutable v0.0.17 legacy mode on Core 28, and R0 legacy mode on Core 28/29.

#### 8.3 Synchronize contract and candidate authority atomically

**OSS/Cloud files**

- OSS `docs/MCP_Contract.md`
- OSS `sync-manifest.yaml`
- OSS `configs/policy/autonomous-outbound-v1.yaml`
- Cloud `docs/MCP_Contract.md`
- Cloud `sync-manifest.yaml`
- Cloud `scripts/ci/export_oss_tools_list.go.tmpl`
- Cloud `deploy/cloud/oss-source.lock`
- new Cloud `deploy/cloud/runtime-candidate.lock`
- new Cloud `scripts/ci/fetch_runtime_candidate_manifest.sh`
- new Cloud `scripts/ci/test_runtime_candidate_manifest_transport.sh`

**Changes**

- Before building, select the approved final runtime semver, prove it is unused across git tags, GitHub Releases, and public OCI tags, and freeze it as `runtime_version` in the candidate manifest and OCI labels without creating a public tag. Prove the SDK 0.3.0 filename is not already published with different bytes.
- Build the OSS candidate exactly once from an exact successful main SHA without a semver tag, GitHub Release, or public release asset; retain the attested manifest/index/platform digests as the sole Phase 9 inputs.
- In one Cloud PR, apply exact mirrors, advance `oss-source.lock`, and write `runtime-candidate.lock` with the attested candidate locator/digests, source revision, MCP contract hash, policy hash, and runtime-manifest SHA. Keep deployed-artifact `runtime.lock` on v0.0.17 through candidate construction. The later protected R0 deployment is authorized only by its signed bridge receipt/release-set member and does not mutate `runtime.lock`; changing source/candidate/bridge metadata alone is non-deploying.
- Advance `CORE_SCHEMA_HASH` to the OSS-authority Core 0029 tree and pin the runtime candidate to `[29,29]`. Before the migration gate, build and attest R0 with `[28,29]`, prove exact legacy equivalence on Core 28, then deploy it on production Core 28. Immutable v0.0.17 is never claimed compatible with Core 29.
- Update contract export to cover onboarding, compose-locked/unlocked, attachment-off/on, legacy/modern, and structured error shapes.
- Do not mutate a public version tag or PyPI in this phase; Phase 9 binds candidates and Phase 10 publishes tested bytes.

#### 8.4 Document the zero-human flow and prerequisites

**Files**

- `README.md`
- `docs/TENANT_GUIDE.md`
- `docs/MCP_TRANSPORT.md`
- `docs/SECURITY.md`
- `sdk/python/README.md`
- `dashboard/src/app/(dashboard)/api-docs/page.tsx`

**Changes**

- Provide private-key generation and public-JWK registration instructions without copying private material.
- Show token discovery/exchange, onboarding start/status, reauthorization, email use, close, and reconnect.
- Show both managed and custom-domain branches.
- Explicitly identify the separate DNS connector requirement.
- Explain that compose unlocks only by live custom-domain readiness or confirmed paid subscription; inbound mail cannot earn trust.
- Explain that zero-human paid compose requires an operator-preauthorized SCA-complete off-session billing mandate at client registration. Show `nerve_billing_subscribe`, spending-cap enforcement, fail-closed `requires_action`, and that agents never automate hosted Checkout or handle card data.
- Explain immediate asynchronous subscription cancellation, no automatic prorated refund, deprovisioning polling, and no entitlement transfer on reconnect.
- Keep 0.2.0 legacy examples clearly labeled.

### Success Criteria

#### Automated verification

- [x] SDK version constants, wheel metadata, filename patterns, and importlib version all equal 0.3.0.
- [ ] Official client conformance passes for 2026-07-28.
- [x] OAuth tests cover discovery, assertion, refresh, clock skew, replay response, and secret redaction.
- [x] Private-key tests cover PKCS#8/PKCS#1, PS256/explicit RS256, RSA-size and encryption rejection, derived/mismatched kid, deterministic clock/JTI, exact claims, single auth mode, and no secret leakage.
- [x] Candidate build installs exact hash-locked PyJWT 2.13.0 and cryptography 49.0.0 bytes and rejects dependency/hash drift.
- [x] 0.3.0 emits every required header/metadata/capability and parses JSON plus multiline/notification SSE.
- [x] Missing content type, wrong/duplicate ID, malformed/truncated SSE, cancellation, and EOF without final response fail safely.
- [x] Separate token-cache tests cover active, deprovisioning, expiry, close, and N-to-N+1 selection.
- [x] 0.3.0 modern tests cover all tool success/error schemas and result unwrapping.
- [ ] 0.3.0 legacy mode passes against immutable v0.0.17 on Core 28 and R0 on Core 28/29 for existing email tools.
- [ ] Exact published 0.2.0 wheel passes against vNext.
- [ ] Candidate/source locks, frozen final semver, policy, manifest, and exact-mirror checks are green in the same Cloud PR while production runtime.lock still validates v0.0.17; rebuilding or changing manifest bytes is not a later release step.
- [x] The SDK publish workflow cannot be manually dispatched.
- [x] No hard-coded 0.2.0 build assertion remains except the explicit legacy-consumer fixture.

#### Manual verification

- [ ] A reference external agent can connect with only endpoint, client_id, and its private key.
- [ ] No Nerve API key is generated or copied during onboarding.

---

## Phase 9: Candidate Release Set and Core/Cloud Contraction

### Overview

Replace historical Artifact A with feature-complete Artifact B, designate B as the post-0010 control-plane rollback floor, replace v0.0.17 with the already attested non-semver R0 bridge while Core is still 28, apply forward-only Cloud 0010 under B, then apply additive Core 0029 and deploy the `[29,29]` runtime plus contracted Artifact C. The runtime and SDK remain unpublished candidates addressed only by immutable digest/SHA; R0 remains the separately attested bridge artifact embedded in the release set.

### Artifact identities

| Artifact | Purpose | Core window | Cloud window |
|---|---|---:|---:|
| validation | Local CI-only current-tree artifact; non-publishable and non-runnable | checked-in head | checked-in head |
| v0.0.17 | Immutable historical production baseline; pre-Core29 only | `[28,28]` | n/a |
| R0 | Non-semver legacy bridge and authorized post-Core29 runtime rollback floor | `[28,29]` | n/a |
| A | Historical Cloud migration-boundary predecessor from Phase 1 | `[28,28]` | `[8,9]` |
| B | Final feature-complete Core/Cloud expand predecessor and permanent rollback floor | `[28,29]` | `[9,10]` |
| C | Normal contracted production control plane | `[29,29]` | `[10,10]` |

A and B are different immutable images. B may return to A only while Core28/Cloud9 still holds; once Cloud 0010 commits, A is historical transition evidence and is no longer an operational rollback target. R0 is also a distinct immutable image: it retains v0.0.17 behavior while widening only the compiled Core window to `[28,29]`. R0 may return to v0.0.17 only before Core 0029; afterward R0—not v0.0.17—is the independently attested below-vNext runtime fallback. The runtime candidate requires Core `[29,29]`.

### Changes Required

#### 9.1 Build B and C, resolve the Phase 8 candidates, and attest the release set

**Files**

- `internal/startup/migrations.go`
- `internal/startup/migrations_test.go`
- `internal/release/release_set.go`
- `internal/release/release_set_test.go`
- `deploy/cloud/Dockerfile.control-plane`
- `.github/workflows/ci.yml`
- `.github/workflows/deploy.yml`
- `.github/workflows/runtime-deploy.yml`
- `.github/workflows/control-plane-deploy.yml`
- new `.github/workflows/mcp2026-candidate.yml`
- new `.github/workflows/mcp2026-runtime-mirror.yml`
- new `.github/workflows/mcp2026-release-set-issuance-off.yml`
- new `.github/workflows/mcp2026-cloud-0010-transition.yml`
- `scripts/ci/verify_cloud_deploy_order.sh`
- `schemas/mcp2026-release-set.schema.json`
- new `schemas/mcp2026-release-set-issuance-off.schema.json`
- new `scripts/release/generate_mcp2026_release_set_issuance_off_receipt.py`
- new `scripts/ci/verify_mcp2026_release_set_issuance_off_receipt.py`
- new `scripts/release/build_mcp2026_release_set.sh`
- new `scripts/ci/verify_mcp2026_release_set.sh`
- new `schemas/mcp2026-cloud-0010-transition-receipt.schema.json`
- new `scripts/release/generate_mcp2026_cloud_0010_transition_receipt.py`
- new `scripts/ci/verify_mcp2026_cloud_0010_transition_receipt.py`
- new `scripts/ci/test_mcp2026_post_set_receipts.sh`
- OSS candidate workflow from Phase 8
- OSS R0 bridge workflow and receipt from Phase 0
- `.github/workflows/publish-python-sdk.yml`

**Changes**

- Build B only after Phases 2-8 are complete. B contains final behavior and spans deployed Cloud 9 through forward-only Cloud 10. It does not support Cloud 8 because the signed A transition already crossed production and Cloud 0009 durable evidence forbids returning to 8.
- Build C from an explicit contraction change. Restrict the B-to-C source diff to compiled Core and Cloud minima, compatibility tests, manifests, and release wiring; fail CI if business behavior changes.
- Resolve the exact already-built Phase 8 runtime candidate and SDK 0.3 wheel by attested workflow-run identity and SHA; verify source, manifest, frozen semver, filename, and digests. Phase 9 never rebuilds either artifact.
- Before release-set construction, run the protected mirror workflow with only the verified OSS candidate run ID and receipt SHA. Authenticate to Fly Registry, copy the exact candidate digest to a content-addressed non-semver `registry.fly.io/nerve-runtime:sha-...` tag without deployment or rebuild, and resolve both target index and linux/amd64 Machine digest. An already exact tag is an idempotent retry; any mismatch fails closed. Emit the signed runtime-mirror receipt defined in Phase 0.
- Resolve and reverify both the Phase 0 historical v0.0.17 baseline receipt and the protected R0 bridge receipt. Release-set construction accepts only their protected run IDs/SHAs and the new candidate mirror receipt run ID/SHA; it accepts no raw Fly tag, baseline tag, bridge tag, patch, or digest.
- Re-prove the frozen runtime semver and SDK filename remain unpublished before constructing the release set; a collision stops the release and requires a new plan revision/candidate build rather than manifest rewriting.
- Generate/attest the canonical release set defined in Phase 0, binding historical A and its immutable 0009 transition evidence, including protected run/artifact/object locators sufficient to retrieve and reverify the canonical bundle and receipt without caller inputs; future B/C with distinct OCI index plus linux/amd64/Fly Machine digests; runtime candidate and verified Fly mirror receipt; the historical v0.0.17 baseline and authorized R0 bridge; SDK, schemas, contract, policy, exact mirrors, conformance pins, source SHAs, and workflow identities. It also binds Cloud head/tree 10, exact 0010 bytes and pre/post contract, B identity, and the protected release-set issuance-off and 0010 producer specifications/identities. The post-set issuance-off and post-deployment 0010 receipts are independently attested and name this release-set SHA; neither is hashed back into its parent set.
- Define a distinct release-set issuance-off receipt schema/generator/verifier for exact B at Core28/Cloud9 with head10/pending0010, the release-set SHA, durable Machine inventory/digests, issuance control version/state, lock scope, producer identity, and timestamp. Do not widen or reinterpret `mcp2026-issuance-off.schema.json`, which remains the immutable Artifact A/Cloud8-or-9 historical evidence format.
- The Cloud 0010 receipt consumes a fresh post-transition issuance-control observation and proves it remains disabled, durably bound to the same release-set SHA, and at the exact control version from the predecessor release-set issuance-off receipt. A hard-coded boolean or a changed/rebound control row cannot satisfy the transition.
- Inject the verified release-set SHA and bounded signed canonical set/verification envelope at deployment time. B/C require it from their immutable artifact-role manifest even when every environment marker is absent. Each binary verifies the envelope offline and reports its build-manifest identity plus that runtime-bound release-set identity only after proving exact role/image/manifest/binary membership; no binary or manifest is rebuilt to embed the release-set hash.
- Candidate component selection accepts only release-set run ID/SHA. Remove or disable raw image, tag, wheel, manifest, and digest inputs in `deploy.yml`, `runtime-deploy.yml`, and `control-plane-deploy.yml`; every candidate entry point derives exact components from the verified set, and reusable child workflows reject calls without the parent verification receipt. Any B or C control-plane deploy on Cloud 10 additionally requires the independently attested 0010 receipt run ID/SHA bound to that set. The transition's exact Cloud10/B resume path may mint the first receipt; every later steady schema-10 deploy fails closed without it. Extend the central deploy-order verifier to enforce release-set issuance-off→B→0010→receipt ordering and the exact schema9/schema10 resume states.

#### 9.2 Rehearse the final lifecycle and rollback graph

- Restore a fresh production snapshot at Core 28/Cloud 9 including every onboarding lifecycle state, expired/active leases, domain claims, aliases, billing workflows, provider retries, tokens, usage meters, and legacy outbox states; rehearse v0.0.17→R0→B→Cloud 10→Core 29→runtime candidate→C and the reverse C→B plus runtime-candidate→R0 paths while retaining Core 29/Cloud 10.
- Before building/deploying the candidate, run a claim-drift preflight proving every live Core domain has exactly one canonical ownership claim and no claim points at a missing/wrong live domain.
- Run all six B manifest-listed database-mutating binaries across Core 28/29 and Cloud 9/10. Full autonomous behavior requires Core 29 plus Cloud 10; every predecessor combination proves legacy behavior, issuance-off refusal, and absence of unsupported-schema query/mutation before each binary's first side effect.
- Run C on Core 29/Cloud 10 and prove all six manifest-listed database-mutating binaries reject Core 28 or Cloud 9 before listener, lease, provider call, or mutation.
- Rehearse controlled C→B→C rollback on Core 29/Cloud 10 with no data rewrite or migration.
- Rehearse controlled runtime-candidate→R0→runtime-candidate rollback on Core 29 with no schema change, using only release-set identities and proving every Machine digest.
- Run immutable SDK 0.2 and exact candidate SDK 0.3 contracts against the release-set runtime.

#### 9.3 Install B as rollback floor and contract to C

1. Under the shared deploy lock, verify Core current 28/head 29/pending `[0029]` and Cloud current 9/head 10/pending `[0010]`, plus the immutable signed Phase 1 transition receipt and its historical A-specific issuance-off evidence.
2. Deploy the release-set R0 bridge on Core 28 from its content-addressed image. Prove every active/stopped runtime Machine is R0, its digest and `[28,29]` window match the signed receipt, production startup verification is enabled, and v0.0.17-equivalent legacy/SDK 0.2 contracts remain green.
3. Deploy B web and reconciler while R0 serves Core 28; run all six manifest-listed compatibility commands and the legacy contract on Core 28/Cloud 9.
4. Prove every active/stopped/scheduled control-plane Machine is B at Core28/Cloud9 and record B in signed deployment evidence as the designated post-0010 control-plane rollback floor; record R0 as the runtime rollback floor. Until 0010 commits, exact B→A remains an authorized predecessor rollback.
5. Invoke `mcp2026-release-set-issuance-off.yml` with only the release-set run ID/SHA. It derives B, verifies every durable control-plane Machine is exact B at Core28/Cloud9 with head10/pending0010, takes the common deploy/global-issuance lock, atomically records issuance off for this release-set SHA, and emits a fresh signed receipt. Raw image/schema inputs and the historical A receipt cannot satisfy this producer.
6. Invoke only the release-set-bound Cloud 0010 transition with the final set and the fresh B issuance-off receipt run ID/SHA. It derives and verifies the immutable 0009 receipt from the set, re-verifies all three producer identities, permits only the known Core28/Cloud9+B prestate or an exact Core28/Cloud10+B resume, applies 0010 once, proves every Machine remains B and issuance remains off, and emits the separately attested 0010 receipt binding the release set plus both the 0009 and fresh issuance-off receipt digests. Cloud 0010 down is forbidden; after this commit A is no longer runnable and recovery is a forward fix under B.
7. Apply Core 0029 with B's migrate binary only after verifying the 0010 receipt. Before continuing, prove Core current/head 29, Cloud current/head 10, zero pending, preserved legacy rows/SQL behavior, exact schema hashes, R0 startup/readiness, and legacy/SDK 0.2 contracts on the actual production R0 Machines.
8. Deploy the release-set runtime candidate only from its verified content-addressed Fly mirror; prove every active/stopped runtime Machine, resolved linux/amd64 digest, `[29,29]` compatibility, and both legacy/modern contract smoke.
9. Deploy C web; admit traffic only after the shared Core `[29,29]`/Cloud `[10,10]` readiness check.
10. Converge reconciler to C only after web is green; run reconciler and migrate compatibility before mutation/schedule activation.
11. Prove every web/reconciler Machine and binary digest/window is C and run both SDK contracts again.

If C fails, roll web and reconciler back to B on Core 29/Cloud 10. Do not change schema and never roll back to A, Core 28, or Cloud 9 after Cloud 0010 commits.

### Success Criteria

- [ ] B is feature-complete, runs legacy/issuance-off behavior across Core 28/29 and Cloud 9/10, and full behavior on Core 29/Cloud 10; C is behaviorally equivalent on 29/10 but refuses Core 28 or Cloud 9.
- [ ] Every manifest-listed database-mutating binary verifies and reports its immutable build-manifest identity, injected release-set hash, exact set membership, and correct window without an artifact rebuild.
- [ ] Missing/wrong injected release-set identity and every raw candidate deployment bypass fail before listener or mutation.
- [ ] Release-set issuance-off evidence rejects Artifact A or the historical schema, any non-B Machine, wrong Core28/Cloud9/head10/pending0010 state, wrong set/producer/lock identity, enabled issuance, or substituted receipt bytes.
- [ ] B is recorded before any C Machine receives traffic.
- [ ] The dedicated 0010 transition derives B and migration bytes only from the release set, emits a separately attested receipt bound to it, and supports only exact schema9/B preflight or schema10/B resume; historical 0009 evidence remains byte-identical.
- [ ] C→B→C rehearsal on Core 29/Cloud 10 and production rollback require no schema change or data loss.
- [ ] v0.0.17→R0 on Core 28 and runtime-candidate→R0→runtime-candidate on Core 29 use distinct attested digests and require no schema change or data loss; v0.0.17 is rejected as a post-Core29 target.
- [ ] One signed release set binds every exact candidate and policy/contract/schema provenance item.
- [ ] Runtime and SDK bytes/digests equal the Phase 8 artifacts exactly, and the frozen runtime manifest/version is byte-identical before and after release-set construction.
- [ ] Mirror production accepts only the candidate receipt, copies without rebuild/deploy, is idempotent only for an exact existing tag, and release-set verification rejects a missing/substituted mirror receipt.
- [ ] The release set embeds both the independently verified historical v0.0.17 baseline and distinct R0 bridge members, and the dedicated rollback entry point accepts only release-set + a fresh Phase-10 post-disable issuance-control receipt + complete-drain evidence and resolves R0; historical A or pre-0010 issuance-off evidence is rejected.
- [ ] No semver tag, GitHub Release, public OCI release tag, or PyPI 0.3 file exists yet.

**Phase gate:** Do not begin production drills until every Machine is C, B and R0 are recorded, the signed release set independently verifies from production, and both the immutable 0009 receipt and release-set-bound 0010 receipt verify.

---

## Phase 10: Two-Client Drills, Soak, Publication, and Activation

### Overview

Validate both onboarding modes with isolated least-privilege clients. Keep every partner inactive during deployment and soak; publish and activate only through evidence-bound post-soak workflows.

### Changes Required

#### 10.1 Register two isolated synthetic clients

- `synthetic-managed` permits managed-mailbox mode only.
- `synthetic-custom-domain` permits custom-domain mode only for the disposable delegated zone.
- Use distinct client IDs, keypairs, scopes, orgs, generations, rate buckets, and audit identities. Store private keys only in the protected canary secret store; upload only public JWKs.
- The release-set-bound production-canary workflow alone registers/activates the two configured `synthetic` identities after C is proven; it checks their immutable class and cannot name or activate an `external` client. The managed synthetic additionally receives a spending-capped operator-owned live Stripe canary billing profile whose payment method and off-session mandate completed SCA before this workflow; only provider references enter the registry.
- With global issuance still off, a protected `mcp2026-enable-issuance` step verifies the signed release set, exact deployed runtime/C digests, Core 29/Cloud 10, policy/contract hashes, both the immutable 0009 receipt and release-set-bound 0010 receipt, and exactly these two active synthetic identities. Under the shared deploy/global-issuance lock it atomically enables `oauth_issuance_control` for that release-set SHA and emits a signed issuance-control enable receipt binding the prior/new control versions and states. Only then may the first token exchange run.
- Missing/wrong release set, an external active client, stale Machine digest, schema drift, or a concurrent disable makes enable/exchange fail closed. Every rollback first invokes the distinct protected `mcp2026-disable-issuance` entry point under the same lock, atomically disables the exact current release-set control version, and emits a fresh Phase-10 post-disable issuance-control receipt binding exact C/runtime/Core29/Cloud10 identities and prior/new state. It explicitly rejects both historical Artifact A evidence and the Phase-9 pre-0010 issuance-off receipt. Only that fresh receipt may authorize lifecycle drain and below-vNext rollback.
- Prove each client is denied the other mode and every cross-client onboarding/org/domain/inbox/thread/attachment resource.
- Keep all partner clients pending/revoked. No deploy, tag, SDK-publish, or smoke workflow may activate one implicitly.

#### 10.2 Managed lifecycle drill

1. Exchange for a generation-bound onboarding token; replay its assertion JTI and prove rejection.
2. Start twice with one idempotency key and prove one generation and one never-used alias.
3. Let the initial token expire, reacquire `nerve:onboarding`, and prove status/close still target the same generation.
4. Mint an org token only after active state; receive, read, and reply to real external mail.
5. Prove compose remains hidden/denied before confirmed payment.
6. As payment actor, use the preauthorized operator-owned canary billing profile to call `nerve_billing_subscribe`; confirm a real qualifying paid subscription and authoritative provider readback, prove org-wide compose becomes live, and capture client/generation/mandate-profile/subscription provenance without card data. If Stripe returns `requires_action`, the drill fails closed and no human completes a challenge mid-run; the operator must repair the registration prerequisite before a clean retry.
7. Close generation 1 and wait for every provider, billing, email-token, and storage barrier, including immediate cancellation of the canary subscription and durable confirmation.
8. With a fresh onboarding token and new idempotency key, create generation 2 and prove new generation, org external reference, and alias. Keep it active only for cross-client drill checks.

#### 10.3 Custom-domain lifecycle drill

1. Use the separate custom client to start generation 1 on the disposable DNS zone.
2. Apply only returned records through the protected DNS-provider credential.
3. Poll ordinary complete results until strict readiness; prove no MRTR fields appear.
4. Mint org token and prove domain-scoped compose, real inbound, and outbound delivery.
5. Run cross-client/cross-org denials against managed generation 2.
7. Emit two separately attested lifecycle receipts—managed and custom—from the recorded drill events and zero-live-resource queries. Promotion accepts only each receipt's protected workflow-run ID plus SHA and verifies that both name the exact deployed release-set SHA; component overrides are impossible.

#### 10.4 Run continuous soak canaries

**Files**

- new `.github/workflows/mcp2026-production-canary.yml`
- new `.github/workflows/mcp2026-enable-issuance.yml`
- new `.github/workflows/mcp2026-disable-issuance.yml`
- new `.github/workflows/mcp2026-post-soak-promote.yml`
- new `.github/workflows/mcp2026-activation-approval.yml`
- new `.github/workflows/mcp2026-activate-clients.yml`
- new `scripts/deploy/mcp2026_managed_canary.py`
- new `scripts/deploy/mcp2026_custom_domain_canary.py`
- new `schemas/mcp2026-issuance-control-receipt.schema.json`
- new `scripts/release/generate_mcp2026_issuance_control_receipt.py`
- new `scripts/ci/verify_mcp2026_issuance_control_receipt.py`
- new `scripts/ci/test_mcp2026_issuance_control_receipt.sh`
- new `schemas/mcp2026-lifecycle-receipt.schema.json`
- new `scripts/release/generate_mcp2026_lifecycle_receipt.py`
- new `scripts/ci/verify_mcp2026_lifecycle_receipt.py`
- new `scripts/ci/verify_mcp2026_activation.py`
- new `schemas/mcp2026-activation-approval.schema.json`
- new `scripts/release/generate_mcp2026_activation_approval.py`
- new `scripts/ci/verify_mcp2026_activation_approval.py`
- new `schemas/mcp2026-soak-evidence.schema.json`
- new `scripts/release/generate_soak_evidence.py`
- new `scripts/ci/verify_mcp2026_soak_evidence.py`
- new `schemas/mcp2026-promotion-evidence.schema.json`
- new `schemas/mcp2026-promotion-request.schema.json`
- new `schemas/mcp2026-oss-promotion-receipt.schema.json`
- new `scripts/release/generate_mcp2026_promotion_evidence.py`
- new `scripts/ci/verify_mcp2026_promotion_evidence.py`
- `.github/workflows/cloud-e2e-smoke.yml`
- `.github/workflows/publish-python-sdk.yml`
- `docs/REPO_SPLIT_RUNBOOK.md`
- OSS new `.github/workflows/mcp2026-promote-candidate.yml`
- OSS `.github/workflows/docker-publish.yml`

**Changes**

- The Phase-10 issuance-control receipt has `action=enable|disable` and binds the release-set SHA; prior/new durable control version, enabled state, and recorded set identity; exact deployed control-plane/runtime roles and digests; Core29/Cloud10 plus both transition receipts; lock scope; producer/workflow/run identity; and timestamp. The verifier requires a strictly increasing committed control version and the exact expected state transition. A disable receipt is fresh only for the current deployed set/version and cannot be substituted by either earlier issuance-off schema.
- Each lifecycle receipt binds schema version; signed release-set digest; deployed runtime/C digests and schema; hashed client identity, immutable client class, mode, key thumbprint, and every generation; idempotency replay outcomes; token-expiry/reacquisition proof; mode-specific readiness/mail assertions; cross-client denials; billing/provider cleanup states; retired alias/domain-claim evidence; zero-live-resource query results; workflow/run identity; timestamps; and a redaction attestation. Missing cleanup fields, cross-mode substitution, or a different release set makes verification fail.
- After lifecycle cleanup, use fresh onboarding tokens and new idempotency keys to create an explicit managed generation 3 and custom generation 2 for soak. Bind the two immutable `synthetic` client IDs in protected release configuration and the drill/soak evidence; normal registration cannot claim those identities or class.
- Probe both clients at least every 15 minutes for at least 24 uninterrupted hours. A missing interval longer than 30 minutes, any failed probe, digest/schema/policy mismatch, cross-tenant anomaly, or unresolved alert resets the continuous-window start.
- Every observation verifies and records the same release-set digest, deployed runtime/C digests, Core 29/Cloud 10, both transition receipts, and policy hash before calling MCP.
- Exercise immutable SDK 0.2 and exact unpublished SDK 0.3 during the same window.
- Produce signed soak evidence with `gate=pilot|broader`, release set, deployment time, hashed client identities, every expected/observed interval, successful run IDs, zero unresolved failures/alerts, and continuous start/end. Its schema/verifier recomputes cadence/coverage rather than trusting claimed timestamps. Promotion and activation accept only protected `soak_evidence_run_id` plus `soak_evidence_sha`, verify producer identity/attestation and the same release-set SHA, and reject direct timestamps or hand-authored summaries. Never record keys, assertions, tokens, DNS credentials, message bodies, or attachment bytes.
- Pilot evidence covers at least 24 uninterrupted hours and has `continuous_end` no older than 30 minutes when used. After the one external pilot activation commits, keep both synthetic canaries running and emit a distinct `gate=broader` artifact bound to the pilot activation audit ID/timestamp. It must cover at least 168 uninterrupted hours beginning no earlier than that pilot commit (or the latest reset) and also end within 30 minutes of broader activation. The original 24-hour artifact can never satisfy the broader gate.
- Any runtime, control-plane, SDK, contract, policy, schema, or release-set change resets the clock.
- Keep both synthetic clients/keys and their canaries operating through the full fresh 168-hour post-pilot broader-activation window; rotate a generation only through the same close/cleanup/reconnect assertions. In an outer finalizer after the broader gate is approved/completed or rollout is explicitly aborted, close custom then managed generations, revoke both keys/clients, and reassert zero live lifecycle/resources while every managed alias tombstone and confirmed audit row remains.

#### 10.5 Publish the exact tested bytes

Publication is split across repository authority. The protected Cloud post-soak workflow:

1. Verifies release-set signature, exact deployed digests/schema, both lifecycle receipts, and at least 24 uninterrupted hours of matching two-client soak evidence.
2. Re-runs both modern contracts and immutable SDK 0.2; refuses on any cross-tenant finding, alias resurrection, provider orphan, stuck deprovisioning/cancellation, compatibility failure, policy mismatch, unexplained 5xx, memory breach, or unresolved alert.
3. Emits a signed promotion request binding the release set, candidate run/receipt, exact OSS SHA, frozen semver, manifest/assets and OCI index/platform digests. A GitHub App installed only on nerve-oss, with only Actions write for dispatch, passes that request run ID/SHA to the OSS workflow; Cloud's ordinary `GITHUB_TOKEN` never receives cross-repository write authority.
4. The OSS workflow verifies the Cloud request and release set with its own pinned producer trust, then uses its repo-scoped `contents:write`, `packages:write`, and `id-token:write` permissions to create the exact source tag/Release, copy the already-tested manifest digest under the semver tag, and attach the exact candidate assets—without a build step. The old generic `push: tags: v*` rebuild path is removed/disabled before tag creation. Exact pre-existing bytes are an idempotent retry; any mismatch fails closed. OSS emits a signed promotion receipt.
5. Cloud verifies the OSS receipt and public tag/Release/OCI bytes, then publishes the already-built SDK 0.3 wheel with the release-set SHA. Existing PyPI filename counts as success only if the digest matches exactly.
6. Updates `runtime.lock` to public release metadata and converges `oss-source.lock` to the same promoted OSS SHA in a non-deploying change; that update neither triggers nor authorizes rollout.
7. Is resumable across partial publication: each component counts as completed only when its source/digest exactly matches the release set. Only after OSS and SDK publication plus lock convergence verify complete, emit schema-validated signed promotion evidence binding release-set SHA, lifecycle/pilot-soak/request/OSS-receipt digests, exact source tags, GitHub Release/assets, OCI tag/digest, PyPI filename/SHA, lock update, protected workflow/run identities, and completion timestamp.

#### 10.6 Activate external clients after promotion only

- Because required-reviewer environment protection is unavailable for this private repository, `mcp2026-activation-approval.yml` is the sole approval producer. It accepts an exact canonical external-client list, release-set digest, `pilot|broader` cohort, not-before, and expiry; requires `github.actor` in the audited repository allowlist of activation approvers; emits a schema-validated GitHub-attested artifact binding those values plus approver/run identity; and cannot activate anything itself.
- The separate protected `mcp2026-activate-clients.yml` workflow runs under the shared deploy lock and accepts the exact pending external `client_id` list, promoted release-set digest, only `promotion_evidence_run_id` plus SHA, only `soak_evidence_run_id` plus SHA, and only `approval_run_id` plus SHA. It first verifies promotion and the same release set/publication, then verifies a fresh soak artifact whose `gate` equals the requested `pilot|broader` cohort, and finally verifies approval producer/actor allowlist and exact set/release/cohort/time equality. It atomically consumes the approval digest with activation in `oauth_activation_approvals`; no wildcard, raw evidence fields, or class mutation is allowed. Exact retry after a committed activation is idempotent; replay for another set/release/cohort is rejected.
- It queries durable client classification/activation state and verifies issuance still enabled for the exact current release set, current digests/Core 29/Cloud 10, both transition and drill receipts, gate-appropriate fresh cadence-valid soak evidence, green synthetic canaries, and zero blockers before an audited targeted DB activation.
- Activation does not edit runtime.lock, set Fly secrets, redeploy, or activate any unlisted client.
- When zero external clients are active, the input list must contain exactly one preapproved pilot ID and match its one-use approval; a multi-ID first cohort is rejected even after 24 hours.
- Once any external client is active, every additional activation requires the distinct fresh `gate=broader` artifact covering 168 continuous post-pilot hours plus a separate approval that was not used for the pilot. Any release-set component change, canary gap/failure, issuance disable, or policy/schema drift resets the broader window.

### Success Criteria

#### Automated verification

- [ ] Two distinct clients/modes/keys/orgs/generations are exercised and cross-client/mode access fails.
- [ ] Global issuance is off through C deployment, can be enabled only for the exact release set after both synthetic registrations, and concurrent enable/disable/exchange tests produce signed receipts with no issuance across the off linearization point.
- [ ] Both lifecycle drills pass, reconnect proves permanent alias non-reuse, and finally cleanup leaves no live resources.
- [ ] Every scheduled run verifies the same release set and production digests.
- [ ] SDK 0.2 and the exact unpublished 0.3 wheel pass throughout soak.
- [ ] SDK publication cannot be manually dispatched or run without matching production/soak evidence.
- [ ] Public runtime assets and PyPI contain exact tested bytes without rebuild.
- [ ] Cloud cannot mutate OSS with its ordinary token; the least-privilege App can only dispatch the pinned OSS workflow, which publishes tested bytes without triggering the legacy tag rebuild path and returns a verified OSS receipt.
- [ ] Activation before complete attested promotion, or with substituted/incomplete promotion evidence, fails even when soak and approval are otherwise valid.
- [ ] Lifecycle receipt validation rejects a missing cleanup assertion, wrong mode/client/generation, release-set substitution, or unsigned producer identity.
- [ ] Soak validation rejects any gap over 30 minutes, failed probe, unresolved alert, or component drift and resets the interval start.
- [ ] Activation rejects a 24-hour/pilot artifact for broader use, any broader window beginning before the pilot commit, stale `continuous_end`, pilot-audit substitution, a post-pilot gap, or release component drift.
- [ ] Pre-soak activation, a multi-ID initial cohort, a second client before 168 uninterrupted post-pilot hours, unapproved actor, expired/not-yet-valid approval, approval substitution, and approval replay for another set/release/cohort all fail; exact committed retry is idempotent and activating one explicit approved pilot cannot activate another.

#### Operator verification

- [ ] Dashboards stay green for auth, replay, lifecycle latency/leases, provider/billing cleanup, policy decisions, complaints/bounces, 5xx, compatibility, and memory shedding.
- [ ] B/C, release-set, R0, historical 0009, pre-0010 release-set issuance-off, Cloud 0010, Phase-10 issuance-control enable/latest post-disable, lifecycle, soak, and promotion receipt digests are recorded in the runbook.
- [ ] No non-synthetic client was active during candidate deployment or soak.

### Soak and exit gates

- Minimum 24 uninterrupted hours on the unchanged release set before the first external client.
- A distinct fresh `gate=broader` artifact must cover at least 168 uninterrupted hours after the pilot activation commit and end no more than 30 minutes before broader activation.
- Zero cross-tenant findings, duplicate generations, duplicate/resurrected aliases, provider orphans, lost idempotency results, or stuck cancellation.
- Zero unexplained compatibility failures, 5xx increase, or memory-budget breach.
- Legacy traffic remains measured by protocol/client version; removal remains a separately approved future plan.

## Testing Strategy

### Unit tests

- OAuth assertion validation, optional/matching `client_id`, RFC 7638 kid/global thumbprint uniqueness, `retain_until=exp+skew`, exact standard/versioned error mapping, numerical pre-decode limits, scope/generation selection, complete token claims, and JWKS rotation.
- Canonical request hashing; lease/version/state transition legality; no `failed` escape state.
- Alias/domain canonicalization, namespace trigger predicates, and collision retry.
- Tool schemas, error partition, principal profiles, protocol routing, complete waiting results, and JSON/SSE parsing.
- Reply-recipient selection, fail-closed autonomous policy, paid/suspension projection, and no inbound-derived trust.
- Stripe subscribe/close fencing, mandate/cap validation, cancellation request/idempotency/result classification, monotonic event projection, and late-result compensation.
- Transition/release/mirror/baseline/issuance/lifecycle/promotion-request/OSS-receipt/pilot-soak/broader-soak evidence schemas, manifest equality, injected release-set membership, cadence coverage, activation cohort rules, and compatibility report parsing.
- Memory reservation calculations and cleanup.

### PostgreSQL integration tests

- Cloud 0008 to 0009 preflight/backfill/transition and refusal-style down for every protected table class; release-set-bound Cloud 9 to forward-only 10 preflight, serialization, exact B/receipt binding, resume, and refusal cases.
- Provider inventory/quarantine under a writer fence plus explicit adopt/delete receipts.
- Alias/catch-all triggers, pre-existing platform-inbox permanent backfill, managed namespace ownership, and retired-address non-reuse through every Core write path.
- Global canonical-domain claims across legacy/autonomous create/verify/delete/GC, including expired-pending takeover and unknown-provider-outcome races.
- Client/key/billing-profile lifecycle, concurrent assertion replay through the skew boundary, release-set-bound issuance enable/disable versus exchange, token issuance versus close/revoke, and durable rate buckets.
- Atomic org/trial/usage/flag graph creation.
- Concurrent generation/idempotency and workflow-version/lease fencing.
- Grant/alias/inbox atomicity, RLS, provider compensations, and tombstone preservation.
- Stripe billing workflows, subscribe-versus-close in both orders, delayed materialization/webhook compensation, cancellation/readback confirmation, ambiguous retry, `requires_action`, and reconnect non-transfer.
- Multi-Machine send limits with namespaced deterministic usage events, shared reserve/reconcile locks, and no lost update.
- Outbox enqueue/claim/provider-start versus complaint/close barriers, retained disabled inbox/FK semantics, and autonomous tombstone refusal before terminal drain.
- Reconciler claim/takeover/cleanup plus compatibility refusal before mutation.

### Protocol and compatibility tests

- Official MCP 2026 server/client conformance and OAuth extension snapshot at pinned revisions.
- Golden client-credentials-only RFC 8414/Errata 7793 metadata plus token-error-extension fixtures.
- Legacy 2025-11-25 golden response fixtures.
- Exact PyPI 0.2.0 wheel against vNext.
- SDK 0.3.0 pinned PEM/private-key JWT, JSON/SSE modern mode against vNext, legacy mode against v0.0.17 on Core 28, and legacy mode against R0 on Core 28/29.
- Exact request `_meta`, metadata/form/cache/challenge/OAuth-error/token-claim golden tests and capability-negotiation failure.
- Route matrix for protocol, credential, absent/allowed/hostile Origin, principal, paid/domain policy, attachment, and response media type.
- B/C compatibility matrix and C→B→C rollback fixture with all lifecycle/retry states; rollback below vNext refuses any non-closed generation, including `active`.

### Security tests

- Algorithm confusion, hostile JWK fields, key-type mismatch, permanent cross-client duplicate thumbprint, mismatched derived kid, and retired-key reuse.
- Oversized/chunked/false-Content-Length bodies, assertion segments, IDs, scope sets, org names, domains, and local parts reject before decode/crypto/DB.
- Wrong issuer/audience/resource and direct-host replay.
- Onboarding-to-org privilege escalation, mixed scopes, and stale N token attempts against N+1 for status/verify/close plus org/generation override attempts.
- Hostile/null/lookalike Origin with valid credentials and absent Origin without native auth.
- Internal delegation redirect/SSRF, replay, stale timestamp, and body substitution.
- Cross-org onboarding status, thread, inbox, domain, and attachment access.
- Idempotency-key payload substitution.
- Caller-controlled approval and reply-recipient manipulation.
- Spoofed/self inbound and forged Authentication-Results cannot change compose authorization.
- Log redaction tests using synthetic recognizable secrets.

### Production contract tests

- Real OAuth discovery through auth.nerve.email and runtime PRM.
- Two isolated client identities and mode/cross-tenant denial.
- Managed mailbox inbound/read/reply/compose-deny, principal-bound subscription through the preauthorized canary mandate, close/reconnect, retained disabled inbox, outbox drain, and alias tombstone.
- Custom DNS verification/inbound/compose/delivery.
- Subscription cancellation before closed and no entitlement transfer.
- Both SDK wheels, JSON/SSE, issuance receipts, Fly mirror/baseline identity, OSS no-rebuild receipt, one pilot, fresh 168-hour broader evidence, and activation refusal/success.

## Performance and Reliability Considerations

- Modern MCP is stateless and can scale after the legacy-session constraint is removed.
- Cache AS JWKS by HTTP freshness with bounded stale use only for already-known keys; unknown kid forces one refresh and then fails closed.
- Token endpoint and onboarding mutations use durable per-client buckets, not process memory.
- Index onboarding work by state, lease expiry, and next retry; reconciler claims bounded batches with SKIP LOCKED and CAS.
- Keep provider/Stripe calls outside transactions and reconcile unknown outcomes by stable provider identity plus workflow fence.
- Acquire global issuance and client locks first when that authority exists. Any operation that can mutate an inbox row observed by the Cloud canonical-identity/status triggers then takes the schema boundary when applicable, canonical-domain, sorted org-resource, org-policy, billing, and row locks in that order; this includes ordinary disable/delete/reactivate and autonomous cleanup. Operations that do not touch such an inbox retain their documented narrower prefix of this order. Never hold transaction locks across network I/O.
- Global canonical-domain claims make ownership contention bounded to one canonical name rather than an org-wide lock.
- Security-critical send policy uses the shared epoch at enqueue, claim, and provider-start; do not cache it across requests or provider operations.
- Namespaced matching usage events plus the shared counter-row lock keep generic reconciliation linear in the indexed org/period/meter slice and prevent collisions or lost updates.
- Use explicit timeouts shorter than the MCP request deadline.
- Avoid copying raw provider responses or attachment bodies into audit logs.
- Reserve modern body memory for official SDK double buffering and decoded base64 before allocation.
- Instrument onboarding latency by state rather than holding an HTTP request open during DNS propagation.

## Observability and Operations

Add metrics with bounded labels:

- mcp_requests_total by protocol, method, profile, and outcome.
- mcp_protocol_mismatch_total.
- mcp_origin_rejections_total by bounded reason.
- mcp_response_media_total by JSON or SSE.
- oauth_token_exchange_total by token_use and outcome.
- oauth_issuance_control_total by enable, disable, refusal, and receipt outcome.
- oauth_assertion_replay_total.
- oauth_jwks_refresh_total by outcome.
- onboarding_transitions_total by mode, from_state, to_state.
- onboarding_stuck_total by state and error_code.
- onboarding_lease_takeovers_total by operation and outcome.
- onboarding_stale_provider_results_total by operation and compensation outcome.
- domain_claim_conflicts_total by owner_kind pair.
- provider_domain_quarantine_total by mismatch kind and resolution.
- managed_alias_allocations_total by outcome.
- managed_alias_guard_rejections_total by write path and reason.
- subscription_cancellation_total by requested, confirmed, ambiguous, and failed.
- subscription_projection_total by provider status, evidence decision, and stale-event refusal.
- outbound_policy_decisions_total by tool and reason.
- outbound_suspensions_total by source.
- outbox_policy_fence_total by claim refusal, queued terminalization, provider-start, drain, and readback outcome.
- outbound_counter_reconcile_drift_total by meter.
- internal_delegation_requests_total by operation and outcome.
- mcp_memory_rejections_total by reason.
- schema_compatibility_refusals_total by binary/schema.
- release_set_verification_total by outcome.
- runtime_mirror_verification_total and legacy_runtime_baseline_verification_total by outcome.
- oss_promotion_handoff_total and soak_gate_verification_total by bounded stage/outcome.

Structured logs include request/replay/onboarding IDs, client ID hash, generation, org ID, and bounded error code. They exclude private/public JWK bodies, assertions, bearer tokens, HMAC values, DNS credentials, message bodies, and attachment bytes.

Audit records are required for client/key/billing-profile mutation, issuance enable/disable receipts, token issuance metadata, onboarding/lease transitions, provider-domain quarantine/adopt/delete, domain claims, platform-inbox backfill, alias reservation/retirement, outbox provider-start/drain, evidence changes, suspension clear, Stripe subscription/cancellation, close/reconnect, mirror/baseline/release-set/OSS promotion, soak gates, and client activation.

Alerts:

- OAuth replay or invalid-signature spike.
- Onboarding rows past retry SLA.
- Provider ambiguity/deprovisioning backlog.
- Lease churn or stale-provider compensation failure.
- Alias allocation collision or namespace-guard rejection spike.
- Domain claim/release backlog.
- Subscription cancellation pending beyond SLA.
- Provider-started outbox operation unresolved beyond its lease/readback SLA.
- Complaint or suspension spike.
- Usage counter/event drift.
- Runtime-control-plane delegation failures.
- MCP memory shedding or 5xx regression.
- Any schema compatibility, release-set, policy-hash, binary, or Machine digest mismatch.
- Any issuance-control/release-set mismatch, Fly mirror/baseline mismatch, unauthorized OSS promotion attempt, or stale/substituted soak evidence.

## Migration and Deployment Notes

### Schema ownership

- Cloud 0009 and forward-only Cloud 0010 are authored only in nerve-cloud. The signed 0009 transition evidence remains historical and immutable; 0010 receives a distinct release-set-bound transition and receipt.
- Core 0029 is authored in nerve-oss first, mirrored byte-for-byte to Cloud, and advances `CORE_SCHEMA_HASH`. It owns only `org_outbound_policy_state` and the nullable outbox provider-fence columns; Cloud must not carry a divergent copy.
- Cloud 0009 owns lifecycle tables and the first alias/catch-all guards. Cloud 0010 owns the hosted canonical active-inbox index, new-write canonicality guard, permanent platform identity, and replacement managed-namespace trigger over the existing Core inbox/domain tables in the same database. Shared Store behavior remains OSS-first and does not depend on a Cloud-created SQL function.
- Shared Go/auth/store files follow OSS-first exact-mirror discipline where they truly exist in both repositories.
- `oss-source.lock` alone selects shared-source authority during development. `runtime.lock` selects the ordinary released production artifact; the pre-release R0 bridge and runtime candidate are selected only from their signed final-release-set members/receipts while `runtime.lock` remains on v0.0.17, then promotion converges it to the published runtime. `runtime-candidate.lock` is non-deploying evidence consumed only by release-set construction.
- internal/mcp remains OSS/runtime-only.
- The exact outbound policy bytes and declared shared auth/store/contract files move OSS-first and are present in both manifests.
- Cloud-only billing, client registry, onboarding, subscription cancellation, alias/domain claim, and evidence files do not move into OSS.

### Expand and contract

- Artifact A `[8,9]` crossed the boundary in the dedicated deploy-before-migrate transition and was the temporary floor; its bytes and evidence are now historical and immutable.
- Artifact B is built from final feature code with Core `[28,29]` and Cloud `[9,10]` and proven across both Cloud states. It is recorded before 0010, becomes the permanent rollback floor when 0010 commits, and until that commit may return to exact A on Core28/Cloud9.
- Before Core 0029, the protected bridge workflow builds R0 as a distinct digest from pinned v0.0.17 source plus the exact allowlisted `[28,28]`→`[28,29]` compatibility-window patch. Production moves v0.0.17→R0 while Core is 28 and proves legacy equivalence.
- After every durable control-plane role is B and every runtime Machine is R0, the dedicated workflow applies Cloud 0010 and emits its receipt; only then does B's migrate binary apply additive Core 0029. The migration gate proves preserved legacy SQL/data behavior and runs the actual R0 binary on Core 29 before the runtime candidate is deployed.
- Artifact C contracts the same behavior to Core `[29,29]` and Cloud `[10,10]`; web, reconciler, and migrate all reject Core 28 or Cloud 9 before side effects.
- Cloud 0009 down refuses after durable data. Production recovery uses forward fixes/restores, not casual down migration.
- Cloud 0010 down is unconditionally forbidden; once its receipt exists, B at Cloud 10 is the permanent control-plane recovery floor.
- Core 0029 down refuses after any autonomous epoch or provider-fence evidence. Production recovery likewise uses a forward fix/restore; B remains compatible with Core 29.

### Runtime promotion

- Build runtime/SDK candidates without public tags and bind them with A/B/C, schema, policy, contract, and the preselected final runtime semver in one signed release set.
- Mirror and deploy the candidate by immutable digest, with a signed pre-release Fly-mirror receipt. The protected post-soak Cloud workflow hands a signed request to the least-privilege OSS-side no-rebuild publisher, verifies its receipt, publishes the exact wheel, and emits promotion evidence without rebuild or manifest mutation.
- Candidate and public runtime locks are separate formats and verifiers; moving from `runtime-candidate.lock` to public `runtime.lock` metadata is non-deploying, and promotion converges `oss-source.lock` to the released OSS SHA.
- Editing runtime.lock never activates a client or authorizes a rollout.

### No staging environment

Preserve the existing decision to use production-snapshot rehearsal plus two isolated production synthetic clients rather than a nominal staging system that cannot exercise production-shaped Resend, Stripe, DNS, and attachment state.

## Rollback Matrix

| Component | Cloud 8/Core 28 before transition | Cloud 9/Core 28 before 0010 | Cloud 10/Core 28 after 0010 | Candidate Core 29/Cloud 10 | After external activation |
|---|---|---|---|---|---|
| Control plane | Cloud `[8,8]`, Core `[28,28]` until every role is A | A remains current until B installs; then exact B→A remains possible before 0010 | B Core `[28,29]`/Cloud `[9,10]` is the forward-only floor; A is forbidden | C Core `[29,29]`/Cloud `[10,10]` normal; B is the only rollback floor | Same C→B rollback; never A/Core28/Cloud9 |
| Runtime | v0.0.17 while issuance is off | v0.0.17 remains current until R0 installs; then exact R0→v0.0.17 remains possible before Core29 | R0 `[28,29]`; v0.0.17 remains possible only until Core29 | Runtime candidate `[29,29]`; R0 is the only runtime rollback floor | Rollback below vNext requires disable/drain then R0; v0.0.17 is forbidden |
| Schema | Cloud8/Core28 | Cloud9/Core28 | Cloud10/Core28; 0010 down forbidden | Cloud10/Core29 | Cloud10/Core29; forward fix or restore only |
| SDK 0.2.0 | Supported | Supported | Supported | Supported and canaried | Supported until separate retirement plan |
| SDK 0.3.0 | Unpublished candidate | Unpublished | Unpublished | Exact release-set wheel | Published exact bytes; legacy email fallback only below vNext |
| Alias/domain claims | None before 0009 | Permanent registry/claims | Canonical identity/namespace evidence retained | Never delete/reuse/bypass | Same invariant |
| Billing cleanup | None | Durable workflows retained | B reconciles the same state | B and C reconcile the same state | Never close while create/cancellation outcome is unconfirmed |
| Outbox shutdown | Legacy behavior | Durable policy evidence | B preserves it | B and C cancel queued and drain/readback starts | Same barrier; no cascading inbox deletion |
| Client activation | None | Issuance off/no client active | Issuance off/no client active | Synthetic clients only | Explicit post-soak client list only |

Before Cloud 0010 commits, the only predecessor rollback is exact B→A on Core28/Cloud9; before Core 0029, exact R0→v0.0.17 is also permitted while issuance remains off. Once 0010 commits, recovery retains Cloud10 and B; once Core0029 commits, v0.0.17 is forbidden. Preferred steady rollback is C→B with schema unchanged. Before any runtime rollback below vNext:

1. Atomically disable global M2M issuance through the protected Phase-10 disable entry point under the common deploy/global issuance lock, then verify its fresh post-disable issuance-control receipt against the exact current release set, prior/new control version, C/runtime digests, Core29/Cloud10, and producer. Reject the historical A and pre-0010 issuance-off receipt formats, and disable all further client activation.
2. Enumerate every non-closed M2M generation, including `active`, under the common lifecycle lock. For each one, run audited close or `revoke-client`, revoke email authority, and retain its onboarding-only polling path when the client itself is not revoked.
3. Keep B running until every enumerated generation is `closed`, every subscription-create/payment/cancellation workflow, outbox provider-start, and domain-claim release is terminal, and signed drain evidence proves the enumeration did not change. Refuse rollback while any generation or cleanup barrier remains nonterminal.
4. Do not delete Cloud 0009/0010 state, either transition receipt, alias tombstones, domain claims, billing workflows, or evidence history.
5. Invoke the dedicated bridge rollback workflow with only the final release set, fresh Phase-10 post-disable issuance-control receipt, and lifecycle-drain receipt. It derives and deploys embedded R0, verifies every Machine digest and its `[28,29]` startup window, and keeps B. State clearly that existing legacy org email may work while autonomous onboarding is unavailable. The workflow must reject the historical/pre-0010 issuance-off formats, v0.0.17, or any raw image/tag input.

Core stays at 29 and Cloud stays at 10 throughout C→B or below-vNext runtime rollback. R0 is the only legacy-behavior runtime authorized on Core 29, and canonical-identity/provider-fence evidence is never removed as part of rollback.

## Definition of Done

- [ ] Every Phase 0 proof gate is real and green.
- [ ] Artifact A crossed Cloud 8→9 through the dedicated deploy-before-migrate workflow and produced a signed receipt.
- [ ] Artifact B is recorded as the designated feature-complete Core `[28,29]`/Cloud `[9,10]` post-0010 rollback floor and becomes permanent only when forward-only 0010 commits.
- [ ] Cloud 0010 is applied only by the release-set-bound B transition after the immutable 0009 and fresh release-set issuance-off evidence verify, and its independent signed receipt binds both predecessor receipt digests and proves the exact pre/post state without a release-set self-cycle.
- [ ] Artifact C runs Core `[29,29]`/Cloud `[10,10]`; web, reconciler, and migrate independently refuse Core 28 or Cloud 9 before side effects.
- [ ] Core 0029 is applied only after B is the durable control-plane floor and R0 is the deployed runtime floor; the runtime candidate and C require `[29,29]`, B and R0 remain `[28,29]`, and actual R0 legacy behavior is proven on Core 28 and additive Core 29.
- [ ] auth.nerve.email serves correct public metadata/JWKS/token behavior.
- [ ] Client-credentials-only metadata passes the pinned consumers with the Errata 7793 omission; numerical request limits, versioned error extension, permanent key thumbprints, and JTI skew retention are exact and tested.
- [ ] A pre-registered private-key JWT client obtains separate generation-bound onboarding and org tokens without a human tenant flow, including maintenance after initial token expiry.
- [ ] Token issuance is linearized with close/revoke: close leaves no email authority but permits own-generation onboarding polling, while `revoke-client` makes both token kinds unusable and no path orphans a generation.
- [ ] Phase-10 enable and disable evidence binds the exact release set, deployed C/runtime/Core29/Cloud10 identity, both transition receipts, and a strictly increasing durable control version; rollback accepts only a fresh post-disable receipt and rejects both earlier issuance-off formats.
- [ ] MCP 2026-07-28 passes pinned conformance, JSON/SSE tests, capability negotiation, error partition, and Origin/auth matrix.
- [ ] Published SDK 0.2.0 remains compatible with the hybrid route.
- [ ] SDK 0.3.0 is built once, tested in both response modes, soaked in production, and published by exact SHA without rebuild.
- [ ] SDK 0.3.0 exposes the pinned PEM/private-key JWT API with derived RFC 7638 kid and exact PyJWT/cryptography dependency locks.
- [ ] Managed mailbox namespace is DB-protected against create/reactivate/catch-all/direct SQL and aliases are never recycled.
- [ ] Legacy and autonomous custom-domain flows share one canonical claim/lock; activation and release require proven provider state.
- [ ] Provider inventory/quarantine and platform-inbox snapshot/backfill prove no provider-only domain or pre-existing platform address can be misclaimed.
- [ ] Lease/version fencing and compensations prevent stale provider results from resurrecting closing resources.
- [ ] Autonomous policy is fail closed; compose unlocks only by live owned domain or confirmed paid subscription, never inbound mail.
- [ ] Every durable outbound meter has matching idempotent usage events with an org/tool/meter-namespaced replay ID; reserve and reconcile share one counter lock, survive generic reconciliation, and cannot collide or lose updates.
- [ ] Complaint/bounce suspension overrides stale token scope, fences new claims/provider starts, terminalizes queued work, and drains/readbacks earlier provider starts before close.
- [ ] Close immediately disables authority, confirms generation-owned Stripe cancellation and provider cleanup, then reconnect creates a clean generation with no entitlement transfer.
- [ ] Runtime candidate/Fly mirror, historical v0.0.17 baseline, distinct R0 bridge, A/B/C, all shipped mutating binaries, SDK, contract, policy, schema, transition evidence, and CI identities are bound by verified signed provenance without a self-hash/rebuild cycle.
- [ ] Two isolated synthetic clients pass managed/custom drills, signed lifecycle receipts, cross-client denials, final cleanup, and cadence-valid unchanged 24-hour pilot soak; they continue canarying through a distinct fresh 168-hour post-pilot broader window.
- [ ] Global issuance remains off until exact C/synthetic proof and has signed enable/disable receipts. Tags, OSS no-rebuild release, PyPI publication, and exactly one first external activation occur only through matching evidence; every additional activation requires fresh broader-soak evidence plus separate approval.
- [ ] Rollback below vNext refuses until every M2M generation, including active ones, is closed and all billing/domain/provider cleanup is terminal.
- [ ] Rollback floors, compatibility commands, alerts, billing semantics, and secret-redaction guarantees are documented.
- [ ] No unresolved product or architecture decision remains in this plan.

## References

### Repository sources

- deploy/cloud/runtime.lock
- .github/workflows/runtime-deploy.yml
- .github/workflows/deploy.yml
- .github/workflows/publish-python-sdk.yml
- .github/workflows/cloud-e2e-smoke.yml
- .github/workflows/cloud-e2e-matrix.yml
- internal/startup/migrations.go
- internal/auth/verifier.go
- internal/store/store_tokens.go
- internal/store/store_billing.go
- internal/billing/stripe.go
- internal/cloudapi/handler_billing.go
- internal/store/outbox.go
- internal/emailtransport/outbox_worker.go
- internal/store/org_domains.go
- internal/store/inboxes_manage.go
- internal/featureflags/resolver.go
- internal/reconcile/service.go
- internal/cloudapi/handler_orgs.go
- internal/cloudapi/handler_domains.go
- internal/cloudapi/resend_webhook.go
- internal/store/email_tenancy.go
- internal/store/migrations/core/0024_email_tenancy.sql
- internal/store/migrations/core/0026_feature_flags.sql
- internal/store/migrations/core/0029_outbox_policy_fence.sql
- internal/store/migrations/core/0002_runtime_tenanting_and_entitlements.sql
- internal/store/migrations/core/0007_tool_idempotency.sql
- sdk/python/src/nerve_email/client.py
- thoughts/shared/plans/2026-08-02-inbound-events-and-attachments.md
- thoughts/shared/plans/2026-08-05-service-token-tenant-boundary.md
- thoughts/shared/plans/2026-08-05-abrolia-phase-2-4-cleanup-contract.md

### Official MCP sources

- https://modelcontextprotocol.io/specification/2026-07-28/changelog
- https://modelcontextprotocol.io/specification/2026-07-28/basic/versioning
- https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization
- https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http
- https://modelcontextprotocol.io/specification/2026-07-28/server/tools
- https://modelcontextprotocol.io/extensions/auth/oauth-client-credentials
- https://github.com/modelcontextprotocol/ext-auth/commit/ce15435bf4e35a0ec972dd7cd8ce4c81d609cc3e
- https://github.com/modelcontextprotocol/go-sdk/tree/v1.7.0
- https://github.com/modelcontextprotocol/go-sdk/blob/v1.7.0/docs/protocol.md
- https://github.com/modelcontextprotocol/conformance
- https://github.com/modelcontextprotocol/conformance/commit/81eb1c3edaed87d7fd585d7b80186da7a2960660
- https://www.rfc-editor.org/rfc/rfc6749.html#section-5.2
- https://www.rfc-editor.org/rfc/rfc7523.html#section-3
- https://www.rfc-editor.org/rfc/rfc7638.html#section-3
- https://www.rfc-editor.org/rfc/rfc8414.html#section-2
- https://www.rfc-editor.org/errata/eid7793
- https://pypi.org/project/PyJWT/2.13.0/
- https://pypi.org/project/cryptography/49.0.0/

### External provider sources

- https://docs.stripe.com/api/subscriptions/cancel
- https://docs.stripe.com/billing/subscriptions/cancel
- https://resend.com/docs/api-reference/emails/retrieve-received-email

## Enhancement History

### Revision 21 — 2026-08-23

**Trigger:** Revision 20 still had no organization-level projection for the
trusted complaint and hard-bounce observations already persisted by the signed
Resend webhook. Recipient suppression alone could not revoke autonomous
outbound authority, and the transition-bundle verifier also used predictable
global `/tmp` files that made concurrent evidence verification racy.

**Disposition:** Add a Cloud-only abuse projection Store boundary plus a
nil-by-default webhook dependency seam. The Store re-resolves the exact
persisted outbox event under tenant RLS and the shared reconciliation-resource
then org-policy lock order; a
complaint immediately activates `abuse_suspension`, while hard bounces use
durable UTC-day attempts and the policy's exact 20-attempt/5-percent threshold.
Observation replay, evidence, suspension flag, policy epoch, and queued-row
terminalization are transactional. The exact signed external event identity
also deterministically selects one timeline row, so a failed projection retry
cannot duplicate audit rows or customer webhook fan-out. Operator clearance
revokes only the active complaint and hard-bounce evidence minted from signed
webhook observations, preserves all history, and clears the flag under the same
epoch only when no stronger suspension authority remains. Client-revocation
evidence and a deprovisioning or closed onboarding state therefore dominate an
abuse-clear request. PostgreSQL tests cover threshold boundaries, tenant/event
identity, rollback, replay, Core-28 inertness, clearance dominance, complaint
versus enqueue in both commit orders, and deterministic serialization with the
OAuth-authority reconciliation-resource lock. Signed-webhook tests cover complaint, threshold,
replay, retryable projection failure, and the nil dependency. The MCP runtime
contract separately reuses one already-issued generation principal
across the live policy transition and proves compose disappears from
`tools/list` while a cached direct `tools/call` fails through the policy gate.
The release evidence verifier now compares process-local normalized inventories and a
concurrent distinct-bundle regression rejects the former shared-file design.

The same Store boundary now includes a bounded drift repair that re-derives
the flag only from active deny evidence or deprovisioning/closed lifecycle
state, rechecks under reconciliation-resource then org-policy locks, advances
the epoch on repair, terminalizes older queued work, and is inert before Core
0029. The scheduled production command consumes only its read-only drift
detector and exports a bounded metric; the repair remains unconstructed until
the same explicit abuse-policy activation is approved.

**Release consequence:** No production suspension or clearance entry point is
activated by this revision. `NewHandler` leaves the projector nil, preserving
the deployed recipient-suppression behavior byte-for-byte until explicit
production authority assigns the Store implementation. The audited clearance
exists only as a Store transaction and has no operator CLI or HTTP caller.
Activating either tenant-wide suspension or its clearance remains a separate
approval-gated trust-boundary change. Revision 20's billing-provider boundary,
revision 19's custom-domain provider boundary, and revision 15's release graph
remain unchanged.

### Revision 20 — 2026-08-23

**Trigger:** Revision 19 left the Phase 7 paid-subscription Store apply seam
and due-list without any non-test consumer. That meant bounded evidence expiry
was modeled and Store-tested, but no scheduled path could refresh or revoke it
after a missed webhook. The first scheduler test also exposed that a
process-clock observation cannot safely satisfy a PostgreSQL-time monotonic
authority fence across independently clocked hosts.

**Disposition:** Add a Cloud-only scheduled paid-readback consumer behind a
separate injected read-only provider interface. It performs exact Stripe GET
after every preflight transaction has closed, then captures `clock_timestamp()`
from PostgreSQL and applies the generation/profile/workflow/subscription CAS in
a fresh transaction. Exact generation metadata and the one price ID encoded in
the persisted create identity are mandatory and byte-exact. Qualifying active
plus `succeeded` payment state grants bounded evidence; exact absence or a
non-qualifying authoritative state revokes; transport uncertainty preserves
only still-live evidence; expiry revokes; and exact-GET provenance drift
revokes without rewriting durable subscription identity. PostgreSQL tests
cover provider I/O outside transactions, active grant, exact absence,
generation/price drift, live-proof uncertainty, proof expiry, close-wins CAS,
and nil-provider inertness; focused race, billing, reconcile, command, compile,
vet, manifest, and exact-mirror gates pass.

**Release consequence:** Revision 19's Phase 6 activation boundary, revision
18's provider receipts, revision 16's OSS authority, and revision 15's release
graph remain unchanged. `nerve-reconcile` emits the new counters but does not
inject a Stripe readback provider, so the new consumer is production-inert.
Constructing that dependency, wiring signed billing delegation, making webhook
mutation plus processed-marker atomic, adding complaint/bounce suspension, or
performing a production billing/provider call remains separately
approval-gated.

### Revision 19 — 2026-08-23

**Trigger:** Revision 18 closed the offline adoption/deletion evidence
contracts, while the shared Store already had exact legacy cleanup discovery,
claim, defer, lease-takeover, quarantine-CAS, and fairness primitives. No
Cloud reconciler consumed them, so expired/releasing provider-backed legacy
domains could retain a claim indefinitely even though the recovery state
machine was otherwise executable.

**Disposition:** Add a Cloud-only scheduled cleanup consumer behind an injected
provider interface. It claims an exact Store snapshot before any provider I/O;
for a known provider ID it validates exact ID plus canonical identity, disables
receiving, issues DELETE, and requires a final same-ID GET 404 before local
release. Initial 404 is idempotent completion, DELETE uncertainty is resolved
only by the final readback, and present/unknown outcomes retain the identity
and release the lease for retry. No-ID or quarantine-blocked work performs
bounded inventory/quarantine and defer only. PostgreSQL tests cover external
I/O after transaction release, mismatch and provider-only quarantine, exact
identity exclusion from name-only quarantine, initial/final absence, uncertain
DELETE, still-present/unknown readback, and pristine local-only cleanup.

**Release consequence:** Revision 18's receipt contracts, revision 16's source
authority, and revision 15's release graph remain unchanged. The consumer is
locally implemented and testable but production-inert because
`nerve-reconcile` does not construct its provider dependency. Supplying real
provider credentials to that interface remains an explicit approval-gated
activation; this revision performs no production provider call, deployment,
or receipt mutation.

### Revision 18 — 2026-08-23

**Trigger:** Revision 17 closed the offline explicit-adoption contract, but the
original Phase 1/6 invariant also permits an audited delete resolution. A
generic receipt SHA or DELETE 2xx cannot prove safe deletion: a quarantined
provider ID may still be referenced locally, and provider absence is
authoritative only after a final exact-ID readback returns 404.

**Disposition:** Add a distinct provider-domain deletion receipt schema,
generator, verifier, and adversarial test. Bind the exact open quarantine and
canonicalizer binary, a fenced local-reference snapshot with zero provider-ID,
active-inbox, and provider-owned-claim references, and an ordered delete/final
GET-404 observation no more than five minutes old. The future protected delete
workflow derives/rechecks snapshots under the global writer plus canonical
lock, performs network calls outside transactions, and resolves the ledger
only after exact receipt verification.

**Release consequence:** Revision 17's adoption contract, revision 16's source
authority, and revision 15's release graph remain unchanged. The offline
deletion contract may land without provider access. Real DELETE, ledger
resolution, writer enable, and historical transition changes remain explicitly
approval-gated; uncertainty leaves quarantine open and writers fenced.

### Revision 17 — 2026-08-23

**Trigger:** Final Phase 6 validation found two remaining provider-inventory
authority gaps. The historical Cloud-0009 Python preflight uses Python's
IDNA2003 codec while Store/runtime identity uses the pinned Go Lookup/UTS-46
profile, so a valid name such as `straße.de` can compare as `strasse.de` in
the preflight but `xn--strae-oqa.de` at runtime. The quarantine table also had
only an unauthenticated receipt-SHA slot: no closed schema bound the exact
open row, unbound target/claim, fresh exact-ID provider observation, protected
approver/producer, and canonical lock scope required for explicit adoption.

**Disposition:** Make `internal/domains` the sole operational inventory
identity by compiling a bounded batch helper and routing every Python
preflight domain through it. Add a separate provider-domain adoption receipt
schema, generator, verifier, and adversarial offline test. The future protected
adoption workflow derives snapshots under the global writer fence plus
canonical lock, applies only an exact snapshot/version CAS, and requires a
fresh zero-finding inventory after mutation. Receipt bytes or canonical-name
lookup alone never authorize attachment.

**Release consequence:** Revision 16's source-authority inventory and revision
15's A/R0/B/Cloud-0010/Core-0029/C release graph are unchanged. The offline
receipt contract may land and be tested without provider access. Changing the
historical production transition or constructing the adoption workflow remains
explicitly approval-gated; until then provider adoption is unavailable and any
unresolved quarantine keeps domain writers fenced.

### Revision 16 — 2026-08-23

**Trigger:** The Phase 6 source-authority audit found that the revision-15
release graph was coherent but its file inventory was not. Both manifests
omitted the lower-level `internal/emailaddr/**` package/tests and the
canonical-domain wrapper/tests, the typed legacy provider lifecycle plus
cleanup-scheduler Store API/tests had no declared OSS authority, and the
byte-identical `internal/store/store_orgs.go` was still labeled Cloud-only.
That incomplete declaration allowed the existing exact-mirror check to report
only `org_domains.go` drift while silently excluding files that determine the
same canonical claim and provider-absence security boundary.

**Disposition:** Freeze one identical inventory in both plan copies and both
sync manifests. The complete `internal/emailaddr/**` package/tests,
`domains/canonical.go` and its test,
`store_orgs.go`, `legacy_domain_lifecycle.go` and its test, and the already
shared claim/Resend/contract files are OSS-first exact mirrors. The lower-level
`emailaddr` package does not import `domains` and may use the pinned IDNA
profile only to validate already-ASCII A-labels; the higher-level canonical
wrapper exclusively owns U-label-to-A-label conversion and then delegates
ASCII validation downward. The lifecycle Store test receives an OSS-local
Cloud-9 schema fixture so the shared API can be proven without importing Cloud
migrations. `internal/mcp/**` remains intentionally OSS/runtime-only and absent
from the manifest. Provider quarantine, Cloud lifecycle orchestration,
domain/HTTP handlers, reconciliation/scheduler consumers, command wiring, and
their handler/scheduler tests remain Cloud-only; the domain handlers are
explicitly excluded ahead of the broader `internal/cloudapi/**` patch-sync
rule.

**Release consequence:** Revision 15's Cloud-10/R0/A/B/C transition and
rollback graph is unchanged. No source lock may advance merely because the old
partial manifest passes: OSS must first contain and test every newly declared
shared lifecycle byte, both manifests must match, and the complete declared
mirror must be byte-identical in Cloud. This inventory correction does not
authorize provider construction, deployment, migration, issuance, billing, or
rollout.

### Revision 15 — 2026-08-22

**Trigger:** Phase 5.1 review proved that request-only canonicalization missed valid pre-Core-0024 address spellings with outer whitespace or a trailing domain dot. Supported Store reads could miss them, writers could create a semantic duplicate, and the hosted lower-only active index did not enforce the same identity. The new forward-only Cloud 0010 also made the previous Phase 9/10 Cloud-9 contraction internally impossible, while the release-set scaffold still omitted the already-approved R0 bridge.

**Disposition:** Keep Core 0029 outbox-only and do not introduce Core 0030. OSS-first shared Store paths now use one serialized, byte-preserving canonical equivalence for every address read/create/ensure/reactivate boundary and canonicalize loaded bytes at downstream comparison/provider boundaries. Cloud 0010 owns the hosted direct-SQL backstop: a serialized global collision preflight, functional active-identity index, new-write canonicality guard, and replacement managed-namespace trigger. Arbitrary direct SQL against standalone OSS remains outside this plan; adding that guarantee requires a separately approved Core migration and a new bridge/release graph.

**Release consequence:** Preserve v0.0.17, R0, Artifact A, and the signed Cloud 0009 evidence exactly. Future B is Core `[28,29]`/Cloud `[9,10]`; future C is Core `[29,29]`/Cloud `[10,10]`; the runtime candidate remains Core `[29,29]`. Production proceeds R0, B on Core28/Cloud9, a dedicated release-set-bound Cloud0010 transition and independent receipt, Core0029, runtime candidate, then C. After 0010 commits, A/Cloud9 rollback is forbidden; C→B and runtime-candidate→R0 retain Core29/Cloud10. Release-set schema/build/verification must include R0 and the exact 0010 transition specification without hashing the post-deploy receipt back into its parent set.

### Revision 14 — 2026-08-19

**Trigger:** The first Cloud sync containing Core 0029 passed exact-mirror but failed `control-plane-artifact`: CI rebuilt the current tree as role A, so the generated manifest had Core head 29 while A's frozen compatibility window remained `[28,28]`. Widening or republishing A would destroy the already-attested transition identity.

**Disposition:** Freeze A permanently at its captured source/digest. Replace post-transition per-commit A builds with a separate `validation` artifact role whose window follows the checked-in migration head. It is local CI evidence only: no GHCR push, Sigstore release signature, deploy-pattern name, transition/release-set membership, or service startup is permitted. Offline `compatibility --json` still verifies the complete six-binary manifest and image labels.

**Release consequence:** Core 0029 exact mirrors may land in Cloud without fabricating a new A or prematurely constructing B. Production remains on the previously attested A until the explicit R0/B Phase 9 sequence; B/C remain the only release-set-required future control-plane roles.

### Revision 13 — 2026-08-19

**Trigger:** Core 0029 review checked the immutable v0.0.17 source at `a794be9f2697e0864d3a31da8f087577e9748f7e` instead of relying on the migration's frozen SQL fixture. That runtime is compiled for Core `[28,28]`, and production startup verification rejects Core 29 before any legacy SQL path can run. Revision 12's claim that v0.0.17 could serve through the migration and remain the post-Core29 rollback target was therefore false.

**Disposition:** Keep v0.0.17 immutable and historical. Introduce a separate non-semver R0 bridge built reproducibly from the pinned v0.0.17 source plus one exact allowlisted compatibility-window patch `[28,28]`→`[28,29]` and attestation wiring. The actual R0 binary must prove v0.0.17-equivalent legacy behavior on Core 28 and production startup/legacy behavior on Core 29. Frozen legacy SQL fixtures remain schema-level evidence only and make no artifact-compatibility claim.

**Release consequence:** Production now moves v0.0.17→R0 on Core 28 before B applies Core 0029. The final release set embeds both the historical v0.0.17 baseline receipt and distinct R0 receipt/digests, but only R0 is authorized for below-vNext rollback once Core is 29. The dedicated rollback workflow derives R0 from the release set, never accepts v0.0.17 or a raw image/tag, and leaves Core 29 plus provider-fence evidence intact.

### Revision 12 — 2026-08-19

**Trigger:** Completing the Phase 7.4 reserve/reconcile fixes exposed the next delivery-side Phase 7.2 tranche. The adapter-side enqueue lock is not sufficient after the transaction releases: the existing Core 0028 outbox schema cannot distinguish a merely claimed row from a provider-started operation after a worker crash, so suspension/close cannot both prevent a later start and wait for an earlier start without durable evidence. The prior “no Core migration” constraint therefore contradicted the already-approved provider-start linearization requirement.

**Disposition:** Approve one additive OSS-authority Core 0029. It adds an org-scoped monotonic policy epoch plus nullable autonomous epoch/provider-start/operation/resolution fields on outbox rows, preserves the existing outbox status constraint, leaves legacy rows null, and uses refusal-style down once evidence exists. The MCP adapter enqueue half and delivery worker half remain separate review units, but the migration, claim/provider-start CAS, suspension/close writers, reconciler recovery, and PostgreSQL race fixtures must land as one delivery-fence tranche before Phase 7.2 is complete.

**Release consequence (superseded by Revision 13):** Artifact B expands to Core `[28,29]`/Cloud `[8,9]`; the runtime candidate and Artifact C require Core `[29,29]`, with C also Cloud `[9,9]`. Revision 13 replaces the invalid v0.0.17-on-Core29 premise with the distinct R0 bridge. C→B and below-vNext runtime rollback keep Core 29; no rollback path deletes provider-fence evidence. Runtime/manifest/source locks and the final release set bind the Core 0029 head/hash.

**Closed prior risk:** Cloud PR #105 (`d95fa69354fc2cb98425ec074096b1f407c98bef`) and OSS-to-Cloud sync PR #107 (`c0e97c25bc74b8886733d6c25af3803cbc99e427`) close Revision 11's period-boundary/replay-namespace follow-up with real PostgreSQL fixtures for both lock orders and atomic source-lock advancement.

### Revision 11 — 2026-08-19

**Trigger:** The post-merge OSS-to-Cloud sync run `32190508638` compiled the OSS-first `internal/store/outbound_limits.go` against Cloud's intentionally unsynchronized `internal/store/store_usage.go` and failed because Cloud lacked `RecordUsageEventAt`. Cloud PR #102 added that compatibility method, preserved the database-clock default for ordinary events, and moved reconcile SUM/SET under a counter-row transaction with a two-connection reservation fixture. Its successful rerun then created Cloud PR #103, whose first head copied the new manifest-owned bytes but left `oss-source.lock` on the predecessor revision, so exact-mirror correctly failed closed.

**Disposition:** Explicitly authorize the Cloud-only store compatibility file and reconcile test in Phase 7.4. Preserve PostgreSQL `DEFAULT now()` for implicit event time and reserve explicit timestamps for operations that own an authoritative time. Require each generated sync PR to update the Cloud source revision and manifest digest in the same change as its exact mirrors. Admin merge `3950a4c45314bb59eef71cb6576945eb4a4f2aef` accepted the remaining concurrent `period_end` mutation race as a deferred follow-up so transport sync could proceed; the period-boundary invariant and its two-ordering test remain open success criteria rather than being reported as complete.

### Revision 10 — 2026-08-12

**Trigger:** Phase 2.1 implementation review found that the OSS-to-Cloud validation workflow still selected Go 1.23 after the module, CI, security scan, and production image moved to Go 1.25.

- Added `.github/workflows/sync-to-cloud.yml` to the Phase 2.1 OSS file list so every build/test workflow that validates the upgraded module uses the pinned Go 1.25 toolchain.

### Revision 9 — 2026-08-12

**Trigger:** Production transition run `31606012064` completed every gate, restored `domain_writes=true`, and published receipt SHA-256 `06ff49382fe1bb14fd1838c494590ce6b5dd8ebf4100f28a4d86c90478ebc63a`. Independent verification proved the receipt, provider preflight, and transition bundle Sigstore identities; Core 28/Cloud 9; exact Artifact A on two started web Machines and the stopped hourly reconciler; zero provider findings; public metadata/JWKS; and durable issuance disabled.

**Disposition:** Close the incident recovery path as required. Remove the active marker, predecessor digest pins, recovery authorizer, classifier branch, workflow step, and recovery generation mode. Retain `recovery` only in the receipt schema/verifier so the already-signed historical receipt remains independently verifiable by its SHA and Sigstore identity; all future schema-9 executions require exact Artifact A and emit `resume`.

### Revision 8 — 2026-08-12

**Trigger:** Stage-diagnostic run `31604771372` proved every public metadata response returned the exact contract and cache directive but failed at `metadata_cache`. The prior ERE used `\r?`, which BSD grep accepted as intended locally but GNU grep treated non-portably on the Ubuntu production runner.

**Disposition:** Keep the cache contract exact. Normalize only CR from curl's header file and compare the complete Cache-Control line case-insensitively with fixed-string matching for both metadata and JWKS. Preserve the writer fence until the corrected strict smoke and every subsequent receipt gate pass.

### Revision 7 — 2026-08-12

**Trigger:** The bounded production retry `31603419097` proved exact metadata and cache headers on its last attempt but did not expose which subsequent strict assertion failed, while correctly retaining the writer fence.

**Disposition:** Preserve the same 60-second wall-clock gate and strict assertions, but replace the opaque boolean chain with fail-closed named stages. Log only the safe assertion stage plus bounded public metadata/JWKS summaries so the next authenticated retry identifies the actual regional failure without exposing secrets or releasing the fence.

### Revision 6 — 2026-08-12

**Trigger:** Two authenticated recovery retries reached exact Artifact A on Cloud 0009 but the single-shot public OAuth discovery smoke failed immediately after the redeploy; an independent byte-for-byte metadata and JWKS check then passed against the same canonical TLS endpoint.

**Disposition:** Preserve the safe writer fence and retry the complete strict OAuth smoke for at most 60 seconds before writer enable. Keep exact metadata, JWKS, key-count/algorithm, TLS, and cache assertions unchanged, emit bounded last-attempt diagnostics on failure, and statically enforce that the smoke remains between post-redeploy Machine proof and fence release.

### Revision 5 — 2026-08-12

**Trigger:** The authenticated Cloud 0009 recovery run `31594799522` converged every durable control-plane Machine to Artifact A but stopped before provider inventory because the maintenance-stopped web Machines were updated without being restarted.

**Disposition:** Preserve the safe checkpoint and writer fence. Add one tested, bounded web-Machine convergence helper and require it both after the pre-transition Artifact A deploy and after the schema-9 redeploy. The helper accepts only the pinned Artifact A digest and stable durable web IDs, requires restored proxy autostart, and fails closed on missing roles, identity/configuration drift, unsafe states, or timeout. The one-time recovery identities remain until a signed transition receipt is independently verified, then leave through the already-required cleanup PR.

### Revision 4 — 2026-08-09

**Trigger:** Phase 0.5 implementation proved that the pinned ext-auth revision is a specification-only repository, while Go SDK 1.7.0's executable client-credentials consumer requires client-secret authentication and PKCE metadata and therefore cannot validate Nerve's private-key-JWT-only profile.

**Disposition:** With explicit approval, the isolated Python SDK 0.3 AS-metadata/private-key-JWT consumer and its exact dependency lock move into Phase 0. Phase 8 integrates and publicly exports that already-tested module instead of creating it after the gate it was supposed to satisfy.

### Revision 3 — 2026-08-08

**Trigger:** Fifteen P1 review groups covering OSS-first ordering, issuance activation, Origin/PS256, exact OAuth metadata/errors/input limits, replay/key identity, SDK private-key API, outbound/close linearization, inbox retention, usage races, provider preflight, autonomous billing, Stripe event ordering, Fly mirror/baseline artifacts, and executable OSS promotion/168-hour activation.

**Disposition:** All fifteen groups were material; nine were confirmed directly and six were refined after repository and standards checks. None were rejected. The `response_types_supported` prescription was refined rather than copied literally: RFC 8414 requires a nonempty value while also forbidding empty arrays, and reported Errata 7793 identifies the client-credentials-only contradiction. The plan therefore pins honest omission plus a blocking consumer-compatibility gate. The v0.0.17 artifact already existed; the missing piece was a signed release-set member and authorized rollback entry point.

**Post-rewrite validation:** Protocol, lifecycle/data, and release reviews confirmed that the revised graph has an OSS authority tranche before A, an explicit release-set-bound issuance enable before synthetic exchange, a provider-start linearization point and retained-inbox tombstone barrier, namespaced usage replay with a common reconcile lock, an authenticated autonomous billing surface at the real Stripe mutation seam, a pre-release Fly mirror, an embedded legacy baseline, an OSS-side no-rebuild publisher, and distinct fresh pilot/broader soak artifacts.

**Material changes:**

- Separated `oss-source.lock`, `runtime-candidate.lock`, and production `runtime.lock`; shared auth now lands OSS-first before Artifact A, and lifecycle-tool assertions occur only after Phase 3 registration.
- Added durable release-set-bound global issuance control with signed enable/disable receipts and barrier tests.
- Made absent-Origin authorization principal-kind based after PS256 verification, while retaining pre-auth rejection for any present hostile Origin.
- Pinned client-credentials-only metadata semantics, a versioned Nerve token-error extension, numerical public-input limits, JTI retention through skew, and permanent RFC 7638 key identity.
- Specified SDK 0.3 `PrivateKeyJWTAuth`, PEM/algorithm/kid behavior, and exact PyJWT 2.13.0/cryptography 49.0.0 dependency locks.
- Linearized enqueue/claim/provider-start with complaint/close by per-org policy epoch; queued work terminalizes and earlier provider starts drain/readback before `closed`.
- Kept generation-owned inboxes disabled and retained, added a specialized autonomous tombstone predicate, and forbade cascade deletion as shutdown.
- Namespaced global usage replay IDs by org/tool/meter/dimension and gave reserve/reconcile one counter lock and transaction.
- Added Resend inventory/quarantine under a writer fence and permanent backfill of all pre-existing platform-domain addresses.
- Replaced autonomous hosted Checkout with principal-bound `nerve_billing_subscribe` using an operator-preauthorized off-session mandate; `requires_action` fails closed.
- Moved paid projection to `internal/billing/stripe.go`/`store_billing.go`, made subscription/entitlement/evidence/flag/event application atomic, and made cancellation monotonic over late payment events.
- Added the signed pre-release Fly-mirror receipt, embedded v0.0.17 baseline rollback member, least-privilege OSS promotion handoff/receipt, and a fresh 168-hour post-pilot broader-soak gate.

**Product decision changed with approval:** Fully autonomous managed-mailbox paid compose now requires a one-time, spending-capped, SCA-complete off-session billing mandate during machine-client registration. Nerve does not automate hosted Checkout or cardholder/SCA interaction; custom-domain compose remains payment-free after full readiness.

### Revision 2 — 2026-08-06

**Trigger:** Sixteen P1 review findings covering protocol/auth lifecycle, database invariants, provider fencing, outbound authorization, billing cleanup, schema transition, provenance, and production drills.

**Disposition:** All sixteen findings were accepted; none were rejected. Finding 14 was refined: `[9,9]` existed as prose but not as a constructed, tested, independently verifiable artifact. Findings 5, 6, 8, 9, 12, 13, and 15 were strengthened after checking the implementation and release workflows.

**Post-rewrite validation:** Independent protocol, lifecycle, and release passes also closed stale-generation access, onboarding-token shutdown wording, optional MCP `clientInfo`, issuer/client-key separation, legacy domain-writer transition order, expired-claim takeover, checkout-versus-close materialization, all-binary compatibility, build-once/self-hash cycles, signed evidence delivery, executable soak cadence, promotion-before-activation, one-pilot/seven-day cohorts, and full-generation rollback drain.

**Material changes:**

- Replaced active-binding token switching with separate scope-selected, generation-bound onboarding-maintenance and org tokens.
- Linearized issuance, close, targeted revoke, and client revoke under one lock/recheck protocol.
- Replaced DNS `input_required` with ordinary complete waiting results and partitioned legacy versus modern errors.
- Pinned the full modern OAuth/Streamable HTTP/capability/Origin contract, including JSON and SSE.
- Added DB-enforced managed alias/platform-domain invariants and a canonical domain claim shared by legacy/autonomous workflows.
- Removed terminal `failed`; added leases, workflow versions, provider fencing, and compensating cleanup.
- Removed earned trust from V1. Compose now requires a live owned custom domain or confirmed paid subscription; inbound mail never raises authority.
- Made immediate generation-owned Stripe cancellation a required close barrier with no entitlement transfer or automatic prorated refund.
- Replaced the impossible steady deploy transition with a dedicated A `[8,9]` deploy-before-migrate workflow.
- Added explicit feature-complete B `[8,9]` rollback and C `[9,9]` contraction artifacts, shared compatibility checks for web/reconcile/migrate, and C→B→C rehearsal.
- Added one signed release set, pre-tag candidate flow, publish-without-rebuild gate, two mode-scoped synthetic clients, 24-hour post-deploy soak, and separate evidence-bound client activation.

**Product decision changed with approval:** The previously approved earned-trust branch of decision 3A was removed from V1 on 2026-08-06. Paid subscription and verified custom domain remain the only compose unlocks.
