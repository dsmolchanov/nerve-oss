---
title: MCP 2026 and Autonomous Agent Onboarding
status: draft
revision: 5
created_at: 2026-08-06 12:49:07 CEST
enhanced_at: 2026-08-08
repository: nerve-cloud
branch: main
commit: 99092688c3213af3cf7dc8e72cc28bd89983f6a1
runtime_baseline: v0.0.17
runtime_source_revision: a794be9f2697e0864d3a31da8f087577e9748f7e
core_schema_baseline: 0028
cloud_schema_baseline: 0008
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
| Schema ownership | One new Cloud migration, cloud/0009; Core remains at 0028 | Runtime never reads Cloud onboarding tables and delegates provisioning to control plane |
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
- Production schema heads are Core 0028 and Cloud 0008. Both binaries currently use exact compatibility windows rather than expand-compatible windows.
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
- An active inbox address is unique only while the inbox remains active. A separate permanent alias registry is required to ensure that an address is never reused after cleanup.
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
- Existing core org_feature_flags and live org-domain ownership can express the effective runtime decision without a new Core migration. Cloud 0009 will preserve the source evidence and policy history.
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
- Runtime deploy verifies a pre-existing Fly mirror but no current workflow creates the candidate mirror before release-set construction. After runtime.lock moves to vNext, v0.0.17 also loses an authorized rollback entry point unless the final release set carries a signed baseline member.
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
- No new Core migration. If implementation discovers a true need for Core 0029, stop and revise this plan before authoring it.

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
      +--> PostgreSQL: Cloud 0009 state plus existing Core resources
      +--> Resend domain API
      +--> Stripe subscription API through a preauthorized client billing mandate
      +--> scheduled nerve-reconcile

    Custom-domain branch only:
    external agent --> separate DNS MCP/API --> DNS provider

The runtime remains the public MCP resource server and OSS authority. It does not import or query Cloud 0009. A small `OnboardingProvisioner` interface in OSS delegates modern onboarding calls to a fixed control-plane endpoint. Each delegated request carries:

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

### Durable Cloud 0009 model

Create `internal/store/migrations/cloud/0009_m2m_oauth_and_onboarding.sql`. It is the only schema migration in this plan and may create Cloud-owned tables, constraints, and triggers over existing Core tables in the same database.

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
- A Cloud 0009 database trigger on inbox insert/update/address/status permits a registered address to activate only for its matching reserved/active inbox ID; `retired` always fails. This covers ordinary create, ensure, reactivate, direct SQL, and catch-all races.
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

The Cloud 0009 down migration refuses when any durable client, key, assertion, nonce, activation approval, onboarding, alias, domain claim, rate, billing-workflow, or evidence row exists.

### Existing Core state used by runtime

Core remains at 0028. Before an org email token can be issued, every autonomous organization must have explicit existing feature rows:

- `autonomous_outbound_policy=true`;
- `email_compose_org_enabled=false` initially;
- `email_outbound_suspended=false` initially.

The security decision does not use the generic cached feature resolver. For an authenticated `m2m_org`, enqueue reads one transaction-scoped org policy snapshot directly from the database. Missing rows, malformed values, or read errors deny the send. Legacy principals retain existing behavior and are identified by principal kind, never inferred from a missing flag.

Enqueue, outbox claim/provider-start, complaint/bounce suspension, operator suspension clear, and onboarding close use one per-org policy advisory lock and monotonic policy epoch. Enqueue and claim recheck the live epoch; suspension/close increments it and terminalizes all `queued` autonomous rows as `policy_revoked`. Immediately before a provider call, the worker takes the same lock, proves the saved epoch is current, and atomically records `provider_started_at` plus a stable provider operation ID. That commit is the send linearization point. A later suspension may not pretend the already-started operation was canceled: it fences all further starts and remains pending/deprovisioning until the in-flight call is terminal or provider readback resolves an unknown outcome.

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
- Give every shipped database-mutating binary a shared read-only `compatibility --json` entry point before its first listener, lease, provider call, or mutation. The immutable build manifest carries `artifact_role=A|B|C` and `release_set_required` (`A=false`, `B/C=true`), so an absent environment marker can never downgrade B/C to transition behavior. The manifest-bound binary reports its own build identity. For B/C deployments, the workflow injects `NERVE_RELEASE_SET_SHA` and `NERVE_RELEASE_SET_ENVELOPE_B64`, whose decoded maximum is 64 KiB and which contains the canonical signed release-set bytes plus offline DSSE/Sigstore verification bundle. The shared verifier caps and decodes the envelope, verifies canonical SHA and signature plus pinned repository/ref/workflow identity using vendored trust material, hashes its own executable/manifest, and proves exact role/image/manifest/binary/window membership before reporting the runtime-bound release-set identity. A SHA without the signed bytes/bundle is never sufficient, and the release-set identity is never compiled into an artifact that the set itself hashes. Environment may tighten verification but cannot disable the manifest requirement.
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
- new `schemas/mcp2026-runtime-mirror-receipt.schema.json`
- new `scripts/ci/verify_mcp2026_runtime_mirror_receipt.py`
- new `schemas/mcp2026-legacy-runtime-baseline.schema.json`
- new `scripts/release/generate_legacy_runtime_baseline.py`
- new `scripts/ci/verify_legacy_runtime_baseline.py`
- new `.github/workflows/mcp2026-capture-runtime-baseline.yml`
- new `.github/workflows/mcp2026-rollback-runtime-baseline.yml`
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
- Define one attested `mcp2026-release-set.json` pinning Cloud and OSS SHAs; a preselected final unused runtime semver; runtime candidate index and linux/amd64 digests; runtime-manifest SHA and MCP/Core windows; a verified pre-release Fly-mirror receipt and resolved Machine digest; a signed v0.0.17 legacy-runtime-baseline member; control-plane A/B/C digests and manifest SHAs; every shipped binary hash/window; Cloud migration head/hash; SDK 0.3 filename/SHA; immutable SDK 0.2 SHA; outbound-policy version/SHA; conformance commit; and producing workflow identities.
- The final release set embeds the verified transition-bundle digest and Phase 1 transition-receipt digest so A identity/provenance cannot be substituted later.
- Verify GitHub OIDC/Sigstore attestations, repository/ref/workflow identity, and subject digest. Only Phase 1 transition consumes transition-bundle inputs. Phase 9 candidate deployment and every Phase 10 artifact-selection path accept only `release_set_run_id` plus `release_set_sha`—never component overrides—and derive build/deploy components from that set. Phase 10 may additionally accept independently attested lifecycle, soak, promotion, or one-use approval evidence created after deployment only when each artifact names the same release-set SHA and its expected protected producer/workflow identity.
- Add the policy YAML to OSS exact mirror, runtime manifest, and OCI labels. Control-plane manifest pins the same hash; mismatch is fatal.
- Support a separate pre-tag `runtime-candidate.lock` at a digest-addressed OCI/artifact locator. Before its Phase 8 build, select and prove one final runtime semver unused across git tags, GitHub Releases, and public OCI tags; freeze that value into the candidate manifest/OCI labels and later release set without creating the tag. Production `runtime.lock` keeps its strict released-artifact contract and continues to describe v0.0.17 until promotion.
- Remove `runtime.lock` from any automatic rollout path filter. Runtime deployment is called explicitly with verified release-set evidence; changing candidate/public lock metadata alone cannot restart Machines.
- OSS candidate build pushes immutable bytes/digests without a semver tag or GitHub Release. Post-soak promotion retags that already-tested digest and publishes the exact manifest without rebuild.
- Remove direct SDK publish dispatch. SDK 0.3 publication is callable only by post-soak promotion with matching release-set evidence.
- Capture the current v0.0.17 production state before runtime.lock changes: exact source revision, GitHub Release manifest/assets, GHCR index/platform digests, current Fly content-addressed tag/Machine digest, contract/core hashes, and verification workflow identity. The signed baseline receipt is immutable and later embedded in the final release set.
- The dedicated below-vNext rollback workflow accepts only the final release set, an issuance-off receipt, and a complete lifecycle-drain receipt. It derives v0.0.17 solely from the embedded baseline member, keeps control plane B, deploys the exact baseline Fly tag, and verifies every Machine; it accepts no raw image/tag/version input and never depends on the later runtime.lock contents.
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
- [x] Release-set schema rejects omitted or free-form component identities and freezes a still-unused final runtime semver without changing candidate manifest bytes later.
- [x] `oss-source.lock` and `runtime.lock` are independently verified; exact-mirror CI follows only source authority, deploy follows only artifact authority, and an attempted cross-use fails.
- [x] The OSS-first shared-auth commit and manifest entry exist before Artifact A's source SHA; Cloud copies are byte-identical.
- [x] The client-credentials-only metadata fixture passes every pinned ext-auth client while empty/fabricated `response_types_supported` fixtures fail.
- [x] The signed v0.0.17 baseline receipt resolves the current Release/GHCR/Fly bytes and rejects every digest/source/manifest substitution.

