#!/usr/bin/env bash
set -euo pipefail

image="${1:?usage: verify_runtime_policy_artifact.sh IMAGE MANIFEST_PATH}"
manifest="${2:?usage: verify_runtime_policy_artifact.sh IMAGE MANIFEST_PATH}"

expected_version="$(jq -er '.outbound_policy_version' "$manifest")"
expected_sha256="$(jq -er '.outbound_policy_sha256' "$manifest")"
labels="$(docker image inspect "$image" --format '{{json .Config.Labels}}')"

if [[ "$(jq -er '."io.nerve.runtime.outbound-policy-version"' <<<"$labels")" != "$expected_version" ]]; then
  echo "runtime image outbound policy version label mismatch" >&2
  exit 1
fi
if [[ "$(jq -er '."io.nerve.runtime.outbound-policy-sha256"' <<<"$labels")" != "$expected_sha256" ]]; then
  echo "runtime image outbound policy digest label mismatch" >&2
  exit 1
fi

embedded_sha256="$(docker run --rm --entrypoint sha256sum "$image" /app/configs/policy/autonomous-outbound-v1.yaml | awk '{print $1}')"
if [[ "$embedded_sha256" != "$expected_sha256" ]]; then
  echo "runtime image embedded outbound policy digest mismatch" >&2
  exit 1
fi

echo "runtime image outbound policy artifact verified"
