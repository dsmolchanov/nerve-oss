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

Configure `NERVE_LLM_PROVIDER=openai` with `NERVE_OPENAI_API_KEY`, or
`NERVE_LLM_PROVIDER=ollama` with `NERVE_OLLAMA_URL`, and set `NERVE_LLM_MODEL`
to a model supported by that provider. Restart the runtime after configuration.
Provider selection does not verify connectivity or model quality; evaluate it
before relying on AI output. A cloud subscription does not configure an LLM.

With the default noop embeddings, inbox search uses PostgreSQL full-text search.
It remains functional without an LLM; semantic/vector search requires a real
embedding provider and vector store. The tool-cost YAML is unchanged by this fix.
