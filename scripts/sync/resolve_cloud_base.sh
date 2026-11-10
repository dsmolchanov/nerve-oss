#!/usr/bin/env bash
set -euo pipefail

refuse() { echo 'Cloud OSS source base refused' >&2; exit 1; }
[[ $# -eq 3 ]] || refuse
oss_root="$1"
cloud_root="$2"
head_ref="$3"
lock="$cloud_root/deploy/cloud/oss-source.lock"
[[ -f "$lock" && "$head_ref" =~ ^[0-9a-f]{40}$ ]] || refuse
[[ "$(awk -F= '$1 == "OSS_SOURCE_REVISION" { count++ } END { print count+0 }' "$lock")" -eq 1 ]] || refuse
base_ref="$(awk -F= '$1 == "OSS_SOURCE_REVISION" { if (NF != 2) exit 1; print $2 }' "$lock")"
[[ "$base_ref" =~ ^[0-9a-f]{40}$ ]] || refuse
git -C "$oss_root" cat-file -e "${head_ref}^{commit}" 2>/dev/null || refuse
git -C "$oss_root" cat-file -e "${base_ref}^{commit}" 2>/dev/null || refuse
git -C "$oss_root" merge-base --is-ancestor "$base_ref" "$head_ref" || refuse
printf '%s\n' "$base_ref"
