#!/usr/bin/env bash

resolve_mcp_conformance_checkout() {
  local repo_root="$1"
  local configured="$2"
  local candidate="$configured"
  if [[ "$candidate" != /* ]]; then
    candidate="$repo_root/$candidate"
  fi
  (cd "$candidate" && pwd -P)
}

resolve_mcp_conformance_overrides() {
  local repo_root="$1"
  if [[ -z "${MCP_CONFORMANCE_DIR:-}" || -z "${MCP_EXT_AUTH_DIR:-}" ]]; then
    echo "MCP_CONFORMANCE_DIR and MCP_EXT_AUTH_DIR must be set together" >&2
    return 1
  fi
  conformance_dir="$(resolve_mcp_conformance_checkout "$repo_root" "$MCP_CONFORMANCE_DIR")"
  ext_auth_dir="$(resolve_mcp_conformance_checkout "$repo_root" "$MCP_EXT_AUTH_DIR")"
}
