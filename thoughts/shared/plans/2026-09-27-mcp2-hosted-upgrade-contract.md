---
date: 2026-09-27
repository: nerve-oss
status: in_progress
topic: MCP 2.0 hosted upgrade and committed paid tier status contract
parent_plan: nerve-cloud/thoughts/shared/plans/2026-09-08-mcp2-pricing-v2.md
---

# Hosted Starter upgrade and status contract for the modern MCP runtime

This is the OSS runtime part of pricing v2 Slice A. It adds a billing-scoped
`nerve_billing_upgrade` and `nerve_billing_status` tools for an active `m2m_org` generation. The caller
provides only an idempotency key. The control plane resolves organization,
generation, owner link, immutable Starter offer and admitted Stripe Price from
the authenticated principal and durable state. The result is an opaque Nerve
owner-consent URL, never a Stripe Session or payment credential.
The status tool takes no caller-selected identifiers and reports the committed
hosted intent state plus a `starter_active` boolean derived from durable paid
projection, not a Checkout redirect. Send permission remains independently fenced.

## Scope and ownership

- `internal/mcp/`: modern-only tool listing, strict input/output schema,
  billing scope and principal gate, no legacy invoker dispatch, safe business
  error translation. The runtime accepts only a URL on its configured HTTPS
  dashboard origin with one canonical opaque intent ID.
- `internal/onboarding/`: signed delegation with the original bearer and exact
  generation tuple, bounded response and strict envelope decoding. A timeout
  is retryable only with the same idempotency key; Cloud's durable open-intent
  lane prevents a second purchase intent.
- `internal/config/`, `internal/app/`: separate dashboard origin from the
  control-plane origin. The tool is absent until the hosted provisioner and
  valid dashboard origin are configured.
- Cloud counterpart: nerve-cloud draft PR #389 implements the authenticated
  delegation endpoints and durable owner/admission intent. This OSS PR must not
  be released to a configured runtime before that endpoint is accepted.

No off-session `nerve_billing_subscribe` behavior, legacy MCP route, payment
profile, policy, quota or paid entitlement changes are in this OSS slice.

## Acceptance

- [x] The tool is hidden without the billing-scoped `m2m_org` principal;
  fields selecting org, owner, Price or Customer are rejected.
- [x] Wrong-generation delegation and local-auth modern calls fail closed;
  the excluded legacy MCP path is untouched.
- [x] The signed delegation carries the authenticated org/generation and
  exact idempotency key; malformed or unbounded replies fail closed.
- [x] Result URL is HTTPS on the configured dashboard origin and contains only
  the opaque intent query; unexpected states and provider errors do not leak.
- [x] Focused MCP, onboarding, app and config tests plus vet pass locally.
- [x] Status rejects caller authority, foreign generation, revoked owner link
  and malformed response; an open Session does not report paid access.
- [ ] Cloud #389 CI/review and deployment prove the matching endpoint and
  owner-consent page before enabling the tool in an admitted cohort.
- [ ] Current-head review and required OSS CI pass before merge.

This document does not mark pricing v2 Gate A or manual Stripe/tenant checks
complete. Its source and release pin advance only through the parent release
process.

## Coordinated paid tier status (parent Slice B)

The strict status result additionally requires `active_tier`, one of empty string, `starter`, `growth`, or `scale`. Empty denotes no currently proved active hosted paid tier; the original intent lifecycle alone does not prove payment. `starter_active` is true exactly for `active_tier=starter`. Any nonempty active tier requires `hosted_state=active`. Native provisioner results and signed delegation share the same consistency validator; no caller may select a tier through the read-only tool. Cloud Starter #389 must emit its proved Starter/empty result, and Cloud B #400 must emit its immutable effective tier, before deploying this mandatory coordinated contract. Neither a mutable legacy plan label nor a redirect authorizes an active result. No compatibility fallback or purchase scope expansion is introduced.

The exact OSS mirror/source and release pins require the combined policy + hosted contract source before a B release. This draft contract remains unexposed until matching Cloud endpoints, current-head checks/review and parent manual release gates pass.
