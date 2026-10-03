# Modern MCP inbound recovery candidate

This source contract belongs to pricing-v2 slice C. It does not publish a paid
or Free offer, admit a new machine client, or change the frozen legacy contract.

An active, generation-bound `m2m_org` bearer with `nerve:email.read` may use three
modern tools when the configured signed control plane is available:

| Tool | Arguments | Result |
| --- | --- | --- |
| `nerve_inbound_usage` | `{}` | Current admitted period, reserved/materialized slots, soft/hard limits, receipt overflow and completeness |
| `nerve_inbound_receipts` | Optional `limit` 1–100 (default 50), optional opaque `cursor` | Bounded content-free `items` and an optional opaque continuation cursor |
| `nerve_inbound_recover` | Exactly one `receipt_id` | The same receipt identity, durable state/reason, a saved message handle only after materialization, optional attachment reopening count and provider attempt deadline |

Arguments never select an organization, generation, inbox, provider delivery,
owner, payment, or allowance. Receipt IDs are canonical nonzero UUIDs or the
`q_` plus 64 lower-case hex alias for historical quota receipts. Neither kind
confers authority. Local identities and onboarding-only tokens are denied.
These operations remain available at zero outbound allowance and consume no
outbound recipients or commercial tool units.

The runtime retains the original read bearer and signs the exact POST path,
body digest, timestamp and unique nonce using existing delegation credentials.
The control plane independently authenticates both parties, checks the live
machine key and original tenant/generation/token tuple, consumes the nonce and
rechecks the active generation. It invokes the existing inline REST handlers,
with their tenant/lifecycle, provider concurrency, memory, storage and inbound
admission guards. No new body worker, automatic replay, or bulk recovery exists.

Results never contain sender, recipient, subject, body, raw provider ID or
provider diagnostics. Native tools and typed SDK methods validate identity,
state, timestamps, the 30-day attempt interval, page bounds and usage arithmetic.
`provider_created_at + 30 days` is a deadline to attempt recovery, not a guarantee;
an unknown origin omits both timestamp and deadline.

Recovery has no automatic retry. A missing, interrupted or invalid successful
response returns `inbound_outcome_unknown` with `retryable=true`; first read the
receipt list, then explicitly recover the same receipt if still needed. The
existing receipt lease/CAS and idempotent save prevent duplicate materialization.
Other fixed codes are `inbound_invalid_request`, `inbound_unavailable`,
`inbound_retry_later`, `recovery_in_progress_or_changed`,
`recovery_provider_or_save_unavailable` and `recovery_response_exceeds_limit`.

Coordinated source/runtime candidate qualification and the slice C release gate
remain required before exposure. Draft CI and local synthetic provider tests do
not establish a production or provider acceptance verdict.
