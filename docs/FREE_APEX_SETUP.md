# Modern Free apex setup

This prospective contract is separate from ordinary trial/managed onboarding and
is available only to an already admitted machine client on the modern Cloud MCP
profile. Free does not approve or activate a pending external client. Public Free
admission and expense acceptance remain release prerequisites.

| Tool | Exact input | Purpose |
| --- | --- | --- |
| `nerve_free_setup` | `idempotency_key`, `organization_name`, `apex_domain`, `local_part` | Create or replay a zero-mail setup generation |
| `nerve_free_status` | `{}` | Read the caller's retained setup history |
| `nerve_free_verify_domain` | `{}` | Prove apex ownership before provider provisioning, then poll mail DNS readiness |
| `nerve_free_resume` | `idempotency_key` | Explicitly request or replay Free after terminal paid cleanup on the same retained apex |
| `nerve_free_close` | `idempotency_key`, `expected_generation` | Close this generation, preserving unknown outcomes and history |

Inputs are closed objects. Caller identity, organization, owner, entitlement,
provider IDs, generation authority and proof timestamps are not input fields.
The close generation must equal the authenticated caller generation. Setup
requires an exact ICANN registrable apex after IDNA/PSL canonicalization; arbitrary
subdomains, private/public/unknown suffixes and IP addresses are refused. One
client has one bounded pending setup with its original 24-hour deadline.

The original `nerve:onboarding` bearer is forwarded unchanged to the fixed-origin
control plane. The runtime signs the path/body/client/generation/token delegation;
the control plane authenticates that bearer independently, checks the live issuing
key, compares its exact tuple and consumes a nonce before execution. Local API
keys, organization/email-scoped tokens and legacy MCP cannot invoke these tools.

Success is a closed object containing `resultType: complete`, canonical UUID
`setup_id` and `onboarding_id`, positive `generation`, exact `apex_domain`,
`setup_state` (`pending`, `proof_verified`, `terminal`), lifecycle `state`
(`provisioning`, `dns_pending`, `active`, `deprovisioning`, `closed`), original
`setup_expires_at`, `ownership_txt`, `next_action`, and boolean `reauthorize`.
An optional `dns_records` array has at most 32 records with only `type`, `name`,
`value`, optional MX `priority`. Internal TTL/purpose/provider-state metadata is
not included. Wire requests and replies are bounded to 64 KiB.

Ownership TXT is exactly `_nerve-verify.<apex>` with a fresh immutable
`nerve-free-verification=` challenge. Publish it, then verify. A successful TXT
lookup alone grants neither inboxes nor mail allowance. Provider provisioning and
complete mail DNS/provider readiness precede atomic Free lease activation.
Pending setup responses have no `address` and `reauthorize` is false. Mail DNS
instructions may also include the domain lifecycle's separate ownership TXT;
preserve both required values when they share a DNS name.

`next_action` is one of `configure_ownership_dns_then_verify`,
`wait_for_domain_setup`, `configure_mail_dns_then_verify`, `reauthorize_org`,
`poll_close`, or `closed`. An active result alone may contain a canonical mailbox
address on the exact apex; it requires `reauthorize: true` and
`next_action: reauthorize_org`. The agent obtains the existing organization-scoped
bearer through the existing OAuth flow before accessing mail. Closed generations
retain their setup identity/deadline and cannot be restarted by replay.

Business errors retain the existing closed onboarding error-code vocabulary.
After a mutation may have reached the control plane, transport, timeout,
redirect, body-read, oversized, malformed or semantically invalid response means
`onboarding_outcome_unknown`. No automatic mutation retry occurs. Poll
`nerve_free_status` for that same client/generation before deciding whether to
explicitly replay the exact saved input/key. Status is read-only and reports a
retryable temporary error for an invalid upstream response.

The Cloud admission path additionally requires the audited global
`free_catalog_published` decision and existing bounded admission/inventory gates.
Missing or withdrawn publication refuses new starts; retained status and cleanup
remain available. This contract changes no deployed lock or publication decision.

## Explicit return after paid cleanup

`nerve_free_resume` uses the original onboarding bearer and one exact saved
idempotency key. It is separate from setup/status/verify replay. Terminal paid
cleanup, the retained physical lease, fresh exact provider GET and ownership TXT,
three compatible active inboxes, one apex and at most one GB are required. Old
used/unknown outcome history remains in the same monthly pool; cancel/resubscribe
cannot grant a new 300-recipient pool. No domain/provider resource is created.

A completed current return additionally reports both `return_receipt_id` (a
canonical UUID) and `return_idempotency_key` (the exact saved key). Both are
optional for ordinary setup history but must appear together only on an active,
proof-verified result. A resume response requires both and exact input-key
correlation. A retained root's active resource label alone is not proof that
Free resumed: it can remain active while a paid subscription is read-only.

After an ambiguous resume result, poll `nerve_free_status` and check the return
receipt and exact saved key before reauthorizing. Never invent a new key to retry.
Explicit replay of the same committed decision does not need another provider
reading and grants no additional allowance. A newer paid period, revoked issuing
key, changed generation or DNS loss fails closed. This prospective contract does
not publish Free or accept its economic/manual release gates.
