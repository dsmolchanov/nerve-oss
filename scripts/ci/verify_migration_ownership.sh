#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CORE_DIR="$ROOT_DIR/internal/store/migrations/core"
CLOUD_DIR="$ROOT_DIR/internal/store/migrations/cloud"

check_absent() {
  local dir="$1"
  local label="$2"
  shift 2

  local pattern
  for pattern in "$@"; do
    if command -v rg >/dev/null 2>&1; then
      matcher='rg -n -S'
    else
      matcher='grep -R -n -E'
    fi
    if $matcher "$pattern" "$dir" >/dev/null; then
      echo "migration ownership violation: $label contains forbidden token '$pattern'" >&2
      $matcher "$pattern" "$dir" >&2
      exit 1
    fi
  done
}

check_absent "$CORE_DIR" "core migrations" \
  "plan_entitlements" \
  "subscriptions" \
  "webhook_events"

check_absent "$CLOUD_DIR" "cloud migrations" \
  "org_entitlements" \
  "org_usage_counters" \
  "usage_events" \
  "cloud_api_keys" \
  "tool_idempotency" \
  "outbox_messages" \
  "outbox_attempts" \
  "org_domains"

# Embed both scopes explicitly: a broad migrations/** glob can silently ship
# unrelated files, and dropping a scope breaks the migration CLI.
grep -Fq '//go:embed migrations/core/*.sql migrations/cloud/*.sql' "$ROOT_DIR/internal/store/migrate.go"
cd "$ROOT_DIR"
go test ./internal/store -run '^TestEmbeddedMigrationInventory$' -count=1

echo "migration ownership checks passed"