#### Operator verification

- [x] An ephemeral Fly rehearsal proves the shared lock, artifact manifests, compatibility commands, and candidate locator.
- [x] The runbook contains no obsolete migration target or unverified free-form image input.

#### Phase 0 production evidence (2026-08-10)

- Legacy runtime baseline: protected main workflow run `31370208433`, Actions artifact `9055729218`, receipt SHA-256 `ab616874e51835aa0e2096e07c52d4c748aaca6105ceec5b5fd609b02e6bb268`. The signed member binds `target_env=cloud-production`, `fly.app=nerve-runtime`, v0.0.17 Release assets, source `a794be9f2697e0864d3a31da8f087577e9748f7e`, GHCR/Fly digest `sha256:eaab11e78806e3ed730367c311b1fc30c1360e5be9897d329ec9208912f81765`, and both production runtime Machine IDs. The protected workflow and an independent downloaded-artifact verification both accepted the exact bytes; negative tests reject substituted environment, app, digest, source, and manifest identities.
- Artifact A authority: protected main Cloud CI run `31366330167`, image `ghcr.io/dsmolchanov/nerve-cloud/control-plane@sha256:cc46c364dd99017d25afd6e6a70350cbebedfebc08eea08e90d5f688d1aaa39b`, manifest SHA-256 `1de234082197d4c94530c80929fa0ffecba0122388c4f12e402596000ab171e6`. The private-repository Sigstore producer completed successfully on main.
- Fly rehearsal: protected main workflow run `31368751660`, Actions artifact `9055204919`, receipt SHA-256 `60c706a844369216ba9182c7873a635328e2f23effc0c76e14a0d6f8435fd8c2`. It held `deploy-cloud-production`, verified the independent production/candidate locator paths and runbook invariants, resolved ephemeral Machine `847559f27d3d78` to the exact Artifact A digest, ran all five manifest-listed compatibility commands successfully, confirmed deletion of that exact Machine, and signed the evidence before upload.

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
- new `scripts/deploy/preflight_provider_domains.py`
- new `schemas/provider-domain-preflight-receipt.schema.json`
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

### Dedicated Compatibility Transition

Run `.github/workflows/cloud-0009-transition.yml` under the shared production deploy lock as one resumable state machine:

1. Accept only `transition_bundle_run_id` plus `transition_bundle_sha`, verify its attestation/target/workflow identity, and derive Artifact A from it: Core `[28,28]`, Cloud `[8,9]`, head 0009, issuance off. Raw A image/manifest inputs and final release-set inputs are rejected.
2. Rehearse a production snapshot at exact Cloud `current=0008`, `head=0009`, `pending=[0009]` and Core `current=head=0028`, `pending=[]`; run SQL claim preflight and a recorded provider-inventory fixture containing local, provider-only, and mismatched-ID cases.
3. In production, take the global domain-writer fence, run both the SQL preflight and full Resend inventory comparison, and abort/quarantine on any unresolved provider-only or mismatched object. Keep the fence through step 7 so inventory and writer enable are one transition.
4. Deploy A web on schema 8, explicitly start every exact A web Machine after the maintenance stop, and prove the pinned web ID set, restored proxy autostart, started state, digest, and window; execute read-only compatibility for `nerve-flags`, `nerve-drill`, and `nerve-oauth-clients` from the same image and prove none queries 0009 or mutates.
5. Converge the scheduled reconciler to A, execute only its read-only compatibility command on schema 8, and prove digest/window.
6. Apply 0009 with `/app/nerve-migrate` from the same A image. Cloud pre-state is exactly 0008/head0009/[0009]; post-state is exactly 0009/head0009/[]. Core remains 0028/head0028/[].
7. Redeploy/prove A web and reconciler on schema 9, repeat the exact bounded web-start/ID/digest/autostart proof, and run read-only compatibility for all six manifest-listed database-mutating binaries.
8. Smoke public metadata/JWKS with issuance off, verify the provider-preflight receipt, then release the domain-writer fence; no client registration or generation creation occurs.
9. Emit and independently verify the Phase 0 transition-receipt schema containing the transition-bundle digest, A digest/manifest and all shipped binary hashes/windows, pre/post schema evidence, provider inventory/quarantine resolution, Machine identities/digests, issuance-off proof, workflow identity, and timestamps.

Retry accepts only `schema8 + old/A` or `schema9 + A` states and converges forward. It rejects schema 9 with an unknown digest. Once 0009 is applied, `[8,8]` is forbidden and A is the temporary boundary floor.

### Success Criteria

#### Automated verification

