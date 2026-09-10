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
  "$oss_fixture/internal/syncpatch" \
  "$oss_fixture/internal/emailtransport" \
  "$oss_fixture/scripts/sync"
# A synthetic patch-owned package retains coverage of three-way conflicts
# after real cloudapi becomes entirely cloud-owned.
jq '."patch-synced" += ["internal/syncpatch/"]' \
  "$repository_root/sync-manifest.yaml" > "$fixture_root/new-manifest.json"
# Simulate the previous ownership at the base of the cleanup commit.
jq '."exact-mirror" += ["internal/webhooks/", "internal/httpsafe/"] |
    ."patch-synced" += ["internal/cloudapi/"] |
    ."cloud-only" -= ["internal/webhooks/", "internal/httpsafe/", "internal/cloudapi/"]' \
  "$fixture_root/new-manifest.json" > "$oss_fixture/sync-manifest.yaml"
cp "$repository_root/scripts/sync/verify_exact_mirror.sh" "$oss_fixture/scripts/sync/verify_exact_mirror.sh"
printf 'contract base\n' >"$oss_fixture/docs/MCP_Contract.md"
printf 'package cloudapi\n\nconst shared = "base"\n' >"$oss_fixture/internal/syncpatch/shared.go"
printf 'package cloudapi\n\nconst cloudOnly = "oss base"\n' >"$oss_fixture/internal/cloudapi/handler_messages.go"
printf 'package emailtransport\n' >"$oss_fixture/internal/emailtransport/stale.go"
for path in internal/cloudapi/owned.go internal/billing/owned.go internal/reconcile/owned.go \
  internal/webhooks/owned.go internal/httpsafe/owned.go \
  internal/store/store_billing.go internal/store/store_tokens.go internal/store/store_usage.go; do
  mkdir -p "$(dirname "$oss_fixture/$path")"
  printf 'upstream base\n' > "$oss_fixture/$path"
done
git -C "$oss_fixture" add .
git -C "$oss_fixture" commit -qm base
base_ref="$(git -C "$oss_fixture" rev-parse HEAD)"

git clone -q "$oss_fixture" "$cloud_fixture"
git -C "$cloud_fixture" config user.email sync-test@example.com
git -C "$cloud_fixture" config user.name sync-test
printf 'package cloudapi\n\nconst cloudOnly = "cloud preserved"\n' >"$cloud_fixture/internal/cloudapi/handler_messages.go"
printf 'package emailtransport\n' >"$cloud_fixture/internal/emailtransport/cloud_extra.go"
for path in internal/cloudapi/owned.go internal/billing/owned.go internal/reconcile/owned.go \
  internal/webhooks/owned.go internal/httpsafe/owned.go \
  internal/store/store_billing.go internal/store/store_tokens.go internal/store/store_usage.go; do
  printf 'cloud preserved\n' > "$cloud_fixture/$path"
done
git -C "$cloud_fixture" add .
git -C "$cloud_fixture" commit -qm cloud-divergence

printf 'contract head\n' >"$oss_fixture/docs/MCP_Contract.md"
printf 'package cloudapi\n\nconst shared = "oss head"\n' >"$oss_fixture/internal/syncpatch/shared.go"
printf 'package cloudapi\n' >"$oss_fixture/internal/syncpatch/bootstrap.go"
printf 'package cloudapi\n\nconst cloudOnly = "oss head"\n' >"$oss_fixture/internal/cloudapi/handler_messages.go"
rm "$oss_fixture/internal/emailtransport/stale.go"
printf 'package emailtransport\n' >"$oss_fixture/internal/emailtransport/current.go"
cp "$fixture_root/new-manifest.json" "$oss_fixture/sync-manifest.yaml"
rm -rf "$oss_fixture/internal/cloudapi" "$oss_fixture/internal/billing" \
  "$oss_fixture/internal/reconcile" "$oss_fixture/internal/webhooks" "$oss_fixture/internal/httpsafe"
