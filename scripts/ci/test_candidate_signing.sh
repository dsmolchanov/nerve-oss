#!/usr/bin/env bash
# The candidate producer must emit an image the control plane can accept.
#
# nerve-cloud's verify_runtime_candidate_lock.sh ends in a Sigstore check
# demanding a keyless signature whose certificate identity is this workflow on
# main. A candidate built without one is not merely unverified -- it is
# unusable, because keyless signing binds to the producing workflow run and
# cannot be applied after the fact. The first candidate shipped that way and
# nothing caught it until a consumer finally had credentials to look.
#
# Parsed with grep and awk: this job installs cosign and nothing else, so a test
# that reached for a YAML library would add a dependency the job never declares.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORKFLOW="$ROOT_DIR/.github/workflows/docker-publish.yml"
failures=0
ok()   { echo "  ok   $1"; }
fail() { echo "  FAIL $1" >&2; failures=$((failures + 1)); }

# Everything below concerns the candidate job alone, so read only its lines:
# from "  candidate:" to the next job at the same indentation.
candidate="$(awk '
  /^  [a-z][a-z0-9_-]*:$/ { inside = ($0 == "  candidate:") }
  inside { print }
' "$WORKFLOW")"

[[ -n "$candidate" ]] || { echo "candidate job not found in $WORKFLOW" >&2; exit 1; }

if grep -q 'id-token: write' <<<"$candidate"; then
  ok "candidate job requests id-token (keyless signing is possible)"
else
  fail "candidate job cannot sign: no id-token: write"
fi

# The candidate creates nothing publishable, so it must not inherit the
# workflow-level contents: write.
if grep -q 'contents: read' <<<"$candidate"; then
  ok "candidate job drops contents: write"
else
  fail "candidate job does not narrow contents to read"
fi

if grep -q 'sigstore/cosign-installer@' <<<"$candidate"; then
  ok "candidate job installs cosign"
else
  fail "candidate job never installs cosign"
fi

if grep -qE 'cosign sign --yes "\$\{reference\}"' <<<"$candidate"; then
  ok "candidate job signs the resolved index digest"
else
  fail "candidate job does not sign the image"
fi

# Signing is not enough: it must be signed as the identity the consumer checks.
# nerve-cloud builds "https://github.com/OWNER/REPO/.github/workflows/WORKFLOW@refs/heads/main".
if grep -q '/.github/workflows/docker-publish.yml@refs/heads/main' <<<"$candidate"; then
  ok "signing identity matches the one the control plane demands"
else
  fail "signing identity does not match the consumer's expected identity"
fi

if grep -q 'certificate-oidc-issuer https://token.actions.githubusercontent.com' <<<"$candidate"; then
  ok "candidate job verifies its own signature against the Actions issuer"
else
  fail "candidate job never proves the signature it just made is usable"
fi

# A signature step that cannot fail the build is decoration.
if grep -nE '(\|\||&&[[:space:]]*true|; *true).*cosign|cosign[^|]*\|\| *true' <<<"$candidate" >/dev/null; then
  fail "a cosign invocation is guarded and cannot fail the build"
else
  ok "cosign invocations are unguarded"
fi

if (( failures > 0 )); then
  echo "$failures candidate signing case(s) failed" >&2
  exit 1
fi
echo "candidate signing contract holds"
