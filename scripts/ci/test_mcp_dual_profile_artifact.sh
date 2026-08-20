#!/usr/bin/env bash
set -euo pipefail

readonly SDK_VERSION="0.2.0"
readonly SDK_SHA256="9f0a7d6316bf47eef64236f96d1a7a151b5517641930422b1b16711da8b02540"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
proof_root="$(mktemp -d "${TMPDIR:-/tmp}/nerve-mcp-dual-profile.XXXXXX")"
trap 'rm -rf "$proof_root"' EXIT

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

python3 -m venv "$proof_root/venv"
python="$proof_root/venv/bin/python"
pip="$proof_root/venv/bin/pip"
mkdir -p "$proof_root/wheel"
"$pip" download --disable-pip-version-check --no-deps --only-binary=:all: \
  --dest "$proof_root/wheel" "nerve-email==${SDK_VERSION}"
wheel_path="$(find "$proof_root/wheel" -maxdepth 1 -type f \
  -name 'nerve_email-0.2.0-*.whl' -print -quit)"
test -n "$wheel_path"
test "$(sha256_file "$wheel_path")" = "$SDK_SHA256"
"$pip" install --disable-pip-version-check "$wheel_path"
test "$("$python" -c 'import nerve_email; print(nerve_email.__version__)')" = "$SDK_VERSION"

cd "$repo_root"
NERVE_IMMUTABLE_SDK_0_2_PYTHON="$python" \
  go test -tags=mcp2026artifact ./internal/mcp \
    -run '^TestImmutableSDK02AndNativeMCP2026ShareEndpoint$' -count=1 -v

echo "immutable SDK 0.2 and native MCP 2026 shared-endpoint proof passed"
