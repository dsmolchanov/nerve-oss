# Nerve

Nerve is an Apache-2.0 email agent runtime, powered by the NeuralMail engine.
It reads an inbox through JMAP and exposes MCP tools to list, read, search and
reply to email. Run it with your own PostgreSQL, Redis and mail transport, or
start the local Docker environment below.

## Quickstart

Requires Docker with Compose v2, `make`, and `openssl`. From a checkout of this
repository, run these three commands. The first creates private random credentials
and refuses to overwrite an existing `.env`; keep your existing file on later runs.

```sh
( set -eu; umask 077; set -C; key=$(openssl rand -hex 32); db=$(openssl rand -hex 32); mail=$(openssl rand -hex 32); printf 'NERVE_API_KEY=%s\nPOSTGRES_PASSWORD=%s\nSTALWART_PASSWORD=%s\n' "$key" "$db" "$mail" > .env )
make up
make mcp-test
```

Go is not required for these container commands. MCP is at
`http://localhost:8088/mcp` and requires the bearer token in `.env`'s `NERVE_API_KEY`.
The [Mailpit viewer](http://localhost:8025) captures outbound messages. The default
sandbox starts the runtime with its outbox worker, PostgreSQL and persistent Redis.
It does not receive Internet email.

For real local inbound ingestion, start Stalwart and seed its inbox:

```sh
docker compose --profile full up -d --wait
make seed-full
# Allow up to 30 seconds for JMAP polling.
make mcp-test
```

`make seed` targets Mailpit's viewer only; it does not populate the runtime inbox.
See [self-hosting](docs/SELF_HOSTING.md) for seed markers, local ports, authentication,
your own SMTP/JMAP server, persistent volumes, upgrades and tested backup/restore.

## Self-host and Cloud

| Responsibility | Self-host runtime | Cloud operating model |
| --- | --- | --- |
| Runtime, database and upgrades | Operated by you | Operated by the service |
| Mail transport | Your JMAP/SMTP server or configured provider | Managed mail operations |
| Domains and delivery | You configure DNS, TLS and sender authentication | Hosted domain onboarding and operations |
| Local agent access | Owner bearer key or inbox-scoped local keys | Organization and machine-client access |
| Backup and recovery | You operate and verify recovery | Service-managed operations |

Cloud access follows the hosted onboarding process. This repository's local
quickstart does not require a subscription or license key. Hybrid onboarding is
not part of this quickstart.

## MCP tools and current limits

- `list_threads`, `get_thread`, `search_inbox`: read and search email. Default search
  uses PostgreSQL full-text search; Qdrant is optional.
- `send_reply`, `compose_email`: enqueue mail through the configured outbound
  transport. Use an idempotency key for retries.
- `triage_message`, `extract_to_schema`, `draft_reply_with_policy`: exposed in the
  contract, but the default noop provider returns `ai_unavailable`. The OpenAI and
  Ollama adapters in this version are placeholders; selecting them does not enable AI.

See the [MCP contract](docs/MCP_Contract.md) for protocol profiles and schemas.
The runtime binary is `nerve-runtime`; `neuralmaild` remains its legacy alias in
the container. Go module and package paths still use `neuralmail`.

## Configuration and development

[Configuration reference](docs/CONFIGURATION.md) lists the runtime environment
variables and their YAML fields. [.env.example](.env.example) covers the variables
consumed by the local Compose stack. An arbitrary variable added to `.env` is not
automatically forwarded into a container: use an explicit Compose override.

- `make up`, `make down`, `make logs`: manage the local stack; `down` preserves volumes.
- `make mcp-test`: check the container's MCP endpoint.
- `make build`, `make test`, `make lint`: build binaries, run Go tests and check formatting/vet.
- `make run`: run the host binary using `CONFIG` (requires configured host-accessible services).
- `make selfhost-smoke`: run the isolated SMTP/JMAP, persistence and restore checks.

See [CONTRIBUTING](CONTRIBUTING.md), [security reporting](SECURITY.md),
[community conduct](CODE_OF_CONDUCT.md), and the [changelog](CHANGELOG.md).

## License

Runtime code is [Apache-2.0](LICENSE); see [NOTICE](NOTICE). Separately distributed
containers and dependencies retain their own licenses. Stalwart is a separate
mail-server dependency, not part of the Apache-licensed runtime.
