# Runtime configuration

The binary starts with `config.Default()`, loads YAML selected by `NERVE_CONFIG`,
then applies nonempty environment overrides. A missing YAML file leaves defaults
in place; malformed YAML or other read errors fail. Prefer `NERVE_*`; corresponding
`NM_*` aliases listed below remain supported with a deprecation warning. A nonempty
modern value takes precedence. Empty environment values generally do not clear YAML
values; edit the YAML when a credential or allowlist must be removed.

The field names below are Go config fields, as defined in
[`internal/config/config.go`](../internal/config/config.go). YAML uses the snake_case
keys shown in [`configs/dev/cortex.yaml`](../configs/dev/cortex.yaml). The runtime
supports these settings; not every provider or cloud capability is implemented or
available to a self-hosted instance.

## Compose versus host process

[`.env.example`](../.env.example) lists the Compose inputs: generated credentials,
published host ports, profiles and embedding selection. Compose reads `.env` for
interpolation; it forwards only variables declared under service `environment`.
Adding `NERVE_SMTP_HOST` to `.env` alone does not change the container SMTP host.
Set it explicitly through a local Compose override or use a custom YAML mount.

The host binary uses its process environment and `NERVE_CONFIG`/`NM_CONFIG`.
It does not automatically read `.env`. Its default bind is `127.0.0.1:8088`;
the container sets `0.0.0.0:8088` and requires a configured key.

## Loader variables

Every `NERVE_*` variable in this table has the corresponding `NM_*` alias.
Secrets have no documented sample value: supply your own key/password privately.

| Environment variable | Config field |
| --- | --- |
| `NERVE_HTTP_ADDR` | `HTTP.Addr` |
| `NERVE_HTTP_READ_TIMEOUT` | `HTTP.ReadTimeout` |
| `NERVE_MEMORY_BUDGET_BYTES` | `Memory.BudgetBytes` |
| `NERVE_DEV_MODE` | `Dev.Mode` |
| `NERVE_CLOUD_MODE` | `Cloud.Mode` |
| `NERVE_CLOUD_PUBLIC_BASE_URL` | `Cloud.PublicBaseURL` |
| `NERVE_AUTH_ISSUER` | `Auth.Issuer` |
| `NERVE_AUTH_AUDIENCE` | `Auth.Audience` |
| `NERVE_AUTH_JWKS_URL` | `Auth.JWKSURL` |
| `NERVE_BILLING_PROVIDER` | `Billing.Provider` |
| `NERVE_STRIPE_SECRET_KEY` | `Billing.StripeSecretKey` |
| `NERVE_STRIPE_WEBHOOK_SECRET` | `Billing.StripeWebhookSecret` |
| `NERVE_METER_TOOL_COST_PATH` | `Metering.ToolCostPath` |
| `NERVE_METER_PAST_DUE_GRACE_DAYS` | `Metering.PastDueGraceDays` |
| `NERVE_JMAP_URL` | `JMAP.URL` |
| `NERVE_JMAP_SESSION_URL` | `JMAP.SessionURL` |
| `NERVE_JMAP_ACCOUNT_ID` | `JMAP.AccountID` |
| `NERVE_JMAP_USERNAME` | `JMAP.Username` |
| `NERVE_JMAP_PASSWORD` | `JMAP.Password` |
| `NERVE_JMAP_PUSH_SECRET` | `JMAP.PushSecret` |
| `NERVE_JMAP_POLL_INTERVAL` | `JMAP.PollInterval` |
| `NERVE_SMTP_HOST` | `SMTP.Host` |
| `NERVE_SMTP_PORT` | `SMTP.Port` |
| `NERVE_SMTP_USERNAME` | `SMTP.Username` |
| `NERVE_SMTP_PASSWORD` | `SMTP.Password` |
| `NERVE_SMTP_FROM` | `SMTP.From` |
| `NERVE_SMTP_REQUIRE_STARTTLS` | `SMTP.RequireStartTLS` |
| `NERVE_SMTP_HELO_DOMAIN` | `SMTP.HeloDomain` |
| `NERVE_RESEND_API_KEY` | `Resend.APIKey` |
| `NERVE_RESEND_BASE_URL` | `Resend.BaseURL` |
| `NERVE_DB_DSN` | `Database.DSN` |
| `NERVE_QDRANT_URL` | `Qdrant.URL` |
| `NERVE_QDRANT_COLLECTION` | `Qdrant.Collection` |
| `NERVE_EMBED_DIM` | `Qdrant.EmbedDim`, `Embedding.Dim` |
| `NERVE_REDIS_URL` | `Redis.URL` |
| `NERVE_OBJECT_STORE_URL` | `ObjectStore.URL` |
| `NERVE_OBJECT_STORE_BUCKET` | `ObjectStore.Bucket` |
| `NERVE_OBJECT_STORE_ACCESS_KEY` | `ObjectStore.AccessKey` |
| `NERVE_OBJECT_STORE_SECRET_KEY` | `ObjectStore.SecretKey` |
| `NERVE_EMBED_PROVIDER` | `Embedding.Provider` |
| `NERVE_EMBED_MODEL` | `Embedding.Model` |
| `NERVE_LLM_PROVIDER` | `LLM.Provider` |
| `NERVE_LLM_MODEL` | `LLM.Model` |
| `NERVE_OPENAI_API_KEY` | `LLM.OpenAIKey` |
| `NERVE_OLLAMA_URL` | `LLM.OllamaURL` |
| `NERVE_LLM_PROMPT_PATH` | `LLM.PromptPath` |
| `NERVE_POLICY_PATH` | `Policy.DefaultPath` |
| `NERVE_MCP_PROTOCOL_VERSION` | `MCP.ProtocolVersion` |
| `NERVE_MCP_ALLOW_ORIGINS` | `MCP.AllowOrigins` |
| `NERVE_ONBOARDING_CONTROL_PLANE_URL` | `Onboarding.ControlPlaneURL` |
| `NERVE_ONBOARDING_DELEGATION_KEY_ID` | `Onboarding.DelegationKeyID` |
| `NERVE_ONBOARDING_DELEGATION_SECRET` | `Onboarding.DelegationSecret` |
| `NERVE_ONBOARDING_TIMEOUT` | `Onboarding.Timeout` |
| `NERVE_HYBRID_STATE_PATH` | `Hybrid.StatePath` |
| `NERVE_HYBRID_POLL_INTERVAL` | `Hybrid.PollInterval` |
| `NERVE_HYBRID_TIMEOUT` | `Hybrid.Timeout` |
| `NERVE_API_KEY` | `Security.APIKey` |
| `NERVE_ALLOW_OUTBOUND` | `Security.AllowOutbound` |
| `NERVE_ALLOW_SEND_WITH_WARNINGS` | `Security.AllowSendWithWarnings` |
| `NERVE_OUTBOUND_DOMAIN_ALLOWLIST` | `Security.OutboundDomainAllowlist` |
| `NERVE_LOG_LEVEL` | `Log.Level` |

