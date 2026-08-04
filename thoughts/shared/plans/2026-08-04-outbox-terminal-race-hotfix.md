# Outbox terminal-race hotfix

## Scope

Close the two shared-code blockers found while reviewing the OSS-to-Cloud
baseline sync:

1. When `ON CONFLICT DO NOTHING` loses to an active content-hash row and that
   winner becomes `sent` or `failed` before the fresh-snapshot lookup, retry the
   insert so a legitimate resend creates a new row instead of returning
   `sql.ErrNoRows`.
2. Keep the webhook integration test's admin database connection open until
   its database-drop cleanup has completed.

## Implementation

- Keep idempotency-key and active-content winner lookup unchanged.
- If no winner remains visible, retry the insert with a fresh statement
  snapshot. Bound repeated unresolved conflicts and return an explicit error.
- Add a deterministic regression seam/test that moves the conflicting winner
  to `sent` between the conflict and lookup.
- Register `adminDB.Close` with `t.Cleanup` before the database-drop cleanup so
  LIFO cleanup order drops the temporary database first.

## Verification

- `go test ./internal/store ./internal/webhooks -count=1`
- Run both content-dedup concurrency regressions repeatedly.
- `go build ./... && go vet ./...`
- After OSS merge, exact-mirror the three changed shared files into Cloud and
  run both repositories' DB-backed suites.
