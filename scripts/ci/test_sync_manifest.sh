#!/usr/bin/env bash
set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
fixture_root="$(mktemp -d)"
trap 'rm -rf "$fixture_root"' EXIT

oss_fixture="$fixture_root/oss"
cloud_fixture="$fixture_root/cloud"
mkdir -p "$oss_fixture"
git -C "$oss_fixture" init -q
git -C "$oss_fixture" config user.email sync-test@example.com
git -C "$oss_fixture" config user.name sync-test

mkdir -p \
  "$oss_fixture/docs" \
  "$oss_fixture/internal/cloudapi" \
  "$oss_fixture/internal/emailtransport" \
  "$oss_fixture/scripts/sync"
cp "$repository_root/sync-manifest.yaml" "$oss_fixture/sync-manifest.yaml"
cp "$repository_root/scripts/sync/verify_exact_mirror.sh" "$oss_fixture/scripts/sync/verify_exact_mirror.sh"
printf 'contract base\n' >"$oss_fixture/docs/MCP_Contract.md"
printf 'package cloudapi\n\nconst shared = "base"\n' >"$oss_fixture/internal/cloudapi/shared.go"
printf 'package cloudapi\n\nconst cloudOnly = "oss base"\n' >"$oss_fixture/internal/cloudapi/handler_messages.go"
printf 'package emailtransport\n' >"$oss_fixture/internal/emailtransport/stale.go"
git -C "$oss_fixture" add .
git -C "$oss_fixture" commit -qm base
base_ref="$(git -C "$oss_fixture" rev-parse HEAD)"

git clone -q "$oss_fixture" "$cloud_fixture"
git -C "$cloud_fixture" config user.email sync-test@example.com
git -C "$cloud_fixture" config user.name sync-test
printf 'package cloudapi\n\nconst cloudOnly = "cloud preserved"\n' >"$cloud_fixture/internal/cloudapi/handler_messages.go"
printf 'package emailtransport\n' >"$cloud_fixture/internal/emailtransport/cloud_extra.go"
git -C "$cloud_fixture" add .
git -C "$cloud_fixture" commit -qm cloud-divergence

printf 'contract head\n' >"$oss_fixture/docs/MCP_Contract.md"
printf 'package cloudapi\n\nconst shared = "oss head"\n' >"$oss_fixture/internal/cloudapi/shared.go"
printf 'package cloudapi\n' >"$oss_fixture/internal/cloudapi/bootstrap.go"
printf 'package cloudapi\n\nconst cloudOnly = "oss head"\n' >"$oss_fixture/internal/cloudapi/handler_messages.go"
rm "$oss_fixture/internal/emailtransport/stale.go"
printf 'package emailtransport\n' >"$oss_fixture/internal/emailtransport/current.go"
git -C "$oss_fixture" add -A
git -C "$oss_fixture" commit -qm head
head_ref="$(git -C "$oss_fixture" rev-parse HEAD)"

changed_file="$fixture_root/changed.txt"
"$repository_root/scripts/sync/apply_to_cloud.sh" \
  "$oss_fixture" "$cloud_fixture" "$base_ref" "$head_ref" "$changed_file"

cmp "$oss_fixture/docs/MCP_Contract.md" "$cloud_fixture/docs/MCP_Contract.md"
cmp "$oss_fixture/internal/cloudapi/shared.go" "$cloud_fixture/internal/cloudapi/shared.go"
cmp "$oss_fixture/internal/cloudapi/bootstrap.go" "$cloud_fixture/internal/cloudapi/bootstrap.go"
test ! -e "$cloud_fixture/internal/emailtransport/stale.go"
test ! -e "$cloud_fixture/internal/emailtransport/cloud_extra.go"
grep -Fq 'cloud preserved' "$cloud_fixture/internal/cloudapi/handler_messages.go"
grep -Fq 'internal/cloudapi/shared.go' "$changed_file"
if grep -Fq 'internal/cloudapi/handler_messages.go' "$changed_file"; then
  echo "cloud-only path was reported as shared" >&2
  exit 1
fi

conflict_fixture="$fixture_root/cloud-conflict"
git clone -q "$oss_fixture" "$conflict_fixture"
git -C "$conflict_fixture" checkout -q "$base_ref"
git -C "$conflict_fixture" config user.email sync-test@example.com
git -C "$conflict_fixture" config user.name sync-test
printf 'package cloudapi\n\nconst shared = "cloud conflict"\n' >"$conflict_fixture/internal/cloudapi/shared.go"
git -C "$conflict_fixture" add .
git -C "$conflict_fixture" commit -qm conflict
if "$repository_root/scripts/sync/apply_to_cloud.sh" \
  "$oss_fixture" "$conflict_fixture" "$base_ref" "$head_ref" \
  "$fixture_root/conflict-changed.txt"; then
  echo "conflicting patch unexpectedly applied" >&2
  exit 1
fi

echo "sync manifest tests passed"
