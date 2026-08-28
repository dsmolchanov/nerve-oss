#!/usr/bin/env bash
# Prove a frozen candidate semver is unused across every place it can be owned.
#
# A candidate freezes a version without creating it, so this runs twice: once
# before the build, and again after, because the image build is long enough for
# another writer to claim the identity in between.
#
# Every probe must distinguish "absent" from "could not tell". A transient DNS
# failure, an expired token, or a registry auth error all exit nonzero, and
# treating that as absence would freeze an identity someone else owns. Only a
# positive proof of absence is accepted; anything else fails closed.
set -euo pipefail

REPOSITORY="${1:?usage: prove_candidate_version_unused.sh <owner/repo> <owner> <vX.Y.Z> <phase>}"
OWNER="${2:?missing owner}"
VERSION="${3:?missing version}"
PHASE="${4:-check}"

: "${GH_TOKEN:?GH_TOKEN is required}"

# git ls-remote --exit-code: 0 found, 2 no match, anything else an error.
set +e
git ls-remote --exit-code --tags "https://github.com/${REPOSITORY}.git" \
  "refs/tags/${VERSION}" >/dev/null 2>&1
tag_status=$?
set -e
case "${tag_status}" in
  0) echo "${PHASE}: ${VERSION} exists as a git tag" >&2; exit 1 ;;
  2) ;;
  *) echo "${PHASE}: git tag probe failed with status ${tag_status}; cannot prove ${VERSION} unused" >&2; exit 1 ;;
esac

# Releases: read the HTTP status directly, so 404 is the only proof of absence.
release_status="$(curl --silent --show-error \
  --output /dev/null --write-out '%{http_code}' \
  --header 'Accept: application/vnd.github+json' \
  --header "Authorization: Bearer ${GH_TOKEN}" \
  --header 'X-GitHub-Api-Version: 2022-11-28' \
  "https://api.github.com/repos/${REPOSITORY}/releases/tags/${VERSION}")"
case "${release_status}" in
  404) ;;
  200) echo "${PHASE}: ${VERSION} exists as a GitHub Release" >&2; exit 1 ;;
  *) echo "${PHASE}: release probe returned HTTP ${release_status}; cannot prove ${VERSION} unused" >&2; exit 1 ;;
esac

# Both published repositories: docker-publish.yml pushes released tags to each,
# so each is a place the identity can already be owned. Only the registry's own
# manifest-unknown signal proves absence -- a generic "not found" also matches a
# proxy's "404 Not Found" and a missing credential helper's "executable file not
# found", neither of which says anything about the tag.
for image in nerve-runtime neuralmaild; do
  set +e
  oci_error="$(docker manifest inspect "ghcr.io/${OWNER}/${image}:${VERSION}" 2>&1 >/dev/null)"
  oci_status=$?
  set -e
  if [[ "${oci_status}" -eq 0 ]]; then
    echo "${PHASE}: ${VERSION} exists as an OCI tag on ${image}" >&2; exit 1
  fi
  if ! grep -qiE 'manifest unknown|manifest_unknown|no such manifest' <<<"${oci_error}"; then
    echo "${PHASE}: OCI probe for ${image} failed without proving absence: ${oci_error}" >&2; exit 1
  fi
done

echo "${PHASE}: ${VERSION} is proven unused across git tags, Releases, and both OCI repositories"