## Additional controls

| Variable / setting | Behavior |
| --- | --- |
| `NERVE_CONFIG` / `NM_CONFIG` | YAML file path; modern name takes precedence. |
| `NERVE_TOKEN_SIGNING_KEY` / `NM_TOKEN_SIGNING_KEY` | Legacy token signing key. |
| `NERVE_ALLOW_UNAUTHENTICATED` | Explicit unsafe non-loopback opt-in; accepts exactly `true` or `false`. Configured bearer keys remain enforced. |
| `NERVE_MIGRATIONS_DIR` | Custom migration root containing `core/` and `cloud/`; invalid overrides fail. |
| `NM_MIGRATE_ON_START` | Startup migration policy; see the startup implementation and schema window before overriding. |
| `security.local_api_keys` (YAML only) | List of `token` and `inbox_ids` entries for inbox-scoped HTTP access. |

`NERVE_API_KEY` is the unrestricted local owner key. `NERVE_JMAP_PUSH_SECRET` protects
JMAP push notifications. `NERVE_ALLOW_OUTBOUND` defaults false; external recipients
also have to satisfy `NERVE_OUTBOUND_DOMAIN_ALLOWLIST` when set. SMTP TLS/auth must
match your relay. See [SELF_HOSTING](SELF_HOSTING.md) for these boundaries and examples.

Lists use comma-separated values. Duration values use Go syntax such as `30s`.
Most boolean overrides accept true/false, 1/0, yes/no or on/off; invalid values
retain the existing value. Numeric/duration validation varies by field; malformed
values do not constitute a readiness check. Verify the effective deployment with
MCP and a real transport test.

Default LLM and embeddings are `noop`. Changing provider, model, URL or key does
not turn the placeholder OpenAI/Ollama adapters into working AI. Cloud auth,
billing, metering and onboarding variables are configuration surfaces, not an
assurance that the corresponding hosted service is provisioned.