- [ ] 0009 migrates from a production-shaped 0008 snapshot and backfills unambiguous claims.
- [ ] Preflight rejects duplicate live canonical-domain claims.
- [ ] Provider inventory rejects/quarantines provider-only and canonical/provider-ID mismatches, and a two-connection test proves no domain mutation can enter between inventory and writer enable.
- [ ] Down succeeds only with no durable rows and refuses for every protected table class.
- [ ] Migration tests exercise alias/catch-all triggers, state/uniqueness constraints, and claim serialization.
- [ ] Artifact A on schema 9 exercises legacy domain create, verify, delete, and expiry and proves every live Core domain has exactly one canonical claim; Artifact A on schema 8 never queries the claim table.
- [ ] Assertion tests reject wrong identity, audience/resource, key/algorithm/window, replay, and scope mixing.
- [ ] Key tests reject reordered/optional-member aliases, another kid, another client, concurrent duplicate registration, and reuse after retirement/revocation; `kid` always equals the RFC 7638 thumbprint.
- [ ] JTI boundary tests cover first use and replay at `exp-1`, `exp`, and `exp+29s`, exact `retain_until`, and cleanup only after the accepted skew window.
- [ ] Golden token-form tests accept omitted `client_id`, accept a matching field, reject a mismatch/duplicate, and pin every OAuth HTTP status, error body/header, and cache directive.
- [ ] Body/field/segment tests cover every numerical boundary, chunked and false Content-Length requests, duplicate fields, and rejection before decode/crypto/DB.
- [ ] Metadata golden tests omit `response_types_supported`, reject empty/fabricated values, and pass the pinned client-credentials conformance consumers.
- [ ] Decoded JWT fixtures pin PS256/current issuer `kid`, the complete common claim set, exact five/fifteen-minute lifetimes, onboarding absence of `org_id`, and org-token presence of the locked `org_id`.
- [ ] JWKS rotation fixtures prove current/next publication, overlap/retirement after lifetime plus skew, no client assertion key exposure, and access-token alg/kid independence from RS256/PS256 client assertions.
- [ ] Token endpoint cannot accept caller-selected org/generation or scopes outside registration/state.
- [ ] Missing/off/wrong-release issuance control rejects before client lookup; concurrent enable/exchange and disable/exchange tests have one global-lock linearization point.
- [ ] Two-connection barrier tests split token authority precisely: close versus email-token issuance leaves no usable email token; close versus onboarding-token issuance may leave only a short-lived token bound to that same generation with status/idempotent-close access; `revoke-client` versus either issuance leaves neither token usable, in both commit orders.
- [ ] Client revocation drives pending/active generation toward closed.
- [ ] Client-class tests reject reclassification, unprotected synthetic/operator assignment, duplicate synthetic identity, and every activation path outside the protected cohort workflow.
- [ ] Artifact A passes compatibility for all six manifest-listed database-mutating binaries on both schema 8 and 9 before any mutation-specific behavior.
- [ ] Normal deploy cannot perform the transition and transition retry rejects unknown states.
- [x] Web-start convergence tests cover stopped/suspended recovery, already-started idempotency, missing web, digest/ID drift, disabled autostart, unsafe `created` state, and timeout; the transition invokes this proof before provider inventory and after schema-9 redeploy.

#### Operator verification

- [x] `auth.nerve.email` metadata, JWKS, TLS, caching, and canonical host behavior work publicly with issuance disabled.
- [x] Every web and reconciler Machine is A before migration and after migration.
- [x] The signed transition receipt is stored with the release evidence.

**Phase gate:** Pause on schema 9 with Artifact A proven and issuance off. Record A as the temporary boundary floor; do not activate a client.

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

- [ ] Immutable SDK 0.2.0 passes all golden legacy tool/resource/error fixtures byte-for-byte.
- [ ] Modern requests are stateless across handler instances and pass pinned conformance.
- [ ] JSONResponse true/false pass JSON/SSE final-response and failure fixtures.
- [ ] A translation table proves no modern response emits `-32040...-32043`.
- [ ] Route matrix covers both versions, all credentials, and absent/allowed/hostile/null/lookalike Origin; hostile Origin never reaches a handler.
- [ ] Valid native auth with absent Origin succeeds; allowed Origin without auth returns 401; hostile Origin with valid auth returns 403.
- [ ] Phase 2 proves typed `m2m_onboarding` and `m2m_org` principals route independently, but lifecycle dispatch remains absent until Phase 3 registration; calling or listing an unregistered lifecycle tool cannot succeed.
- [ ] Missing extension capability fails before dispatch.
- [ ] Missing/malformed `protocolVersion` or `clientCapabilities`, malformed-present `clientInfo`, absent OAuth extension for M2M, and every header/body version or method/name mismatch fail with the exact modern code before dispatch; omitted `clientInfo` succeeds for a conformant external agent.
- [ ] HS256/RS256 confusion and unknown-kid/stale-cache cases fail closed.
- [ ] Memory and 413/503 tests are deterministic and leak-free.
- [ ] Runtime/policy/contract hashes match exact-mirror sources.

#### Manual verification

- [ ] Explicit 2025 returns the frozen shape; explicit 2026 returns modern JSON or SSE.
- [ ] SDK 0.2.0 and a native M2M client use the same `/mcp` URL concurrently.
- [ ] Modern `tools/list` changes with principal/state while cache scope remains private.

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

- [ ] Concurrent same-key start creates one graph and returns one onboarding ID.
- [ ] Same key with different normalized input is a typed conflict.
- [ ] Different key while a generation is live cannot create a second org.
- [ ] A DB failure at every graph step rolls back org, entitlement, usage, and flags.
- [ ] Lost MCP/internal HTTP response followed by status returns the persisted graph.
- [ ] Onboarding token cannot submit or override client ID, org ID, owner org ID, domain ID, inbox ID, or generation.
- [ ] Delegation rejects bad signature, stale timestamp, nonce replay, redirect, or mismatched bearer/client.
- [ ] After the first token expires, a new onboarding-scope exchange selects the same live generation and can status/verify/close it.
- [ ] A token bound to closed generation N cannot start N+1; a fresh assertion exchange plus new key can.
- [ ] With N+1 live, a still-unexpired N token can read only N's closed result, verify is rejected for closed N, and idempotent close returns only N; it cannot observe, verify, close, or otherwise affect N+1.
- [ ] `m2m_org` never exposes lifecycle tools and mixed onboarding/email scope exchange is rejected.
- [ ] After Phase 3 registration, `m2m_onboarding` lists exactly Start/Status/VerifyDomain/Close, hidden lifecycle calls from `m2m_org` fail, and every profile assertion is cache-private.
- [ ] Reconciler retries and assertion-JTI cleanup are exercised with a real PostgreSQL service.

#### Manual verification

- [ ] A canary onboarding client sees only four tools and creates the durable graph without any API-key response.
- [ ] Email-scope exchange is denied before active while onboarding-scope exchange remains available.

---

## Phase 4: Fenced Provider Lifecycle and Billing Closure

### Overview

Run every external operation through a lease/version fence and make cleanup—including Stripe cancellation—a required barrier. Reconnect is possible only after the previous generation is provably closed.

### Changes Required

#### 4.1 Lease and fence every external operation

**Cloud files**

- `internal/onboarding/service.go`
- `internal/onboarding/state.go`
- `internal/reconcile/service.go`
- `cmd/nerve-reconcile/main.go`

**Changes**

- Workers claim bounded batches with `SKIP LOCKED` and acquire a time-bounded lease by CAS over onboarding ID, state, and workflow version.
- Before an external call, persist provider intent, stable operation ID, and workflow version, commit, then call without holding a DB transaction.
- Apply a result only when state, workflow version, provider operation, and lease still match.
- Lease expiry permits takeover by provider lookup and stable identity rather than blind recreation.
- `close` increments workflow version and enters deprovisioning, fencing every verifier/provisioner already in flight.
- A stale success that created or enabled a resource schedules compensating disable/delete and cannot reactivate the generation.
- Permanent failures store a bounded terminal reason and enter deprovisioning. There is no `failed` state outside live uniqueness and cleanup.

#### 4.2 Linearize token shutdown and cleanup start

**Cloud files**

- `internal/onboarding/service.go`
- `internal/store/agent_onboardings.go`
- `internal/store/store_tokens.go`
- `internal/oauth/issuer.go`
- `cmd/nerve-oauth-clients/main.go`

**Changes**

