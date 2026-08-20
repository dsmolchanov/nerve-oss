#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
# shellcheck source=mcp_conformance_paths.sh
source "$repo_root/scripts/ci/mcp_conformance_paths.sh"

assert_overrides() {
  local configured="$1"
  local expected="$2"
  MCP_CONFORMANCE_DIR="$configured"
  MCP_EXT_AUTH_DIR="$configured"
  resolve_mcp_conformance_overrides "$repo_root"
  [[ "$conformance_dir" == "$expected" ]]
  [[ "$ext_auth_dir" == "$expected" ]]
  [[ "$conformance_dir" == /* ]]
  [[ "$ext_auth_dir" == /* ]]
}

fixture="$repo_root/scripts/ci"
assert_overrides "scripts/ci" "$fixture"
assert_overrides "$fixture" "$fixture"

echo "MCP conformance override path tests passed"
