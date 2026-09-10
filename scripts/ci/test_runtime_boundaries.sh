#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
fixture="$(mktemp -d)"
trap 'rm -rf "$fixture"' EXIT
mkdir -p "$fixture/scripts/ci" "$fixture/cmd/neuralmaild" \
  "$fixture/internal/app" "$fixture/internal/mcp" "$fixture/internal/tools" \
  "$fixture/deploy/docker/cortex"
cp "$root/scripts/ci/verify_runtime_boundaries.sh" "$fixture/scripts/ci/"
printf 'module neuralmail\n\ngo 1.25.0\n' > "$fixture/go.mod"
printf 'FROM scratch\n' > "$fixture/deploy/docker/cortex/Dockerfile"
for package in app mcp tools; do
  printf 'package %s\n' "$package" > "$fixture/internal/$package/package.go"
done
cat > "$fixture/cmd/neuralmaild/main.go" <<'GO'
package main
import (
    _ "neuralmail/internal/app"
    _ "neuralmail/internal/mcp"
    _ "neuralmail/internal/tools"
)
func main() {}
GO
"$fixture/scripts/ci/verify_runtime_boundaries.sh"
mkdir -p "$fixture/internal/orphan"
printf 'package orphan\n' > "$fixture/internal/orphan/orphan.go"
if "$fixture/scripts/ci/verify_runtime_boundaries.sh" > "$fixture/result" 2>&1; then
  echo 'unreachable package incorrectly accepted' >&2
  exit 1
fi
grep -Fq 'neuralmail/internal/orphan' "$fixture/result"
# An independent command legitimately makes that package reachable.
mkdir -p "$fixture/cmd/helper"
printf 'package main\nimport _ "neuralmail/internal/orphan"\nfunc main() {}\n' > "$fixture/cmd/helper/main.go"
"$fixture/scripts/ci/verify_runtime_boundaries.sh"
printf 'package orphan\nimport _ "neuralmail/internal/missing"\n' > "$fixture/internal/orphan/orphan.go"
if "$fixture/scripts/ci/verify_runtime_boundaries.sh" > "$fixture/result" 2>&1; then
  echo 'go-list failure incorrectly accepted' >&2
  exit 1
fi
echo 'runtime reachability regression passed'
