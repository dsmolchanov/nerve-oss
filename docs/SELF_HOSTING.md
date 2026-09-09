# Self-hosting: outbound configuration

This guide currently covers outbound configuration and worker lifecycle. The
complete Compose quickstart and backup/restore guide are still being implemented.

## Outbound configuration

Set `NERVE_SMTP_HOST` and `NERVE_SMTP_PORT` to your SMTP relay, and configure
`NERVE_SMTP_USERNAME`, `NERVE_SMTP_PASSWORD`, and `NERVE_SMTP_REQUIRE_STARTTLS`
as required by that relay. Host/port validation does not prove that the relay
is reachable or that its credentials work.

External recipients require `NERVE_ALLOW_OUTBOUND=true`. If
`NERVE_OUTBOUND_DOMAIN_ALLOWLIST` is set, the recipient must also match it.
The existing `local.nerve.email` sandbox exception is preserved. A policy
refusal is not a request to buy a cloud subscription.

Local setup errors expose `code`, `retryable: false`, and this guide's path in
`remediation`. Correct the configuration before retrying. A Resend-backed inbox
uses its configured outbound provider; it does not require an SMTP host.

## Worker lifecycle

`neuralmaild serve` starts the outbox worker by default in OSS mode. Set
`--with-worker=false` when running the separate `neuralmaild worker` process.
Cloud mode keeps the previous default (a separate worker); an explicit
`--with-worker=true` opts in to a combined process.

On SIGTERM/SIGINT, the runtime stops loops, waits for the in-flight delivery
and its database acknowledgement, then closes the shared store. An in-flight
worker delivery has a one-minute deadline; the whole claimed batch, including
acknowledgements and returning unstarted messages, has a shared 70-second limit.
Failed requeues are reported to the process; their leases remain until stale-claim
recovery. Allow sufficient container shutdown time (at least
90 seconds) for the HTTP drain and worker acknowledgement. A hard kill, an
ambiguous SMTP acknowledgement, or a database failure after provider acceptance
can still result in redelivery. SMTP does not provide exactly-once delivery.

Repeating the same tool request with the same idempotency key does not enqueue
another delivery. Changing the payload under that key is a conflict. OSS replies
target inbound messages, so a prior outbound reply does not change the recipient
of an identical retry. Cloud legacy recipient behavior is unchanged.

## AI configuration

The default `noop` LLM cannot classify, extract, or draft. These tools return
`ai_unavailable`, `retryable: false`, and this section's path in `remediation`.
They produce no synthetic result and reserve no tool usage. Cloud authentication
and scope checks still run first. This also applies when an incomplete provider
configuration causes the runtime to select noop.

In this version, the OpenAI and Ollama adapters are also placeholders and return
`provider not implemented`. Setting a provider name, key, URL or model does not
enable working AI. The noop-specific availability check only distinguishes noop
from a selected adapter; it is not a provider health or implementation check.
A working adapter and an evaluation of its output are still required.

With the default noop embeddings, inbox search uses PostgreSQL full-text search.
It remains functional without an LLM; semantic/vector search requires a real
embedding provider and vector store. The tool-cost YAML is unchanged by this fix.

## Local authentication and network binding

The host binary defaults to `127.0.0.1:8088`. Set `NERVE_API_KEY` to an owner
bearer token to require authentication on `/mcp` (both protocol versions).
Missing, invalid or duplicate Authorization headers return HTTP 401. The owner
key can access all local inboxes and `/debug`.

A non-loopback bind requires at least one configured key. The explicit unsafe
opt-in `NERVE_ALLOW_UNAUTHENTICATED=true` permits anonymous exposure; it never
disables authentication when keys are configured. Development mode does not
bypass authentication. Binding to loopback without keys trusts all local users.

The container sets `NERVE_HTTP_ADDR=0.0.0.0:8088`; supply a key and publish only
`127.0.0.1:8088:8088`. Do not place tokens in images or tracked configuration.
For access beyond localhost, terminate TLS at a trusted proxy.

To give agents separate inbox access, configure `security.local_api_keys` in
an untracked YAML file selected by `NERVE_CONFIG`. Each entry has a `token` and
an `inbox_ids` list of actual UUIDs from `email://inboxes`. Empty token/inbox
lists and duplicate tokens are rejected. Keep this file readable only by the
runtime owner. Keys grant read/search/draft/send access within the listed
inboxes; they do not grant owner debug or cloud billing/onboarding access.
Remove a key and restart the runtime to revoke it. The optional `NERVE_API_KEY`
is an unrestricted owner key; do not give that key to a restricted agent.

Stdio runs as the operating-system owner and retains local owner access. Its
process must not be shared as an unauthenticated network gateway. HTTP mailbox
isolation covers tool operations, inbox discovery, thread and message resources.

OAuth metadata endpoints are mounted only in cloud mode. In cloud mode,
`cloud.public_base_url` controls the resource/challenge URL and `auth.issuer`
controls the authorization server; unset values retain the existing defaults.
OSS 401 responses advertise a plain Bearer challenge without a cloud URL.

## Bundled defaults and overrides

The binary contains policy YAML, prompt assets, meter costs, JSON schemas and
separate core/cloud migration sets. It can start from an empty working directory
without a source checkout. Prompt assets are packaged; current placeholder LLM
adapters do not execute them.

Existing files at configured paths override bundled defaults. Only canonical
`configs/...` paths fall back to bundled assets when the file is absent; missing
custom absolute paths and unreadable files do not select another policy. Meter
loader behavior on invalid custom files retains its existing fallback costs.

`NERVE_MIGRATIONS_DIR` overrides the migration root, which must contain `core/`
and `cloud/`. An invalid override fails rather than selecting bundled migrations.
Startup still respects the compiled schema window and cloud migration policy.
