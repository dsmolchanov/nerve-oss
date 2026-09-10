#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="$root/scripts/release/select_runtime_candidate_contract.sh"
export GITHUB_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa GITHUB_RUN_ID=17
export CORE_SCHEMA_MAX_SUPPORTED='' CORE_MIGRATIONS_PATH="$root/internal/store/migrations/core"
select_contract() {
  RUNTIME_MANIFEST_VERSION="$1" ARTIFACT_ROLE="$2" CORE_SCHEMA_MIN_REQUIRED="$3" bash "$script"
}
legacy="$(select_contract 1 '' '')"
[[ "$legacy" == *"CORE_SCHEMA_MIN_REQUIRED=29"* && "$legacy" == *"CORE_SCHEMA_MAX_SUPPORTED=29"* ]]
[[ "$legacy" == *"CANDIDATE_ARTIFACT_NAME=mcp2026-runtime-candidate-${GITHUB_SHA}"* ]]
for role in B C; do
  prior=''; [[ "$role" != B ]] || prior=29
  output="$(select_contract 2 "$role" "$prior")"
  [[ "$output" == *"RUNTIME_MANIFEST_VERSION=2"* && "$output" == *"ARTIFACT_ROLE=$role"* ]]
  [[ "$output" == *"CANDIDATE_TAG_SUFFIX=v2-${role}-${GITHUB_SHA}-${GITHUB_RUN_ID}"* ]]
  [[ "$output" == *"CANDIDATE_ARTIFACT_NAME=mcp2026-runtime-successor-${role}-${GITHUB_SHA}-${GITHUB_RUN_ID}"* ]]
done
for values in '3::' '1:B:' '1::29' '2::' '2:A:29' '2:B:' '2:B:28' '2:B:029' '2:B:9999' '2:C:29'; do
  IFS=: read -r version role prior <<< "$values"
  if select_contract "$version" "$role" "$prior" >/dev/null 2>&1; then
    echo 'invalid candidate contract accepted' >&2; exit 1
  fi
done
if GITHUB_SHA=main select_contract 2 B 29 >/dev/null 2>&1; then exit 1; fi
if GITHUB_RUN_ID=0 select_contract 2 B 29 >/dev/null 2>&1; then exit 1; fi
echo 'historical and successor candidate dispatch contracts passed'

# The candidate Dockerfile and COPY bytes must come from the asserted Git tree.
python3 - "$root/.github/workflows/docker-publish.yml" <<'PYTEST'
from pathlib import Path
import sys
candidate = Path(sys.argv[1]).read_text().split('  candidate:', 1)[1]
assert '          context: https://github.com/${{ github.repository }}.git#${{ github.sha }}\n' in candidate
assert '          provenance: mode=min,version=v0.2\n' in candidate
assert 'build-contexts:' not in candidate
assert '          file: deploy/docker/cortex/Dockerfile\n' in candidate
PYTEST
