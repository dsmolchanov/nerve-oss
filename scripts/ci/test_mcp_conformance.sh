#!/usr/bin/env bash
set -euo pipefail

readonly CONFORMANCE_REVISION="81eb1c3edaed87d7fd585d7b80186da7a2960660"
readonly CONFORMANCE_LOCK_SHA256="161aef794720d2393a6a3db64e9751f2d52730b49f662e84b23363df5c1196e1"
readonly EXT_AUTH_REVISION="ce15435bf4e35a0ec972dd7cd8ce4c81d609cc3e"
readonly EXT_AUTH_LOCK_SHA256="77bc803809af11ce06fe227409589c1f842c0b5494206446f597feb50c1bfa8d"
readonly LOCAL_NPM_VERSION="10.9.2"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
temp_root=""

cleanup() {
  if [[ -n "$temp_root" && -d "$temp_root" ]]; then
    rm -rf "$temp_root"
  fi
}
trap cleanup EXIT

npm_command=(npm)
npm_major="$(npm --version | cut -d. -f1)"
if [[ "$npm_major" -gt 10 ]]; then
  # The pinned upstream lock was produced for npm 10 and npm 11 rejects it.
  # Hosted CI uses Node 20/npm 10; this exact fallback keeps newer local
  # developer installations from silently rewriting the upstream lock.
  npm_command=(npx --yes "npm@$LOCAL_NPM_VERSION")
fi

hash_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

checkout_exact() {
  local repository="$1"
  local revision="$2"
  local destination="$3"
  if [[ ! -d "$destination/.git" ]]; then
    git clone --quiet --filter=blob:none "$repository" "$destination"
  fi
  git -C "$destination" fetch --quiet --depth=1 origin "$revision"
  git -C "$destination" checkout --quiet --detach "$revision"
  [[ "$(git -C "$destination" rev-parse HEAD)" == "$revision" ]]
}

if [[ -n "${MCP_CONFORMANCE_DIR:-}" || -n "${MCP_EXT_AUTH_DIR:-}" ]]; then
  if [[ -z "${MCP_CONFORMANCE_DIR:-}" || -z "${MCP_EXT_AUTH_DIR:-}" ]]; then
    echo "MCP_CONFORMANCE_DIR and MCP_EXT_AUTH_DIR must be set together" >&2
    exit 1
  fi
  conformance_dir="$MCP_CONFORMANCE_DIR"
  ext_auth_dir="$MCP_EXT_AUTH_DIR"
else
  temp_root="$(mktemp -d "${TMPDIR:-/tmp}/nerve-mcp-conformance.XXXXXX")"
  conformance_dir="$temp_root/conformance"
  ext_auth_dir="$temp_root/ext-auth"
  checkout_exact "https://github.com/modelcontextprotocol/conformance.git" "$CONFORMANCE_REVISION" "$conformance_dir"
  checkout_exact "https://github.com/modelcontextprotocol/ext-auth.git" "$EXT_AUTH_REVISION" "$ext_auth_dir"
fi

if [[ "$(git -C "$conformance_dir" rev-parse HEAD)" != "$CONFORMANCE_REVISION" ]]; then
  echo "MCP conformance checkout is not pinned to $CONFORMANCE_REVISION" >&2
  exit 1
fi
if [[ "$(git -C "$ext_auth_dir" rev-parse HEAD)" != "$EXT_AUTH_REVISION" ]]; then
  echo "MCP ext-auth checkout is not pinned to $EXT_AUTH_REVISION" >&2
  exit 1
fi
if [[ "$(hash_file "$conformance_dir/package-lock.json")" != "$CONFORMANCE_LOCK_SHA256" ]]; then
  echo "MCP conformance package-lock digest mismatch" >&2
  exit 1
fi
if [[ "$(hash_file "$ext_auth_dir/package-lock.json")" != "$EXT_AUTH_LOCK_SHA256" ]]; then
  echo "MCP ext-auth package-lock digest mismatch" >&2
  exit 1
fi

grep -Fq 'LATEST_PROTOCOL_VERSION = "2026-07-28"' "$conformance_dir/src/spec-types/draft.ts"
grep -Fq 'OAuth Client Credentials Extension' "$ext_auth_dir/specification/draft/oauth-client-credentials.mdx"
grep -Fq '"private_key_jwt"' "$ext_auth_dir/specification/draft/oauth-client-credentials.mdx"

"${npm_command[@]}" --prefix "$conformance_dir" ci --ignore-scripts
"${npm_command[@]}" --prefix "$conformance_dir" test
"${npm_command[@]}" --prefix "$conformance_dir" run build
"${npm_command[@]}" --prefix "$ext_auth_dir" ci --ignore-scripts
"${npm_command[@]}" --prefix "$ext_auth_dir" run check:docs:format

python3 - "$repo_root/scripts/ci/fixtures" <<'PY'
import json
import pathlib
import sys

fixtures = pathlib.Path(sys.argv[1])

def validate_client_credentials_profile(document):
    if "authorization_endpoint" in document:
        raise ValueError("client-credentials metadata must omit authorization_endpoint")
    if "response_types_supported" in document:
        raise ValueError("client-credentials metadata must omit response_types_supported")
    if document.get("grant_types_supported") != ["client_credentials"]:
        raise ValueError("metadata must advertise only client_credentials")
    if document.get("token_endpoint_auth_methods_supported") != ["private_key_jwt"]:
        raise ValueError("metadata must advertise private_key_jwt")

golden = json.loads((fixtures / "oauth-as-metadata-client-credentials.json").read_text())
validate_client_credentials_profile(golden)
for filename in (
    "oauth-as-metadata-invalid-empty-response-types.json",
    "oauth-as-metadata-invalid-fabricated-response-type.json",
):
    document = json.loads((fixtures / filename).read_text())
    try:
        validate_client_credentials_profile(document)
    except ValueError as error:
        if "must omit response_types_supported" not in str(error):
            raise SystemExit(f"negative fixture {filename} failed for the wrong reason: {error}") from error
    else:
        raise SystemExit(f"negative fixture {filename} was accepted")
PY

echo "pinned MCP conformance and ext-auth proof passed"
