# Security policy

Report suspected vulnerabilities privately to **support@nerve.email**. Include the
runtime version or commit, affected component, impact, and a minimal reproduction
using synthetic data. Do not send live credentials, mailbox contents or a database
dump. We can arrange a suitable way to share additional sensitive details if needed.

Do not disclose exploit details in public issues before maintainers have assessed
the report. There is no published response-time SLA or guaranteed backport policy;
check release notes and verify fixes in your deployed version.

## Deployment boundaries

The Compose profiles are local test environments with loopback host ports. Their
mail-server configuration is not intended for Internet exposure. Use strong local
keys, inbox-scoped access for agents, and TLS for remote access. Backups and `.env`
contain sensitive state; protect them separately from source code.

See [self-host authentication and backup guidance](docs/SELF_HOSTING.md) and
[hosted runtime security boundaries](docs/SECURITY.md). Current placeholder AI
adapters do not establish safety or policy guarantees for generated replies.
