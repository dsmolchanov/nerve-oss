# Nerve (powered by NeuralMail OSS engine)

Nerve is an open-source email agent runtime: JMAP-first inboxes, vector memory, and workflow-grade MCP tools. The repo name remains `neuralmail`, while the product brand is **Nerve**.

## Quickstart

```bash
make up
make seed
make mcp-test
```

By default, the Make targets use `configs/dev/host.yaml` (localhost ports). To run against a different config:
```bash
CONFIG=configs/dev/cortex.yaml make seed
```

Then open:
- `http://localhost:8088/healthz`
- `http://localhost:8088/debug`

## What You Get
- Local inbox stack: Stalwart + Postgres + Redis + Qdrant + MinIO
- MCP tools: list/get/search, triage, extract, draft, send
- Policy guardrails and audit logging
- A static landing page in `site/`

## MCP Tools
- `list_threads`
- `get_thread`
- `search_inbox`
- `triage_message`
- `extract_to_schema`
- `draft_reply_with_policy`
- `send_reply`
- `compose_email`

See `docs/MCP_Contract.md` for schemas.

## Developer Experience
- `make up`: start local stack
- `make seed`: send dramatic demo emails (outage + refund)
- `make mcp-test`: validate MCP endpoint
- `make doctor`: connectivity checks

## Configuration
Defaults live in `configs/dev/cortex.yaml`. Environment variables override config.

Key env vars (preferred):
- `NERVE_JMAP_URL`
- `NERVE_DB_DSN`
- `NERVE_QDRANT_URL`
- `NERVE_REDIS_URL`
- `NERVE_SMTP_HOST`
- `NERVE_POLICY_PATH`

Legacy aliases are still supported during migration:
- `NM_JMAP_URL`
- `NM_DB_DSN`
- `NM_QDRANT_URL`
- `NM_REDIS_URL`
- `NM_SMTP_HOST`
- `NM_POLICY_PATH`

## Repo Split Transition
- Runtime pin for cloud deploys: `deploy/cloud/runtime.lock`
- Cloud deploy order (core migrations -> cloud migrations -> deploys): `scripts/deploy/cloud_deploy.sh`
- Local two-repo loop guide: `docs/TWO_REPO_DEV_LOOP.md`

## License
- NeuralMail code: Apache-2.0
- Stalwart Mail Server: AGPLv3 (separate container dependency)

## Branding
The OSS runtime service is `nerve-runtime` (legacy binary alias: `neuralmaild`); the product is **Nerve** (`nerve.email`).
