# Runtime security boundaries

The hosted `/mcp` endpoint validates `Origin` before authentication, authenticates
exactly once, and routes only an exact `MCP-Protocol-Version`. Browser callers
must use an allowlisted HTTPS origin. Native clients may omit `Origin` only when
their typed credential is allowed by the runtime policy; a hostile or malformed
origin is rejected even when credentials are otherwise valid.

OAuth machine tokens use issuer-owned PS256 keys obtained from the configured
HTTPS JWKS endpoint. The runtime follows no JWKS redirects, limits response size
and request time, honors freshness for at most five minutes, and permits bounded
stale use only for a key that was already known. An unknown `kid` forces one
refresh and fails closed if the refreshed set does not contain it. Current and
next issuer keys may overlap during rotation. Registered client-assertion keys
are separate authority data and are never loaded into this verifier.

The runtime serves identical protected-resource metadata at
`/.well-known/oauth-protected-resource` and
`/.well-known/oauth-protected-resource/mcp`. Bearer challenges point to the
versioned path; both representations advertise the canonical runtime resource,
authorization server, scope order, and header-only bearer transport with a
stable ETag and five-minute revalidation policy.

Legacy HS256 tokens, Cloud API keys, and the explicitly configured bootstrap key
remain distinct typed principals. Algorithm or header metadata never selects a
different principal kind, and routed protocol metadata is checked against the
initialize body before session creation or tool dispatch.

The MCP 2026 billing boundary is capability- and profile-gated. The runtime
registers `nerve_billing_subscribe` only for a configured `m2m_org` principal
with a complete org/client/generation binding, `m2m_bearer` authentication, and
the exact `nerve:billing.subscribe` scope. Its public input is only a bounded
plan code and idempotency key. Client/org/generation authority and Stripe
customer, payment-method, and mandate references travel out-of-band from the
authenticated principal and protected registration; caller overrides are
rejected. Results expose only closed payment-readiness states, never provider
bodies, hosted action URLs, client secrets, card data, or payment identifiers.
An unconfigured provisioner is hidden and denied, unknown failures are
sanitized, and `requires_action` never enables compose.