mkdir -p "$oss_fixture/internal/cloudapi"
printf 'upstream addition must not be copied\n' > "$oss_fixture/internal/cloudapi/new.go"
# Store files remain in OSS but may differ without overwriting Cloud.
for name in billing tokens usage; do
  printf 'runtime subset\n' > "$oss_fixture/internal/store/store_$name.go"
done
git -C "$oss_fixture" add -A
git -C "$oss_fixture" commit -qm head
head_ref="$(git -C "$oss_fixture" rev-parse HEAD)"

changed_file="$fixture_root/changed.txt"
"$repository_root/scripts/sync/apply_to_cloud.sh" \
  "$oss_fixture" "$cloud_fixture" "$base_ref" "$head_ref" "$changed_file"

test ! -e "$cloud_fixture/internal/cloudapi/new.go"
cmp "$oss_fixture/docs/MCP_Contract.md" "$cloud_fixture/docs/MCP_Contract.md"
cmp "$oss_fixture/internal/syncpatch/shared.go" "$cloud_fixture/internal/syncpatch/shared.go"
cmp "$oss_fixture/internal/syncpatch/bootstrap.go" "$cloud_fixture/internal/syncpatch/bootstrap.go"
test ! -e "$cloud_fixture/internal/emailtransport/stale.go"
test ! -e "$cloud_fixture/internal/emailtransport/cloud_extra.go"
grep -Fq 'cloud preserved' "$cloud_fixture/internal/cloudapi/handler_messages.go"
for path in internal/cloudapi/owned.go internal/billing/owned.go internal/reconcile/owned.go \
  internal/webhooks/owned.go internal/httpsafe/owned.go \
  internal/store/store_billing.go internal/store/store_tokens.go internal/store/store_usage.go; do
  grep -Fxq 'cloud preserved' "$cloud_fixture/$path"
  if grep -Fxq "$path" "$changed_file"; then
    echo "cloud-owned path entered changed shared files: $path" >&2
    exit 1
  fi
done
grep -Fq 'internal/syncpatch/shared.go' "$changed_file"
if grep -Fq 'internal/cloudapi/handler_messages.go' "$changed_file"; then
  echo "cloud-only path was reported as shared" >&2
  exit 1
fi

conflict_fixture="$fixture_root/cloud-conflict"
git clone -q "$oss_fixture" "$conflict_fixture"
git -C "$conflict_fixture" checkout -q "$base_ref"
git -C "$conflict_fixture" config user.email sync-test@example.com
git -C "$conflict_fixture" config user.name sync-test
printf 'package cloudapi\n\nconst shared = "cloud conflict"\n' >"$conflict_fixture/internal/syncpatch/shared.go"
git -C "$conflict_fixture" add .
git -C "$conflict_fixture" commit -qm conflict
if "$repository_root/scripts/sync/apply_to_cloud.sh" \
  "$oss_fixture" "$conflict_fixture" "$base_ref" "$head_ref" \
  "$fixture_root/conflict-changed.txt"; then
  echo "conflicting patch unexpectedly applied" >&2
  exit 1
fi

# Refuse overlapping exact/cloud ownership BEFORE any files are changed.
for exact in internal/cloudapi/ internal/ internal/cloudapi/owned.go; do
  jq --arg exact "$exact" '."exact-mirror" += [$exact]' \
    "$fixture_root/new-manifest.json" > "$oss_fixture/sync-manifest.yaml"
  before="$(git -C "$cloud_fixture" diff --binary | git hash-object --stdin)"
  if "$repository_root/scripts/sync/apply_to_cloud.sh" \
    "$oss_fixture" "$cloud_fixture" "$base_ref" "$head_ref" "$changed_file"; then
    echo "overlapping cloud ownership unexpectedly accepted: $exact" >&2
    exit 1
  fi
  after="$(git -C "$cloud_fixture" diff --binary | git hash-object --stdin)"
  test "$before" = "$after"
done

echo "sync manifest tests passed"
