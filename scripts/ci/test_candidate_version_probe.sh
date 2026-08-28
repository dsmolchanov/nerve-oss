#!/usr/bin/env bash
# Contract test for prove_candidate_version_unused.sh.
#
# This classifier decides whether a frozen release identity is free. The class
# has already regressed several ways -- a generic nonzero exit read as absence,
# an ambiguous OCI message read as absence, and a post-build phase that skipped
# two of the four probes -- so the rule is pinned here rather than left to
# review. Every case asserts the outcome for one probe answer, including the
# ones that mean "could not tell", which must refuse rather than proceed.
#
# Deterministic: git, curl and docker are stubbed on PATH, so no network.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT="$ROOT_DIR/scripts/release/prove_candidate_version_unused.sh"
[[ -x "$SCRIPT" ]] || { echo "missing $SCRIPT" >&2; exit 1; }

failures=0

# OCI_TAKEN names an image whose tag exists, so a case can make exactly one of
# the two repositories collide. Probing only one repository is a regression the
# registered invariant claims this test catches.
run_case() {
  local name="$1" want="$2" git_exit="$3" http="$4" oci_exit="$5" oci_msg="$6" oci_taken="${7:-}"
  local stub; stub="$(mktemp -d)"
  local probed="$stub/probed"
  : > "$probed"

  cat > "$stub/git" <<EOF
#!/usr/bin/env bash
exit ${git_exit}
EOF
  cat > "$stub/curl" <<EOF
#!/usr/bin/env bash
printf '%s' "${http}"
EOF
  cat > "$stub/docker" <<EOF
#!/usr/bin/env bash
# Record the reference so the test can assert both repositories were probed.
printf '%s\n' "\$*" >> "${probed}"
if [[ -n "${oci_taken}" ]] && [[ "\$*" == *"/${oci_taken}:"* ]]; then
  exit 0
fi
printf '%s\n' "${oci_msg}" >&2
exit ${oci_exit}
EOF
  chmod +x "$stub/git" "$stub/curl" "$stub/docker"

  set +e
  PATH="$stub:$PATH" GH_TOKEN=stub "$SCRIPT" owner/repo owner v1.2.3 test >/dev/null 2>&1
  local got=$?
  set -e

  local outcome="refused"; [[ "$got" -eq 0 ]] && outcome="accepted"
  if [[ "$outcome" != "$want" ]]; then
    echo "  FAIL ${name}: ${outcome}, want ${want}" >&2
    failures=$((failures + 1))
  else
    echo "  ok   ${name}: ${outcome}"
  fi

  # On the clear path every repository must have been probed; a helper that
  # silently stopped checking one would otherwise pass every case above.
  if [[ "$want" == "accepted" ]]; then
    local image
    for image in nerve-runtime neuralmaild; do
      if ! grep -q "/${image}:" "$probed"; then
        echo "  FAIL ${name}: ${image} was never probed" >&2
        failures=$((failures + 1))
      fi
    done
  fi
  rm -rf "$stub"
}

echo "uniqueness probe contract:"
#        name                              want       git  http  oci  oci message
run_case "all clear"                       accepted   2    404   1    "manifest unknown"
run_case "git tag exists"                  refused    0    404   1    "manifest unknown"
run_case "git probe unreachable"           refused    128  404   1    "manifest unknown"
run_case "git probe other error"           refused    1    404   1    "manifest unknown"
run_case "release exists"                  refused    2    200   1    "manifest unknown"
run_case "release api error"               refused    2    500   1    "manifest unknown"
run_case "release api unauthorized"        refused    2    401   1    "manifest unknown"
run_case "oci tag exists"                  refused    2    404   0    ""
run_case "oci proxy 404 is not absence"    refused    2    404   1    "404 Not Found"
run_case "oci missing cred helper"         refused    2    404   1    "executable file not found"
run_case "oci unauthorized"                refused    2    404   1    "unauthorized: authentication required"
run_case "oci no such manifest"            accepted   2    404   1    "no such manifest: ghcr.io/o/i:v1"
run_case "only nerve-runtime taken"        refused    2    404   1    "manifest unknown"  nerve-runtime
run_case "only neuralmaild taken"          refused    2    404   1    "manifest unknown"  neuralmaild

# The invariant covers the call sites too, not just the classifier: a helper
# that is correct but invoked in only one phase reopens the post-build window.
echo "workflow call sites:"
workflow="$ROOT_DIR/.github/workflows/docker-publish.yml"
for phase in preflight postbuild; do
  if grep -q "prove_candidate_version_unused.sh" "$workflow" && \
     grep -qE "\\\"\\$\\{FINAL_VERSION\\}\\\" ${phase}|${phase}\\s*$" "$workflow"; then
    echo "  ok   ${phase} call site present"
  else
    echo "  FAIL ${phase} call site missing from docker-publish.yml" >&2
    failures=$((failures + 1))
  fi
done

if [[ "$failures" -ne 0 ]]; then
  echo "${failures} uniqueness-probe contract case(s) failed" >&2
  exit 1
fi
echo "all uniqueness probe cases hold"