- Close, token issuance, targeted generation email-authority revoke, key revoke, and client revoke use the common lock helper and state recheck defined in Phase 1.
- Close atomically sets deprovisioning, increments the workflow version, disables effective outbound authorization, and revokes only `m2m_org` rows for `oauth_client:<client_id>:g:<generation>` before any provider call.
- Existing/reacquired onboarding tokens remain bound to that same generation and authorize only status plus idempotent close while deprovisioning/closed; they never authorize email or N+1. They expire normally and do not block `closed`.
- `revoke-client` performs the same lifecycle transition, marks the client unusable, and thereby invalidates both onboarding and org tokens before the reconciler finishes cleanup.
- No email-scope token can be issued after the transition commits.

#### 4.3 Fence subscription creation and cancel generation-owned Stripe state

**Cloud files**

- `internal/onboarding/service.go`
- `internal/billing/stripe.go`
- `internal/store/store_billing.go`
- `internal/store/agent_billing_workflows.go`
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

#### 4.4 Complete cleanup and reconnect

**Cloud files**

- `internal/onboarding/service.go`
- `internal/reconcile/service.go`
- `cmd/nerve-reconcile/main.go`
- `internal/store/outbox.go`
- `internal/emailtransport/outbox_worker.go`
- `internal/store/email_tenancy.go`

**Changes**

- Close takes the per-org policy lock, increments the policy epoch, forbids new outbox claims/provider starts, and terminalizes generation-owned `queued` rows as `policy_revoked`. Every `sending` row must either be fenced before `provider_started_at` or drain to a terminal/readback-resolved outcome after its earlier linearization point.
- Retire a managed alias before disabling—but never cascading-deleting—the generation-owned inbox, revoke its grant, or transition a custom-domain claim to releasing; then reconcile provider removal.
- Use the specialized Cloud-only autonomous tombstone predicate only after proving every retained inbox belongs to this onboarding generation, is disabled, has no queued/sending outbox row, and has no unresolved provider-started operation. Existing legacy tombstone behavior remains unchanged.
- `closed` requires terminal Stripe evidence for every generation billing workflow (`canceled` or proven absent), retired/released mailbox and domain state, the outbox barrier, provider readback proving disabled/deleted, no live `m2m_org` generation token, and a tombstoned org with retained disabled inbox/audit rows. Short-lived onboarding status/close tokens do not block this state.
- A repeated close returns the same progress. An old start/close idempotency key returns its persisted generation.
- Only a fresh onboarding token and new start key after `closed` can allocate N+1, with a distinct org external reference and no inherited resources.

### Success Criteria

#### Automated verification

- [ ] Verify-versus-close and verify-versus-reconcile races cannot overwrite a newer workflow version or restore active state.
- [ ] Expired-lease takeover resumes by stable provider identity without duplicating resources.
- [ ] Stale provider success schedules compensating cleanup.
- [ ] Permanent provisioning failure remains uniquely live in deprovisioning until cleanup completes.
- [ ] Two-connection barriers prove close versus email issuance leaves no usable email token; close versus onboarding issuance leaves at most a poll/close-only token for N; and `revoke-client` versus either issuance leaves neither token usable, in both commit orders.
- [ ] Close revokes email tokens and outbound permission before the first Stripe/provider call.
- [ ] Subscribe-versus-close tests cover both transaction commit orders, a delayed create response, provider-unknown lookup, `requires_action`, and a webhook that materializes `trialing`/active after close; all converge to cancellation without paid evidence or a false `closed`.
- [ ] Stripe timeout remains deprovisioning and retry uses the same idempotency key.
- [ ] `closed` is impossible while a subscription-create outcome remains unresolved, an in-flight/provider-started email is unresolved, or a required subscription cancellation/provider cleanup is unconfirmed.
- [ ] Two-connection barriers cover enqueue, claim, provider-start, complaint, and close in both commit orders: no provider start occurs after the new policy epoch, queued rows terminalize, and an earlier provider start keeps close pending until terminal/readback.
- [ ] A worker holding a payload cannot race inbox cleanup into an untracked send; inbox rows remain disabled/retained, MarkSent detects zero affected rows, and the autonomous tombstone predicate refuses any nonterminal outbox state.
- [ ] FK/retention tests prove close preserves outbox/message/audit evidence and never invokes cascading DeleteInboxForOrg.
- [ ] Generation 2 receives distinct org/trial state and inherits no subscription/evidence.

#### Operator verification

- [ ] Killing a reconciler after each external call converges to active or fully closed without duplicate provider resources.
- [ ] Closing a paid canary confirms Stripe cancellation before tombstone and permits a clean reconnect.

---

## Phase 5: Fresh Nerve-Managed Mailbox

### Overview

Provision a ready mailbox from a protected preverified platform domain and make every reserved or retired address impossible to create through another API, catch-all, or SQL path.

### Changes Required

#### 5.1 Register and validate the platform domain

**Cloud files**

- `internal/config/config.go`
- `internal/config/config_test.go`
- `internal/store/managed_mailbox_aliases.go`
- `internal/store/inboxes_manage.go`
- `deploy/cloud/env.example`
- `docs/REPO_SPLIT_RUNBOOK.md`
- new `cmd/nerve-drill/managed_mailbox.go`

**Changes**

- Configure enabled state, owner org ID, and platform org-domain ID; keep production identifiers outside the repository.
- Insert or validate `managed_mailbox_platform_domains` only after proving the owner org is live, the domain is active/fully verified, receiving is enabled, and catch-all is disabled.
- Under the platform-domain advisory lock and with allocation disabled, snapshot every existing active and disabled inbox on the chosen canonical domain. Permanently backfill each address/inbox pair into the alias registry as legacy-reserved with its observed state, then prove a second snapshot has no unregistered address before enabling the platform writer. Never attach these reservations to an onboarding generation.
- Fail closed on missing/mismatched configuration or readiness.
- Disabling new allocation leaves the registered namespace and all retired aliases protected.

#### 5.2 Allocate under database-enforced namespace ownership

**Cloud files**

- `internal/store/managed_mailbox_aliases.go`
- `internal/store/agent_onboardings.go`
- `internal/store/email_tenancy.go`
- `internal/store/inboxes_manage.go`
- `internal/cloudapi/resend_webhook.go`
- `internal/onboarding/service.go`

**Changes**

- Generate the random local part and inbox UUID only on the server.
- In one transaction insert a `reserved` alias naming that exact inbox UUID, ensure the owner-to-grantee grant, create that inbox, and transition the alias `active`.
- Reuse existing locked grant/inbox helpers; do not add a second public provisioning implementation.
- Retry only a cryptographic collision; do not expose an address-choice loop to the client.
- Persist the winning address before returning.
- Retiring cleanup changes alias to `retired` before inbox deactivation and never deletes or reactivates it.
- Cloud 0009 triggers reject ordinary create/ensure/address update/reactivate/direct SQL unless a registered address maps to the same reserved/active inbox ID; retired rejects even its old ID.
- Database enforcement prevents catch-all enablement for the platform domain. Inbound routing drops/rejects unknown or retired recipients before the generic catch-all path.

### Success Criteria

#### Automated verification

