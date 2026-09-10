#!/usr/bin/env bash
# Shared build input for the linker and the runtime manifest. No runtime env.
set -euo pipefail
case "${RUNTIME_MANIFEST_VERSION:-1}" in
  1) printf '\n'; exit 0 ;;
  2) ;;
  *) echo "unsupported runtime manifest version" >&2; exit 2 ;;
esac
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
last="$(find "${CORE_MIGRATIONS_PATH:-$root/internal/store/migrations/core}" -maxdepth 1 -type f -name '*.sql' | LC_ALL=C sort | tail -1)"
name="${last##*/}"
[[ "$name" =~ ^([0-9]{4})_[a-z0-9_]+\.sql$ ]] || { echo "invalid Core source head" >&2; exit 2; }
core_max="$((10#${BASH_REMATCH[1]}))"
case "${ARTIFACT_ROLE:-}" in
 B) core_min="${CORE_SCHEMA_MIN_REQUIRED:?bridge requires prior Core head}" ;;
 C) core_min="$core_max" ;;
 *) echo "successor runtime requires role B or C" >&2; exit 2 ;;
esac
[[ "$core_min" =~ ^[1-9][0-9]{0,3}$ ]] || { echo "invalid Core minimum" >&2; exit 2; }
(( core_min >= 29 && core_min <= core_max )) || { echo "invalid Core window" >&2; exit 2; }
[[ -z "${CORE_SCHEMA_MAX_SUPPORTED:-}" || "$CORE_SCHEMA_MAX_SUPPORTED" == "$core_max" ]] || { echo "Core maximum must match source head" >&2; exit 2; }
printf '2:%s:%s\n' "$core_min" "$core_max"
