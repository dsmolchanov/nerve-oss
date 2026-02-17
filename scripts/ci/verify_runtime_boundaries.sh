#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT_DIR"

DEPS="$(go list -deps ./cmd/neuralmaild ./internal/app ./internal/mcp ./internal/tools)"
if echo "$DEPS" | grep -q '^neuralmail/internal/billing$'; then
  echo "runtime boundary violation: runtime imports internal/billing" >&2
  exit 1
fi

DOCKERFILE="deploy/docker/cortex/Dockerfile"
if grep -q 'cmd/nerve-control-plane' "$DOCKERFILE"; then
  echo "runtime docker boundary violation: control-plane build found in $DOCKERFILE" >&2
  exit 1
fi
if grep -q 'cmd/nerve-reconcile' "$DOCKERFILE"; then
  echo "runtime docker boundary violation: reconcile build found in $DOCKERFILE" >&2
  exit 1
fi
if grep -q '/app/nerve-control-plane' "$DOCKERFILE"; then
  echo "runtime docker boundary violation: control-plane binary copy found in $DOCKERFILE" >&2
  exit 1
fi
if grep -q '/app/nerve-reconcile' "$DOCKERFILE"; then
  echo "runtime docker boundary violation: reconcile binary copy found in $DOCKERFILE" >&2
  exit 1
fi
if grep -qi 'dashboard' "$DOCKERFILE"; then
  echo "runtime docker boundary violation: dashboard artifact reference found in $DOCKERFILE" >&2
  exit 1
fi

echo "runtime boundary checks passed"
