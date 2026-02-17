#!/usr/bin/env bash
set -euo pipefail

RUNTIME_VERSION="${1:-${RUNTIME_VERSION:-dev}}"
OUT_PATH="${2:-${RUNTIME_MANIFEST_OUT:-runtime-manifest.json}}"

GIT_COMMIT="${GIT_COMMIT:-$(git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)}"
BUILD_TIME="${BUILD_TIME:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
MCP_CONTRACT_PATH="${MCP_CONTRACT_PATH:-docs/MCP_Contract.md}"
CORE_MIGRATIONS_PATH="${CORE_MIGRATIONS_PATH:-internal/store/migrations/core}"

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

MCP_CONTRACT_HASH="$(hash_file "$MCP_CONTRACT_PATH")"
CORE_SCHEMA_HASH="$(hash_core_schema_dir "$CORE_MIGRATIONS_PATH")"

cat > "$OUT_PATH" <<JSON
{
  "runtime_version": "$RUNTIME_VERSION",
  "mcp_contract_hash": "$MCP_CONTRACT_HASH",
  "core_schema_hash": "$CORE_SCHEMA_HASH",
  "build_commit": "$GIT_COMMIT",
  "build_time": "$BUILD_TIME"
}
JSON

echo "wrote runtime manifest: $OUT_PATH"