- [ ] Platform owner/grantee confusion and inactive-domain cases fail closed.
- [ ] Registration refuses any unaccounted pre-existing platform inbox; active and disabled rows are permanently backfilled, cannot be allocated/reactivated under another ID, and survive later inbox status changes.
- [ ] Parallel allocations never share an address.
- [ ] Injected failures after grant, alias, or inbox insert roll back the entire graph.
- [ ] Same-key replay returns the same address.
- [ ] Cleanup plus generation 2 produces a different address.
- [ ] Ordinary create, ensure, address update, reactivate, catch-all, direct SQL, and concurrent races cannot claim a reserved or retired alias.
- [ ] No non-alias inbox can activate under the platform domain and catch-all cannot be enabled.
- [ ] Inbound to an unknown or retired platform address creates no inbox or thread.
- [ ] Tenant can read/use the granted domain but cannot mutate or delete the owner's domain.
- [ ] Inbound Resend delivery resolves to the correct grantee inbox/org.

#### Manual verification

- [ ] Canary receives a real external email at its generated platform address.
- [ ] The message appears through modern list_threads/get_thread within the established ingestion SLO.
- [ ] Mail to an unallocated sibling address creates no inbox or thread.

---

## Phase 6: Autonomous Custom-Domain Ownership and Readiness

### Overview

Serialize legacy and autonomous ownership through one canonical-domain claim and make provider provisioning resumable, fenced, and releasable. Activation still requires the complete mail path.

### Changes Required

#### 6.1 Put every domain path behind one claim

**Cloud files**

- `internal/domains/canonical.go`
- `internal/store/org_domains.go`
- `internal/cloudapi/handler_domains.go`
- `internal/onboarding/service.go`
- `internal/reconcile/service.go`

**Changes**

- Acquire one global transaction advisory lock derived only from canonical domain; never include org ID.
- Under that lock create/reconcile `domain_ownership_claims` for legacy REST and autonomous onboarding alike.
- Treat every foreign pending, provider-owned, or releasing claim as a conflict regardless of claim expiry. Under the canonical lock, an expired pending claim is first fenced into `releasing` and its owner workflow into `deprovisioning`; it cannot be overwritten or rebound.
- Give autonomous Core domain rows a stable `m2m-onboarding:` external-reference prefix.
- Replace direct pending deletion: generic `ExpirePendingDomains` excludes autonomous rows and cannot delete any claimed domain.
- Legacy expiry/delete transitions its claim to releasing and uses the same confirmed provider-cleanup path.
- Release a claim only after provider readback proves safe removal and no live Nerve resource depends on it.

#### 6.2 Run the provider workflow under lifecycle fencing

**Cloud files**

- `internal/onboarding/service.go`
- `internal/domains/instructions.go`
- `internal/domains/verification.go`
- `internal/emailtransport/providers/resend/resend_domains.go`
- `internal/store/org_domains.go`
- `internal/reconcile/service.go`

**Changes**

- Persist pending Core domain and ownership claim before external work.
- Perform provider create/lookup/verify/receiving-enable/disable/delete outside DB transactions using stable operation IDs.
- Unknown outcomes remain resumable only when canonical provider-domain lookup matches the workflow's persisted provider intent/fence or a Phase 1 explicitly adopted inventory record. A provider object found only by canonical name with no matching ownership provenance is quarantined, never claimed as this workflow's timeout result.
- Persist complete DNS records/checks and transition to `dns_pending`.
- Apply results only under the current workflow version and lease. Close/expiry fences in-flight verification and moves the claim to releasing.
- A stale create/enable success initiates compensating disable/delete. Permanent provisioning errors enter deprovisioning; no state escapes cleanup.

#### 6.3 Require complete live readiness and instructions

**Contract/docs files**

- `docs/MCP_Contract.md`
- `docs/TENANT_GUIDE.md`
- `internal/onboarding/service.go`
- `sdk/python` examples

**Changes**

- Activate only after ownership challenge, SPF, DKIM, inbound MX, provider verified status, and receiving-enabled readback all succeed.
- Never accept caller-supplied verified state.
- Create the requested inbox transactionally after provider checks. Keep legacy public domain semantics but use the stricter autonomous predicate.
- Return record type, name, value, TTL where applicable, purpose, observed state, and retry guidance in ordinary complete results.
- State that the agent must use another DNS connector.
- Never ask for registrar/API credentials in a Nerve tool.

### Success Criteria

#### Automated verification

- [ ] Canonical case/IDN/trailing-dot variants resolve to one claim.
- [ ] Legacy-versus-autonomous and autonomous-versus-autonomous pending races produce one winner.
- [ ] Migration preflight rejects ambiguous existing claims.
- [ ] Legacy pending GC cannot delete autonomous work or bypass provider cleanup.
- [ ] Expired-pending create-versus-GC and unknown-provider-outcome races never rebind the claim until the old provider identity is proven absent/removed and the old claim is released.
- [ ] Provider timeout and lease takeover do not duplicate provider domains.
- [ ] A provider-only orphan or mismatched provider ID can never be adopted by canonical-name lookup without the explicit preflight/adopt receipt; writer enable remains blocked while quarantine is unresolved.
- [ ] Verify-versus-close cannot restore active state; stale success is compensated.
- [ ] SPF/DKIM without MX/receiving remains dns_pending.
- [ ] Only full readiness creates the inbox and domain-scoped compose permission.
- [ ] Domain revocation or receiving failure removes effective domain compose permission.
- [ ] A claim is reusable only after confirmed provider cleanup and release.

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
- `internal/mcp/billing.go`
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

**OSS-first/shared files**

- `internal/tools/service.go`
- new `internal/tools/outbound_policy.go`
- `internal/store/store_threads.go`
- `internal/store/outbox.go`
- `internal/store/feature_flags.go`
- `internal/emailtransport/outbox_worker.go`
- `internal/entitlements/service.go`
- `configs/policy/autonomous-outbound-v1.yaml`

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

#### 7.3 Project only paid and abuse evidence

**Cloud files**

- `internal/cloudapi/handler_billing.go`
- `internal/billing/stripe.go`
- `internal/cloudapi/resend_webhook.go`
- `internal/store/store_billing.go`
- `internal/store/agent_billing_workflows.go`
- `internal/store/agent_outbound_evidence.go`
- `internal/store/feature_flags.go`
- `internal/reconcile/service.go`

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
- Reconciler recomputes paid/suspension projection and detects drift.

#### 7.4 Reserve durable limits with matching events

**OSS-first/shared files**

- new `internal/store/outbound_limits.go`
- `internal/entitlements/rate_limiter.go`
- `internal/tools/outbound_policy.go`

**Cloud files**

- `internal/reconcile/service.go`

**Changes**

- Use PostgreSQL UTC buckets and the common `(org_id,meter,period_start,period_end)` counter-row lock across Machines. Reservation and reconciliation take the same lock.
- For every reply, total-send, first-recipient, or recipient-hash reservation, insert a matching successful `usage_events` row in the same transaction as idempotency and outbox enqueue.
- Derive the global replay ID as SHA-256 over the versioned length-prefixed `(org_id,tool_name,idempotency_key,meter,dimension)` tuple so retry cannot increment twice, and cross-org/cross-tool keys cannot collide.
- Reconciler locks the counter before SUM and SET and performs both in one transaction. A concurrent reservation is therefore wholly before the SUM or waits and increments after SET.
- Count an attempt when accepted for enqueue regardless of later provider outcome; do not refund abuse units.
- Keep the existing process-local MCP RPM limiter only as a cheap front-line shedder until a durable MCP RPM replacement is proven.
- Garbage-collect expired rate buckets without removing audit or delivery history.

