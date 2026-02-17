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
    if rg -n -S "$pattern" "$dir" >/dev/null; then
      echo "migration ownership violation: $label contains forbidden token '$pattern'" >&2
      rg -n -S "$pattern" "$dir" >&2
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

echo "migration ownership checks passed"
