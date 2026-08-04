#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 3 ]]; then
  echo "usage: verify_exact_mirror.sh MANIFEST OSS_ROOT CLOUD_ROOT" >&2
  exit 2
fi

manifest="$1"
oss_root="$2"
cloud_root="$3"

jq -e '
  .version == 1 and
  ([."exact-mirror", ."patch-synced", ."cloud-only"] | all(type == "array")) and
  ([."exact-mirror"[], ."patch-synced"[], ."cloud-only"[]] |
    all(type == "string" and length > 0 and
        (startswith("/") | not) and
        (contains("..") | not)))
' "$manifest" >/dev/null

failed=0
while IFS= read -r manifest_path; do
  relative_path="${manifest_path%/}"
  oss_path="$oss_root/$relative_path"
  cloud_path="$cloud_root/$relative_path"

  if [[ ! -e "$oss_path" && ! -e "$cloud_path" ]]; then
    continue
  fi
  if [[ -d "$oss_path" && -d "$cloud_path" ]]; then
    if ! diff -qr "$oss_path" "$cloud_path"; then
      failed=1
    fi
    continue
  fi
  if [[ -f "$oss_path" && -f "$cloud_path" ]]; then
    if ! cmp -s "$oss_path" "$cloud_path"; then
      echo "exact mirror differs: $relative_path" >&2
      failed=1
    fi
    continue
  fi

  echo "exact mirror missing or has a type mismatch: $relative_path" >&2
  failed=1
done < <(jq -r '."exact-mirror"[]' "$manifest")

if [[ "$failed" -ne 0 ]]; then
  exit 1
fi

echo "exact mirror paths match"