### Success Criteria

#### Automated verification

- [ ] Onboarding token cannot call any email tool.
- [ ] Locked autonomous org can read/draft/reply but cannot list or call compose.
- [ ] Autonomous policy missing/read-error/malformed denies M2M send while legacy behavior is unchanged.
- [ ] Reply on an outbound-only thread fails and enqueues nothing.
- [ ] Reply after multiple outbound messages still targets the latest real inbound sender.
- [ ] Custom-domain evidence enables compose only for that owned domain.
- [ ] Confirmed paid evidence enables org-wide compose, including managed mailbox.
- [ ] Trial entitlement alone does not enable compose.
- [ ] Spoofed From, self-mail, arbitrary Authentication-Results headers, and inbound volume never unlock compose.
- [ ] Suspension overrides every scope/evidence path immediately, including a previously issued compose token.
- [ ] Enqueue/claim/provider-start versus complaint/close barriers in both orders prove no provider start after the new epoch and correct drain/readback of the earlier linearization point.
- [ ] One complaint suspends; bounce threshold obeys sample size and rate.
- [ ] Multi-Machine concurrent sends cannot exceed durable limits.
- [ ] Every counter increment has a matching event and a generic reconcile preserves the value.
- [ ] Cross-org same-key, cross-tool same-key, and two-connection reserve-versus-reconcile fixtures prove no replay collision or lost update.
- [ ] Idempotent replay consumes one applicable unit/event and creates one outbox row.
- [ ] Closing a paid generation removes compose immediately and never transfers it to reconnect.
- [ ] Caller manipulation of needs_human_approval cannot bypass policy.
- [ ] Autonomous billing rejects the legacy checkout route, caller org/generation/payment fields, missing/disabled mandate, cap violations, and `requires_action`; exact tool replay returns one subscription workflow.
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
- Support a deliberate legacy mode/fallback for ordinary email tools against v0.0.17.
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
- Test 0.3.0 against both vNext modern mode and v0.0.17 legacy mode.

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

**Changes**

- Before building, select the approved final runtime semver, prove it is unused across git tags, GitHub Releases, and public OCI tags, and freeze it as `runtime_version` in the candidate manifest and OCI labels without creating a public tag. Prove the SDK 0.3.0 filename is not already published with different bytes.
- Build the OSS candidate exactly once from an exact successful main SHA without a semver tag, GitHub Release, or public release asset; retain the attested manifest/index/platform digests as the sole Phase 9 inputs.
- In one Cloud PR, apply exact mirrors, advance `oss-source.lock`, and write `runtime-candidate.lock` with the attested candidate locator/digests, source revision, MCP contract hash, policy hash, and runtime-manifest SHA. Keep deployed-artifact `runtime.lock` on v0.0.17 until post-soak promotion; changing either source/candidate lock is non-deploying.
- Keep CORE_SCHEMA_HASH and [28,28] unchanged.
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

- [ ] SDK version constants, wheel metadata, filename patterns, and importlib version all equal 0.3.0.
- [ ] Official client conformance passes for 2026-07-28.
- [ ] OAuth tests cover discovery, assertion, refresh, clock skew, replay response, and secret redaction.
- [ ] Private-key tests cover PKCS#8/PKCS#1, PS256/explicit RS256, RSA-size and encryption rejection, derived/mismatched kid, deterministic clock/JTI, exact claims, single auth mode, and no secret leakage.
- [ ] Candidate build installs exact hash-locked PyJWT 2.13.0 and cryptography 49.0.0 bytes and rejects dependency/hash drift.
- [ ] 0.3.0 emits every required header/metadata/capability and parses JSON plus multiline/notification SSE.
- [ ] Missing content type, wrong/duplicate ID, malformed/truncated SSE, cancellation, and EOF without final response fail safely.
- [ ] Separate token-cache tests cover active, deprovisioning, expiry, close, and N-to-N+1 selection.
- [ ] 0.3.0 modern tests cover all tool success/error schemas and result unwrapping.
- [ ] 0.3.0 legacy mode passes against v0.0.17 for existing email tools.
- [ ] Exact published 0.2.0 wheel passes against vNext.
- [ ] Candidate/source locks, frozen final semver, policy, manifest, and exact-mirror checks are green in the same Cloud PR while production runtime.lock still validates v0.0.17; rebuilding or changing manifest bytes is not a later release step.
- [ ] The SDK publish workflow cannot be manually dispatched.
- [ ] No hard-coded 0.2.0 build assertion remains except the explicit legacy-consumer fixture.

#### Manual verification

- [ ] A reference external agent can connect with only endpoint, client_id, and its private key.
- [ ] No Nerve API key is generated or copied during onboarding.

---

## Phase 9: Candidate Release Set and Cloud `[9,9]` Contraction

### Overview

Replace temporary Artifact A with feature-complete Artifact B, record B as the permanent rollback floor, and then deploy contracted Artifact C. Runtime and SDK remain unpublished candidates addressed only by immutable digest/SHA.

### Artifact identities

| Artifact | Purpose | Core window | Cloud window |
|---|---|---:|---:|
| A | Initial migration-boundary predecessor from Phase 1 | `[28,28]` | `[8,9]` |
| B | Final feature-complete rollback predecessor | `[28,28]` | `[8,9]` |
| C | Normal contracted production control plane | `[28,28]` | `[9,9]` |

A and B are different immutable images. After B is proven, A is historical transition evidence and is no longer an operational rollback target.

### Changes Required

#### 9.1 Build B and C, resolve the Phase 8 candidates, and attest the release set

**Files**

- `internal/startup/migrations.go`
- `internal/startup/migrations_test.go`
- `deploy/cloud/Dockerfile.control-plane`
- `.github/workflows/ci.yml`
- `.github/workflows/deploy.yml`
- `.github/workflows/runtime-deploy.yml`
- `.github/workflows/control-plane-deploy.yml`
- new `.github/workflows/mcp2026-candidate.yml`
- new `.github/workflows/mcp2026-runtime-mirror.yml`
- new `scripts/release/build_mcp2026_release_set.sh`
- new `scripts/ci/verify_mcp2026_release_set.sh`
- OSS candidate workflow from Phase 8
- `.github/workflows/publish-python-sdk.yml`

**Changes**

