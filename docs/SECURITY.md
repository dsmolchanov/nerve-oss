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

Legacy HS256 tokens, Cloud API keys, and the explicitly configured bootstrap key
remain distinct typed principals. Algorithm or header metadata never selects a
different principal kind, and routed protocol metadata is checked against the
initialize body before session creation or tool dispatch.
