#!/usr/bin/env bash
set -euo pipefail

RUNTIME_VERSION="${1:-${RUNTIME_VERSION:-dev}}"
OUT_PATH="${2:-${RUNTIME_MANIFEST_OUT:-runtime-manifest.json}}"

GIT_COMMIT="${GIT_COMMIT:-$(git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)}"
BUILD_TIME="${BUILD_TIME:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
MCP_CONTRACT_PATH="${MCP_CONTRACT_PATH:-docs/MCP_Contract.md}"
CORE_MIGRATIONS_PATH="${CORE_MIGRATIONS_PATH:-internal/store/migrations/core}"
OUTBOUND_POLICY_PATH="${OUTBOUND_POLICY_PATH:-configs/policy/autonomous-outbound-v1.yaml}"
CORE_SCHEMA_MIN_REQUIRED="29"
CORE_SCHEMA_MAX_SUPPORTED="29"

hash_file() {
  local path="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$path" | awk '{print $1}'
  else
    shasum -a 256 "$path" | awk '{print $1}'
  fi
}

hash_core_schema_dir() {
  local dir="$1"
  local tmp
  tmp="$(mktemp)"
  while IFS= read -r file; do
    cat "$file" >> "$tmp"
  done < <(find "$dir" -type f -name '*.sql' | LC_ALL=C sort)
  local digest
  digest="$(hash_file "$tmp")"
  rm -f "$tmp"
  printf '%s' "$digest"
}

if [[ ! -f "$MCP_CONTRACT_PATH" ]]; then
  echo "missing MCP contract file: $MCP_CONTRACT_PATH" >&2
  exit 1
fi
if [[ ! -d "$CORE_MIGRATIONS_PATH" ]]; then
  echo "missing core migrations dir: $CORE_MIGRATIONS_PATH" >&2
  exit 1
fi
if [[ ! -f "$OUTBOUND_POLICY_PATH" ]]; then
  echo "missing autonomous outbound policy: $OUTBOUND_POLICY_PATH" >&2
  exit 1
fi

MCP_CONTRACT_HASH="$(hash_file "$MCP_CONTRACT_PATH")"
CORE_SCHEMA_HASH="$(hash_core_schema_dir "$CORE_MIGRATIONS_PATH")"
OUTBOUND_POLICY_HASH="$(hash_file "$OUTBOUND_POLICY_PATH")"
OUTBOUND_POLICY_VERSION="$(awk '/^version: / { print $2; count++ } END { if (count != 1) exit 1 }' "$OUTBOUND_POLICY_PATH")"
if [[ ! "$OUTBOUND_POLICY_VERSION" =~ ^[a-z0-9][a-z0-9._-]*$ ]]; then
  echo "invalid autonomous outbound policy version: $OUTBOUND_POLICY_VERSION" >&2
  exit 1
fi

cat > "$OUT_PATH" <<JSON
{
  "runtime_version": "$RUNTIME_VERSION",
  "mcp_contract_hash": "$MCP_CONTRACT_HASH",
  "core_schema_hash": "$CORE_SCHEMA_HASH",
  "core_schema_min_required": "$CORE_SCHEMA_MIN_REQUIRED",
  "core_schema_max_supported": "$CORE_SCHEMA_MAX_SUPPORTED",
  "outbound_policy_version": "$OUTBOUND_POLICY_VERSION",
  "outbound_policy_sha256": "$OUTBOUND_POLICY_HASH",
  "build_commit": "$GIT_COMMIT",
  "build_time": "$BUILD_TIME"
}
JSON

echo "wrote runtime manifest: $OUT_PATH"
