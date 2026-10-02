# Qualify published hybrid image rotation

repository: nerve-oss
status: in-progress

The active-image smoke already proves pairing, inbound ACK, one reply, and
backup/restart behavior against a loopback Cloud fixture. It does not exercise
the operator's two-phase key rotation on the published image. Extend that
single qualification flow without changing the published image or production
configuration.

Change `scripts/ci/hybrid_active_image_smoke.py`, its focused tests, and the
smoke description in `docs/SELF_HOSTING.md` only.
The fixture must bind issued bearer tokens to the asserted key and distinguish
an admitted key from the key currently active for an installation. The smoke
prepares a replacement while the old key remains active, proves commit is
refused before admission and after admission but before owner switch, then
switches the synthetic installation, commits, restarts the runtime, observes a
poll with the replacement key, and confirms the settled inbound/reply were not
replayed. Preserve the existing backup/restore and post-restart checks.

Acceptance: deterministic fixture tests cover the admitted/active distinction;
the isolated exact published v0.0.25 digest smoke passes with Core31 schema;
normal CI, current-head review and branch protection pass. This is synthetic
self-host qualification, never a production owner/provider canary. No workflow
changes, release publication, or Cloud deployment belong in this change.