- Build B only after Phases 2-8 are complete. B contains final behavior and retains Cloud `[8,9]`; at schema 8 every 0009-dependent path remains issuance-off/inactive and cannot query a 0009 object.
- Build C from an explicit contraction change. Restrict the B-to-C source diff to compiled Cloud minimum, compatibility tests, manifests, and release wiring; fail CI if business behavior changes.
- Resolve the exact already-built Phase 8 runtime candidate and SDK 0.3 wheel by attested workflow-run identity and SHA; verify source, manifest, frozen semver, filename, and digests. Phase 9 never rebuilds either artifact.
- Before release-set construction, run the protected mirror workflow with only the verified OSS candidate run ID and receipt SHA. Authenticate to Fly Registry, copy the exact candidate digest to a content-addressed non-semver `registry.fly.io/nerve-runtime:sha-...` tag without deployment or rebuild, and resolve both target index and linux/amd64 Machine digest. An already exact tag is an idempotent retry; any mismatch fails closed. Emit the signed runtime-mirror receipt defined in Phase 0.
- Resolve and reverify the Phase 0 v0.0.17 legacy-runtime-baseline receipt. Release-set construction accepts only its protected run ID/SHA and the new mirror receipt run ID/SHA; it accepts no raw Fly tag, baseline tag, or digest.
- Re-prove the frozen runtime semver and SDK filename remain unpublished before constructing the release set; a collision stops the release and requires a new plan revision/candidate build rather than manifest rewriting.
- Generate/attest the canonical release set defined in Phase 0, binding A/B/C, runtime candidate and verified Fly mirror receipt, the v0.0.17 baseline member, SDK, schemas, contract, policy, exact mirrors, conformance pins, source SHAs, and workflow identities.
- Inject the verified release-set SHA and bounded signed canonical set/verification envelope at deployment time. B/C require it from their immutable artifact-role manifest even when every environment marker is absent. Each binary verifies the envelope offline and reports its build-manifest identity plus that runtime-bound release-set identity only after proving exact role/image/manifest/binary membership; no binary or manifest is rebuilt to embed the release-set hash.
- Candidate deployment accepts only release-set run ID/SHA. Remove or disable raw image, tag, wheel, manifest, and digest inputs in `deploy.yml`, `runtime-deploy.yml`, and `control-plane-deploy.yml`; every candidate entry point must derive exact components from the verified set, and reusable child workflows reject calls without the parent verification receipt.

#### 9.2 Rehearse the final lifecycle and rollback graph

- Restore a fresh production snapshot at Core 28/Cloud 9 including every onboarding lifecycle state, expired/active leases, domain claims, aliases, billing workflows, provider retries, tokens, and usage meters.
- Before building/deploying the candidate, run a claim-drift preflight proving every live Core domain has exactly one canonical ownership claim and no claim points at a missing/wrong live domain.
- Run all six B manifest-listed database-mutating binaries on schema 9 and schema 8. On 9 prove full behavior; on 8 prove legacy behavior, issuance-off refusal, and absence of every 0009 query/mutation before each binary's first side effect.
- Run C on schema 9 and prove all six manifest-listed database-mutating binaries reject schema 8 before listener, lease, provider call, or mutation.
- Rehearse controlled C→B→C rollback on schema 9 with no data rewrite or migration.
- Run immutable SDK 0.2 and exact candidate SDK 0.3 contracts against the release-set runtime.

#### 9.3 Install B as rollback floor and contract to C

1. Under the shared deploy lock, verify Core current/head 28 and Cloud current/head 9 with zero pending plus a valid Phase 1 transition receipt.
2. Deploy the release-set runtime candidate only from its verified content-addressed Fly mirror; prove every active/stopped runtime Machine and resolved linux/amd64 digest.
3. Deploy B web and reconciler; run all six manifest-listed compatibility commands and full legacy/modern contract smoke.
4. Prove every active/stopped/scheduled control-plane Machine is B and record B in signed deployment evidence as the permanent rollback floor.
5. Re-prove schema 9 and deploy C web; admit traffic only after the shared `[9,9]` readiness check.
6. Converge reconciler to C only after web is green; run reconciler and migrate compatibility before mutation/schedule activation.
7. Prove every web/reconciler Machine and binary digest/window is C and run both SDK contracts again.

If C fails, roll web and reconciler back to B. Do not change schema and never roll back to A or `[8,8]`.

### Success Criteria

- [ ] B is feature-complete, runs legacy/issuance-off behavior on Cloud 8 and full behavior on 9; C is behaviorally equivalent on 9 but refuses 8.
- [ ] Every manifest-listed database-mutating binary verifies and reports its immutable build-manifest identity, injected release-set hash, exact set membership, and correct window without an artifact rebuild.
- [ ] Missing/wrong injected release-set identity and every raw candidate deployment bypass fail before listener or mutation.
- [ ] B is recorded before any C Machine receives traffic.
- [ ] C→B→C rehearsal and production rollback path require no schema change or data loss.
- [ ] One signed release set binds every exact candidate and policy/contract/schema provenance item.
- [ ] Runtime and SDK bytes/digests equal the Phase 8 artifacts exactly, and the frozen runtime manifest/version is byte-identical before and after release-set construction.
- [ ] Mirror production accepts only the candidate receipt, copies without rebuild/deploy, is idempotent only for an exact existing tag, and release-set verification rejects a missing/substituted mirror receipt.
- [ ] The release set embeds the independently verified v0.0.17 baseline member, and the dedicated rollback entry point accepts only release-set + issuance-off + complete-drain evidence.
- [ ] No semver tag, GitHub Release, public OCI release tag, or PyPI 0.3 file exists yet.

**Phase gate:** Do not begin production drills until every Machine is C, B is recorded, and the signed release set independently verifies from production.

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
- With global issuance still off, a protected `mcp2026-enable-issuance` step verifies the signed release set, exact deployed runtime/C digests, schema 9, policy/contract hashes, transition receipt, and exactly these two active synthetic identities. Under the shared deploy lock it atomically enables `oauth_issuance_control` for that release-set SHA and emits a signed enable receipt. Only then may the first token exchange run.
- Missing/wrong release set, an external active client, stale Machine digest, schema drift, or a concurrent disable makes enable/exchange fail closed. Every rollback first performs the symmetric locked disable, emits an issuance-off receipt, and only then drains/revokes lifecycle state.
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
6. Close custom generation 1, then managed generation 2; assert no non-closed onboarding, active grant/inbox/domain claim, usable email token, unresolved billing workflow, or provider resource remains and both managed aliases remain retired. Retain confirmed billing/cleanup rows as audit evidence. Keep the two canary identities/keys active only to create the explicitly separate soak generations in 10.4.
7. Emit two separately attested lifecycle receipts—managed and custom—from the recorded drill events and zero-live-resource queries. Promotion accepts only each receipt's protected workflow-run ID plus SHA and verifies that both name the exact deployed release-set SHA; component overrides are impossible.

#### 10.4 Run continuous soak canaries

**Files**

- new `.github/workflows/mcp2026-production-canary.yml`
- new `.github/workflows/mcp2026-enable-issuance.yml`
- new `.github/workflows/mcp2026-post-soak-promote.yml`
- new `.github/workflows/mcp2026-activation-approval.yml`
- new `.github/workflows/mcp2026-activate-clients.yml`
- new `scripts/deploy/mcp2026_managed_canary.py`
- new `scripts/deploy/mcp2026_custom_domain_canary.py`
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

- Each lifecycle receipt binds schema version; signed release-set digest; deployed runtime/C digests and schema; hashed client identity, immutable client class, mode, key thumbprint, and every generation; idempotency replay outcomes; token-expiry/reacquisition proof; mode-specific readiness/mail assertions; cross-client denials; billing/provider cleanup states; retired alias/domain-claim evidence; zero-live-resource query results; workflow/run identity; timestamps; and a redaction attestation. Missing cleanup fields, cross-mode substitution, or a different release set makes verification fail.
- After lifecycle cleanup, use fresh onboarding tokens and new idempotency keys to create an explicit managed generation 3 and custom generation 2 for soak. Bind the two immutable `synthetic` client IDs in protected release configuration and the drill/soak evidence; normal registration cannot claim those identities or class.
- Probe both clients at least every 15 minutes for at least 24 uninterrupted hours. A missing interval longer than 30 minutes, any failed probe, digest/schema/policy mismatch, cross-tenant anomaly, or unresolved alert resets the continuous-window start.
- Every observation verifies and records the same release-set digest, deployed runtime/C digests, schema 9, and policy hash before calling MCP.
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
- It queries durable client classification/activation state and verifies issuance still enabled for the exact current release set, current digests/schema 9, both drill receipts, gate-appropriate fresh cadence-valid soak evidence, green synthetic canaries, and zero blockers before an audited targeted DB activation.
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
- [ ] B/C, release-set, lifecycle-receipt, soak, and promotion digests are recorded in the runbook.
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

