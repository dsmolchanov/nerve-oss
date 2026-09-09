# Self-hosting

The Compose stack below is a local development environment. It binds published
ports to loopback and captures outbound mail instead of delivering to the Internet.

## Compose quickstart

Requires Docker with Compose v2, `make`, and `openssl`. Go is unnecessary for the
container CLI. From the repository root:

```sh
umask 077
printf 'NERVE_API_KEY=%s\nPOSTGRES_PASSWORD=%s\nSTALWART_PASSWORD=%s\n' \
  "$(openssl rand -hex 32)" "$(openssl rand -hex 32)" "$(openssl rand -hex 32)" > .env
docker compose up -d --build --wait
make mcp-test
```

Create `.env` once; do not overwrite an existing installation's credentials.
`.env.example` lists the Compose variables and host ports. PostgreSQL passwords
in this example are hexadecimal so they can safely appear in the connection URI.
Keep `.env` private and back it up separately. It is excluded from Git and Docker
build context. The owner MCP token is available in `.env` as `NERVE_API_KEY`.

The default sandbox starts PostgreSQL 16, Redis 7 with AOF persistence, the runtime
with its outbox worker, and Mailpit. Open <http://localhost:8025> for the mail viewer;
MCP is <http://localhost:8088/mcp>. PostgreSQL and Redis have no published host port.
`make seed` sends five sample messages to Mailpit; they are visible in the viewer
but **are not runtime inbox messages**: Mailpit has no JMAP interface.

For actual inbound ingestion, enable the local `full` profile:

```sh
docker compose --profile full up -d --wait
make seed-full
# Polling runs every 30 seconds; wait for the inbox to populate.
make mcp-test
```

`full` adds Stalwart pinned to v0.11.8, matching the checked-in TOML. Local inbound
SMTP is `127.0.0.1:2526` (container port 25), JMAP is `127.0.0.1:8080`, submission
is `127.0.0.1:1587` (587), and IMAPS is `127.0.0.1:1993` (993). The `dev` and
`admin` accounts use `STALWART_PASSWORD`; `dev@local.nerve.email` is the test inbox.
The local SMTP ingestion listener has TLS disabled; submission/IMAPS use the
server's development certificate. Do not expose this test configuration publicly.
Replies go to Mailpit, so testing cannot send them to external recipients.

`seed` and `seed-full` share `/tmp/nerve-seed.done` inside the runtime container.
If you seeded Mailpit before enabling full, remove that marker before `seed-full`:
`docker compose exec cortex rm -f /tmp/nerve-seed.done`. Removing it permits another
five messages. Recreating the container also removes the marker.

A public mail server needs a separately maintained Stalwart deployment with a
supported release and production configuration: domain/MX pointing to its public
address, matching PTR set by the IP provider, reachable port 25, valid TLS,
submission authentication, and SPF/DKIM/DMARC. The local profile does not provision
DNS, certificates or sender reputation. Follow the [Stalwart deployment guide](https://stalw.art/docs/install/).
Configure the runtime's JMAP and SMTP settings for that server rather than
publishing this development profile.

`--profile vector` additionally starts Qdrant with a persistent volume. Leave it
off with the default `NERVE_EMBED_PROVIDER=noop`; PostgreSQL full-text search works
without it. Starting Qdrant alone does not enable a working embedding provider.

`configs/dev/cortex.yaml` uses Compose service names; `configs/dev/host.yaml` is
for a host binary with separately supplied endpoints. Use `make mcp-test-host`
with `NERVE_API_KEY` and `CONFIG` for that mode. The default stack does not publish
PostgreSQL or Redis; provide those services separately or use a local override.

## Persistence and backup/restore

PostgreSQL (including outbox and idempotency records), Redis AOF, Mailpit and
Stalwart data live in named volumes. `docker compose restart` and `down` preserve
them. `docker compose down -v` deletes those volumes and their data. Keep the same
Compose project name when restarting an installation; a new project gets new volumes.

For a consistent application snapshot, stop the runtime and any separately running
workers before dumping PostgreSQL. Preserve the source mailbox and prevent inbound
changes during a coordinated full-system backup. These scripts back up PostgreSQL
only; they do not back up Stalwart raw mail, Redis jobs, Qdrant, external object
storage, `.env` or custom configuration.

```sh
mkdir -p backups
docker compose stop cortex
scripts/selfhost/backup.sh backups/runtime.dump
docker compose start cortex
```

The script creates a private custom-format `pg_dump` file and refuses to overwrite
an existing file. A failed dump is incomplete: remove it before retrying. Do not
resume writers until the snapshot is finished. Keep backups off-host and periodically
verify recovery; a volume is not a backup.

Restore into a **new** database on the same PostgreSQL service:

```sh
scripts/selfhost/restore.sh backups/runtime.dump restore_verify
```

The target must begin with `restore_` and must not already exist. `pg_restore` uses
a single transaction and stops on errors. On failure the newly created database
remains for inspection; the active database is unchanged. Test it with an isolated
runtime pointing `NERVE_DB_DSN` at `restore_verify`, with `serve --with-worker=false`,
and compare `list_threads` with the source. Keep transport ingestion disabled in the
verification runtime. Never run two workers against independent restored snapshots
of the same mail queue: that can redeliver queued messages.

After verification, a deliberate cutover requires stopping all writers, configuring
the chosen restored database, and reconciling external delivery since the snapshot.
Restoring an older outbox cannot undo mail already accepted by an SMTP server.

`make selfhost-smoke` creates an isolated disposable Compose project with generated
credentials. It tests sandbox MCP/auth, SMTP→JMAP ingestion, a queued reply across
restart, idempotent delivery into Mailpit, Redis persistence, backup/restore with
identical `list_threads`, and migrations from an empty working directory. It removes
only its own containers and volumes. CI runs this with named volumes. On Docker
Desktop with a full VM disk, `python3 scripts/ci/selfhost_smoke.py --bind-data` uses
persistent host directories instead; this is a separate storage-backend check.

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
key can access all local inboxes and `/debug`. Cloud mode does not mount
`/debug` and rejects direct access to its handler, including authenticated users.

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

Mailbox-scoped reads require message and thread inboxes to agree. Inconsistent
historical rows are excluded from thread/search results and denied by direct
message resources; this does not delete or repair them. Local vector results
use the database for ownership and displayed content, so a stale index cannot
supply another inbox's snippet or thread identifier.

## Updating a deployment

Choose a reviewed commit or release compatible with your current schema and
configuration. The local Compose stack builds the checked-out source; `docker
compose pull` alone does not update the runtime. Record your current commit,
configuration, image and schema versions before changing them.

1. Read the target's release notes and migration window. Test the update against
   an isolated restored database with outbound workers disabled.
2. Stop the runtime and any separate workers; make and verify a fresh backup using
   the procedure above. Back up the other stores and configuration as required.
3. Check out the chosen ref, review changes to Compose/YAML and preserve your private
   credentials. Run `docker compose up -d --build --wait` with the same project name
   and the profiles you use (include `--profile full` before `up` for local Stalwart).
4. Run `make mcp-test` and verify a synthetic receive/reply through your transport.
   Check queued/failed deliveries and logs before resuming normal traffic.

Startup applies/verifies only its supported migration window. An incompatible
schema is a reason to stop and investigate, not to disable verification. Returning
to an older image after migrations is not a guaranteed rollback. Restore into a
separate database and reconcile deliveries before any cutover; old snapshots may
contain messages that were already sent. Do not run down migrations blindly.
