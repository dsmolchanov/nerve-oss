#!/usr/bin/env bash
set -euo pipefail

MANIFEST_PATH="${1:?usage: export_runtime_manifest_outputs.sh MANIFEST_PATH EXPECTED_VERSION OUTPUT_PATH}"
EXPECTED_VERSION="${2:?usage: export_runtime_manifest_outputs.sh MANIFEST_PATH EXPECTED_VERSION OUTPUT_PATH}"
OUTPUT_PATH="${3:?usage: export_runtime_manifest_outputs.sh MANIFEST_PATH EXPECTED_VERSION OUTPUT_PATH}"

if ! command -v jq >/dev/null 2>&1; then
  echo "jq is required to export runtime manifest outputs" >&2
  exit 1
fi

if [[ ! -f "$MANIFEST_PATH" ]]; then
  echo "runtime manifest not found: $MANIFEST_PATH" >&2
  exit 1
fi

if ! jq -e -s --arg expected_version "$EXPECTED_VERSION" '
  def output_safe:
    type == "string" and (explode | all(. >= 32 and . != 127));
  length == 1
  and (.[0] |
    type == "object"
    and (.runtime_version | output_safe and . == $expected_version)
    and (.mcp_contract_hash | output_safe and test("^[0-9a-f]{64}$"))
    and (.core_schema_hash | output_safe and test("^[0-9a-f]{64}$"))
    and (.build_time |
      output_safe
      and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$")
      and (. as $timestamp |
        try ((fromdateiso8601 | todateiso8601) == $timestamp) catch false)
    )
  )
' "$MANIFEST_PATH" >/dev/null; then
  echo "runtime manifest contains missing or invalid release metadata: $MANIFEST_PATH" >&2
  exit 1
fi

RUNTIME_VERSION="$(jq -er -s '.[0].runtime_version' "$MANIFEST_PATH")"
MCP_HASH="$(jq -er -s '.[0].mcp_contract_hash' "$MANIFEST_PATH")"
CORE_HASH="$(jq -er -s '.[0].core_schema_hash' "$MANIFEST_PATH")"
BUILD_TIME="$(jq -er -s '.[0].build_time' "$MANIFEST_PATH")"

{
  printf 'runtime_version=%s\n' "$RUNTIME_VERSION"
  printf 'mcp_contract_hash=%s\n' "$MCP_HASH"
  printf 'core_schema_hash=%s\n' "$CORE_HASH"
  printf 'build_time=%s\n' "$BUILD_TIME"
} >> "$OUTPUT_PATH"