- Cloud 0008 to 0009 preflight/backfill/transition and refusal-style down for every protected table class.
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
- SDK 0.3.0 pinned PEM/private-key JWT, JSON/SSE modern mode against vNext, and legacy mode against v0.0.17.
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
- Acquire global issuance, client, org-policy, billing, canonical-domain, and row locks in the documented order and never hold transaction locks across network I/O.
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

- Cloud 0009 is authored only in nerve-cloud.
- Core remains at 0028; CORE_SCHEMA_HASH and runtime core window stay unchanged.
- Cloud 0009 owns its lifecycle tables and the alias/catch-all protection triggers installed over existing Core inbox/domain tables in the same database.
- Shared Go/auth/store files follow OSS-first exact-mirror discipline where they truly exist in both repositories.
- `oss-source.lock` alone selects shared-source authority during development; `runtime.lock` alone selects the deployed production artifact. `runtime-candidate.lock` is non-deploying evidence consumed only by release-set construction.
- internal/mcp remains OSS/runtime-only.
- The exact outbound policy bytes and declared shared auth/store/contract files move OSS-first and are present in both manifests.
- Cloud-only billing, client registry, onboarding, subscription cancellation, alias/domain claim, and evidence files do not move into OSS.

### Expand and contract

- Artifact A `[8,9]` crosses the boundary in the dedicated deploy-before-migrate transition and is only the temporary floor.
- Artifact B is built from final feature code with `[8,9]`, proven on schema 9 and compatibility-tested on 8, then recorded as the permanent rollback floor.
- Artifact C contracts the same behavior to `[9,9]`; web, reconciler, and migrate all reject schema 8 before side effects.
- Cloud 0009 down refuses after durable data. Production recovery uses forward fixes/restores, not casual down migration.

### Runtime promotion

- Build runtime/SDK candidates without public tags and bind them with A/B/C, schema, policy, contract, and the preselected final runtime semver in one signed release set.
- Mirror and deploy the candidate by immutable digest, with a signed pre-release Fly-mirror receipt. The protected post-soak Cloud workflow hands a signed request to the least-privilege OSS-side no-rebuild publisher, verifies its receipt, publishes the exact wheel, and emits promotion evidence without rebuild or manifest mutation.
- Candidate and public runtime locks are separate formats and verifiers; moving from `runtime-candidate.lock` to public `runtime.lock` metadata is non-deploying, and promotion converges `oss-source.lock` to the released OSS SHA.
- Editing runtime.lock never activates a client or authorizes a rollout.

### No staging environment

Preserve the existing decision to use production-snapshot rehearsal plus two isolated production synthetic clients rather than a nominal staging system that cannot exercise production-shaped Resend, Stripe, DNS, and attachment state.

## Rollback Matrix

| Component | Schema 8 before transition | Schema 9 after A | Candidate production after B/C | After external activation |
|---|---|---|---|---|
| Control plane | `[8,8]` remains valid until every role is A | A `[8,9]` temporary floor; `[8,8]` forbidden | C `[9,9]` normal; B `[8,9]` is the only rollback floor | Same C→B rollback; never A/`[8,8]` |
| Runtime | v0.0.17 while issuance is off | v0.0.17 possible only with autonomous issuance/use off | Release-set dual runtime is product floor | Rollback below it requires client disable and lifecycle drain |
| Schema | Cloud 8 | Cloud 9; no casual down after durable rows | Cloud 9 | Cloud 9; forward fix or restore only |
| SDK 0.2.0 | Supported | Supported | Supported and canaried | Supported until separate retirement plan |
| SDK 0.3.0 | Unpublished candidate | Modern unavailable if runtime remains old | Exact release-set wheel | Published exact bytes; legacy email fallback only below vNext |
| Alias/domain claims | None before 0009 | Permanent registry/claims survive every rollback | Never delete/reuse/bypass | Same invariant |
| Billing cleanup | None | Durable subscribe/payment/cancellation workflows survive rollback | B and C reconcile the same state | Never tombstone or close while subscription-create outcome or cancellation is unconfirmed |
| Outbox shutdown | Legacy behavior | Autonomous policy epoch/barrier state is durable | B and C cancel queued and drain/readback provider-started work | Same barrier; no cascading inbox deletion |
| Client activation | None | Issuance off/no client active | Synthetic clients only | Explicit post-soak client list only |

Preferred rollback is C→B with schema unchanged. Before any runtime rollback below vNext:

1. Atomically disable global M2M issuance under the common deploy/global issuance lock, verify the exact release-set-bound issuance-off receipt, and disable all further client activation.
2. Enumerate every non-closed M2M generation, including `active`, under the common lifecycle lock. For each one, run audited close or `revoke-client`, revoke email authority, and retain its onboarding-only polling path when the client itself is not revoked.
3. Keep B running until every enumerated generation is `closed`, every subscription-create/payment/cancellation workflow, outbox provider-start, and domain-claim release is terminal, and signed drain evidence proves the enumeration did not change. Refuse rollback while any generation or cleanup barrier remains nonterminal.
4. Do not delete Cloud 0009 state, alias tombstones, domain claims, billing workflows, or evidence history.
5. Invoke the dedicated baseline rollback workflow with only the final release set, issuance-off receipt, and lifecycle-drain receipt. It derives and deploys the embedded v0.0.17 Fly baseline, verifies every Machine digest, and keeps B. State clearly that existing legacy org email may work while autonomous onboarding is unavailable.

## Definition of Done

- [ ] Every Phase 0 proof gate is real and green.
- [ ] Artifact A crossed Cloud 8→9 through the dedicated deploy-before-migrate workflow and produced a signed receipt.
- [ ] Artifact B is recorded as the permanent feature-complete `[8,9]` rollback floor.
- [ ] Artifact C runs `[9,9]`; web, reconciler, and migrate independently refuse Cloud 8 before side effects.
- [ ] auth.nerve.email serves correct public metadata/JWKS/token behavior.
- [ ] Client-credentials-only metadata passes the pinned consumers with the Errata 7793 omission; numerical request limits, versioned error extension, permanent key thumbprints, and JTI skew retention are exact and tested.
- [ ] A pre-registered private-key JWT client obtains separate generation-bound onboarding and org tokens without a human tenant flow, including maintenance after initial token expiry.
- [ ] Token issuance is linearized with close/revoke: close leaves no email authority but permits own-generation onboarding polling, while `revoke-client` makes both token kinds unusable and no path orphans a generation.
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
- [ ] Runtime candidate/Fly mirror/v0.0.17 baseline, A/B/C, all shipped mutating binaries, SDK, contract, policy, schema, transition evidence, and CI identities are bound by verified signed provenance without a self-hash/rebuild cycle.
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
