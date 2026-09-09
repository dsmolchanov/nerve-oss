# Changelog

## Unreleased

- Add the Apache-2.0 license and NOTICE files.
- Make CLI MCP smoke validate protocol negotiation, sessions, authentication and errors.
- Run the outbox worker with the self-host server, with bounded graceful shutdown.
- Return explicit errors for unavailable AI and incomplete outbound configuration.
- Bundle migration and configuration resources for standalone binaries.
- Add local owner/inbox-scoped authentication and mailbox isolation.
- Add local Compose profiles, persistent data, backup/restore and end-to-end smoke.
- Document self-host setup, runtime configuration and contribution/reporting paths.

## [0.0.18](https://github.com/dsmolchanov/nerve-oss/releases/tag/v0.0.18) — 2026-08-30

- Introduce typed M2M principals and prepare the OAuth authority transition.
- Add MCP origin/version routing and the stateless MCP 2026 adapter.
- Add phase-zero release proof artifacts and pin the toolchain to Go 1.25.13.
- Fix outbound limits to use database time and verify usage replay namespaces.

[Full release comparison](https://github.com/dsmolchanov/nerve-oss/compare/v0.0.17...v0.0.18).
The self-host work listed under Unreleased is not included in the v0.0.18 image.
