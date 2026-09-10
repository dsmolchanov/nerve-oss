#!/usr/bin/env bash
# Validated dispatch inputs for the existing protected runtime producer.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
version="${RUNTIME_MANIFEST_VERSION:-1}"
role="${ARTIFACT_ROLE:-}"
prior="${CORE_SCHEMA_MIN_REQUIRED:-}"
[[ "${GITHUB_SHA:-}" =~ ^[0-9a-f]{40}$ && "${GITHUB_RUN_ID:-}" =~ ^[1-9][0-9]*$ ]] || { echo "candidate needs exact source/run" >&2; exit 2; }
case "$version" in
  1)
    [[ -z "$role" && -z "$prior" ]] || { echo "historical candidate rejects successor inputs" >&2; exit 2; }
    min=29; max=29
    suffix="${GITHUB_SHA}"
    artifact="mcp2026-runtime-candidate-${GITHUB_SHA}"
    ;;
  2)
    [[ "$role" == B || "$role" == C ]] || { echo "successor candidate requires B/C" >&2; exit 2; }
    [[ "$role" == B || -z "$prior" ]] || { echo "target minimum derives from source" >&2; exit 2; }
    window="$(bash "$root/scripts/release/runtime_schema_window.sh")"
    IFS=: read -r format min max <<< "$window"
    suffix="v2-${role}-${GITHUB_SHA}-${GITHUB_RUN_ID}"
    artifact="mcp2026-runtime-successor-${role}-${GITHUB_SHA}-${GITHUB_RUN_ID}"
    ;;
  *) echo "unsupported candidate manifest version" >&2; exit 2 ;;
esac
printf 'RUNTIME_MANIFEST_VERSION=%s\nARTIFACT_ROLE=%s\nCORE_SCHEMA_MIN_REQUIRED=%s\nCORE_SCHEMA_MAX_SUPPORTED=%s\nCANDIDATE_TAG_SUFFIX=%s\nCANDIDATE_ARTIFACT_NAME=%s\n' "$version" "$role" "$min" "$max" "$suffix" "$artifact"
