# Ordinary OSS→Cloud utility collector validation

Plan: ../plans/2026-11-11-sync-cloud-utility-collector.md
Base: accepted OSS eff664a4; normal producer42947126103 terminalFAIL.

The failed producer reached the full Cloud suite and finished Store1192.946s.
Exactly eight mandatory original-six/lifecycle/export/export-status commit and
rollback cases refused because the real statistics collector was not loaded.
No Cloud PR/source pin was produced. Raw9.87MB failure log is retained at
/private/tmp/oss-cloud-production-20261110/oss169-cloud-sync-failed.log.

The new bounded setup uses only the job's exact PG16 service CID/loopback port,
awaits actual TCP SQL readiness, sets preload and restarts only that service.
It discovers/validates the actual host port after restart and positively checks
preload, track top/all and track_utility on before exporting NM_TEST_DB_DSN.
The full build/test no longer overwrites that export with a stale context port.
Full uncached30m Go suite,45m job, required DB, source-base/mirror and normal PR
ordering remain unchanged. Product code, SQL, tests and provider settings do not
change. CID/port/refusal guards explicitly exit, including under Bash3.2 where
compound-command errexit alone did not refuse every local negative case.

Local executable workflow contracts:3PASS/0FAIL, with happy-path exact Go argv
and rebound connection plus12 setup refusal boundaries and CI/guard/order wiring.
The initial Bash3.2 negative failures are retained separately, fixed explicitly
and not countedPASS. Required OSS build-test invokes this same contract file.

Actual same-shell PG16 qualification PASS, not a SQL double. The own temporary
volume preserves auto.conf across Docker restart, matching the service lifecycle.
Local psql transport executes inside the owned public image because the host
has no psql; only the exact synthetic TCP transport is translated, no SQL result.
The exported host port is independently checked and the unmodified Go credential
suite connects through that actual host DSN. All eight original cases pass:
9PASS events/0FAIL/0SKIP, own DB/role inventory0|0, exact owned container and
owner-labeled temporary volume removed and independently confirmed absent.
No cryptographic volume signature is claimed; owner-label/identity are checked.
Proof and raw logs:
/private/tmp/oss-cloud-production-20261110/sync-collector-5a2f0b0128b8483ebf9e83c8511cf969/.

`git diff --check` passes. Normal review/required OSS CI and the next exact-main
producer remain pending. The adopted-base resolver still must carry the failed
169 four-test-file delta into that normal generated Cloud PR; no hand-pin/copy.
Production acceptance=false/provider_complete=false; no customer/app/mail/key
read or production resource/restart/deployment action.
