# Contributing to Nerve

Start with an issue describing the behavior you need or the bug you can reproduce.
For vulnerabilities, use [SECURITY.md](SECURITY.md) instead of posting exploit details
or credentials in a public issue. Community participation follows our
[Code of Conduct](CODE_OF_CONDUCT.md).

## Local development

Use Go 1.25.13 (the pinned CI toolchain), Git, and `make`. Docker Compose is needed
for integration tests and the local mail stack. Follow the [README](README.md) to
create a private `.env` and start containers.

```sh
make build
make lint
make test
```

Binaries are written to `dist/`: `nerve-runtime`, `neuralmail` (CLI), and
`nerve-migrate`. `make run CONFIG=/absolute/path/to/config.yaml` runs the host
runtime; that file must point to reachable PostgreSQL, Redis and mail endpoints.
The default Docker stack does not publish PostgreSQL or Redis to the host.

Some Go database tests skip without a test database. A green unit test run alone
does not prove self-host readiness. For the complete disposable mail scenario:

```sh
make selfhost-smoke
```

This creates a separate Compose project and deletes only its own test containers
and volumes. See [SELF_HOSTING](docs/SELF_HOSTING.md) for the storage-backend options.
CI additionally runs race suites, runtime/migration boundary checks, pinned MCP
conformance, release-artifact checks and vulnerability scanning.

## Pull requests

Keep changes focused and explain the observed problem, resulting behavior and
validation. Include a reproduction or regression test for correctness/security
fixes. Update examples when behavior changes. Follow [AGENTS.md](AGENTS.md) and
[repository invariants](AGENTS.repo-invariants.md) for review and scope rules.

Do not commit `.env`, tokens, mail dumps, database backups or customer messages.
Use synthetic test data. Never disable auth, tenant isolation or review checks to
make a test pass. Check [sync-manifest.yaml](sync-manifest.yaml) before changing
shared runtime/cloud files; preserve their ownership and compatibility boundaries.

Contributions are submitted under the repository's Apache-2.0 license. Keep
third-party notices and attribution intact.
