#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 5 ]]; then
  echo "usage: apply_to_cloud.sh OSS_ROOT CLOUD_ROOT BASE_REF HEAD_REF CHANGED_FILE" >&2
  exit 2
fi

oss_root="$1"
cloud_root="$2"
base_ref="$3"
head_ref="$4"
changed_file="$5"
manifest="$oss_root/sync-manifest.yaml"

if [[ ! -d "$oss_root/.git" && ! -f "$oss_root/.git" ]]; then
  echo "OSS root is not a git worktree: $oss_root" >&2
  exit 2
fi
if [[ ! -d "$cloud_root/.git" && ! -f "$cloud_root/.git" ]]; then
  echo "Cloud root is not a git worktree: $cloud_root" >&2
  exit 2
fi

"$oss_root/scripts/sync/verify_exact_mirror.sh" "$manifest" "$oss_root" "$oss_root" >/dev/null

if [[ "$base_ref" =~ ^0+$ ]] || ! git -C "$oss_root" cat-file -e "${base_ref}^{commit}" 2>/dev/null; then
  base_ref="$(git -C "$oss_root" hash-object -t tree /dev/null)"
fi
git -C "$oss_root" cat-file -e "${head_ref}^{commit}" 2>/dev/null

matches_manifest_path() {
  local candidate="$1"
  local entry="$2"
  entry="${entry%/}"
  [[ "$candidate" == "$entry" || "$candidate" == "$entry/"* ]]
}

is_in_manifest_list() {
  local list_name="$1"
  local candidate="$2"
  local entry
  while IFS= read -r entry; do
    if matches_manifest_path "$candidate" "$entry"; then
      return 0
    fi
  done < <(jq -r --arg list "$list_name" '.[$list][]' "$manifest")
  return 1
}

is_cloud_only() {
  is_in_manifest_list "cloud-only" "$1"
}

changed_paths=()
while IFS= read -r changed_path; do
  changed_paths+=("$changed_path")
done < <(git -C "$oss_root" diff --name-only --no-renames "$base_ref" "$head_ref")

: >"$changed_file"
for changed_path in "${changed_paths[@]}"; do
  if [[ "$changed_path" == "sync-manifest.yaml" ]]; then
    printf '%s\n' "$changed_path" >>"$changed_file"
    continue
  fi
  if is_cloud_only "$changed_path"; then
    continue
  fi
  if is_in_manifest_list "exact-mirror" "$changed_path" ||
     is_in_manifest_list "patch-synced" "$changed_path"; then
    printf '%s\n' "$changed_path" >>"$changed_file"
  fi
done

if [[ ! -s "$changed_file" ]]; then
  echo "no shared paths changed"
  exit 0
fi

sync_exact_path() {
  local manifest_path="$1"
  local relative_path="${manifest_path%/}"
  local source_path="$oss_root/$relative_path"
  local destination_path="$cloud_root/$relative_path"

  case "$relative_path" in
    ""|.|..|/*|../*|*/../*|*/..)
      echo "unsafe exact-mirror path: $relative_path" >&2
      exit 2
      ;;
  esac

  if [[ "$manifest_path" == */ ]]; then
    if [[ -d "$source_path" ]]; then
      mkdir -p "$destination_path"
      rsync -a --delete "$source_path/" "$destination_path/"
    elif [[ -e "$destination_path" ]]; then
      rm -rf -- "$destination_path"
    fi
  elif [[ -f "$source_path" ]]; then
    mkdir -p "$(dirname "$destination_path")"
    cp "$source_path" "$destination_path"
  elif [[ -e "$destination_path" ]]; then
    rm -f -- "$destination_path"
  fi
}

while IFS= read -r exact_path; do
  sync_exact_path "$exact_path"
done < <(jq -r '."exact-mirror"[]' "$manifest")

patch_file="$(mktemp)"
trap 'rm -f "$patch_file"' EXIT
for changed_path in "${changed_paths[@]}"; do
  if is_cloud_only "$changed_path" ||
     ! is_in_manifest_list "patch-synced" "$changed_path"; then
    continue
  fi

  source_path="$oss_root/$changed_path"
  destination_path="$cloud_root/$changed_path"
  if [[ -e "$source_path" && ! -e "$destination_path" ]]; then
    mkdir -p "$(dirname "$destination_path")"
    cp -R "$source_path" "$destination_path"
    continue
  fi
  if [[ ! -e "$source_path" && ! -e "$destination_path" ]]; then
    continue
  fi

  git -C "$oss_root" diff --binary --no-renames "$base_ref" "$head_ref" -- "$changed_path" >"$patch_file"
  if [[ -s "$patch_file" ]]; then
    git -C "$cloud_root" apply --3way "$patch_file"
  fi
done

"$oss_root/scripts/sync/verify_exact_mirror.sh" "$manifest" "$oss_root" "$cloud_root"
echo "shared paths applied"
