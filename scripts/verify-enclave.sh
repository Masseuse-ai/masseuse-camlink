#!/usr/bin/env sh
# Verify where the enclave your camera streams to was built from
# (VERIFY.md, "The enclave your camera streams to").
#
# The connector only dials an enclave whose Confidential Space attestation
# names an image digest in the service's policy (https://masseuse.ai/api/tee-policy).
# The policy also says, per digest, which public registry holds that exact
# digest with its SLSA provenance and keyless signature, and which source
# repository and tag it was built from. This script fetches the policy and,
# for each digest (or the ones you name, e.g. the "image" the connector
# logged at dial), checks:
#   1. slsa-verifier: the provenance attached to the image in the public
#      registry names that source repository at that tag, for that digest;
#   2. cosign: the image is signed keyless by that repository's release
#      workflow at a v* tag.
# The digest is the identity: the enclave attests it, the registry serves
# it, the provenance covers it. Nothing here needs an account anywhere.
#
# usage: scripts/verify-enclave.sh [--service URL] [sha256:<digest> ...]
# needs: curl, jq, slsa-verifier, cosign (3 or later).
set -eu

SERVICE="https://masseuse.ai"
ISSUER="https://token.actions.githubusercontent.com"

while [ $# -gt 0 ]; do
  case "$1" in
    --service) SERVICE="${2:?--service needs a URL}"; shift 2 ;;
    --service=*) SERVICE="${1#--service=}"; shift ;;
    -h|--help) sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    sha256:*) break ;;
    *) echo "unexpected argument: $1" >&2; exit 2 ;;
  esac
done

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing: $1" >&2; exit 2; }; }
need curl; need jq; need slsa-verifier; need cosign

echo "==> policy from ${SERVICE%/}/api/tee-policy"
policy=$(curl -fsSL "${SERVICE%/}/api/tee-policy")
if [ $# -gt 0 ]; then
  digests="$*"
else
  digests=$(printf '%s' "$policy" | jq -r '.allowedImageDigests[]')
fi

failed=0
for digest in $digests; do
  case "$digest" in sha256:????????????????????????????????????????????????????????????????) ;; *)
    echo "not a digest: $digest" >&2; exit 2 ;;
  esac
  if ! printf '%s' "$policy" | jq -e --arg d "$digest" '.allowedImageDigests | index($d)' >/dev/null; then
    echo "==> $digest is NOT in the policy: the connector would refuse this enclave"
    failed=1
    continue
  fi
  source=$(printf '%s' "$policy" | jq -c --arg d "$digest" '.imageSources[$d] // empty')
  if [ -z "$source" ]; then
    echo "==> $digest: allowed, but the policy lists no public build for it (built before the source was published); the attestation holds, the source cannot be checked"
    continue
  fi
  repo=$(printf '%s' "$source" | jq -r .repo)
  tag=$(printf '%s' "$source" | jq -r .tag)
  src=$(printf '%s' "$source" | jq -r .sourceUri)
  echo "==> $digest"
  echo "    built from $src at $tag, published at $repo"
  if slsa-verifier verify-image "$repo@$digest" --source-uri "$src" --source-tag "$tag" >/dev/null; then
    echo "    ok  provenance: $src@$tag produced this digest"
  else
    echo "    FAIL provenance"; failed=1
  fi
  if cosign verify "$repo@$digest" \
      --certificate-identity-regexp "^https://github.com/${src#github.com/}/\\.github/workflows/release\\.yml@refs/tags/v" \
      --certificate-oidc-issuer "$ISSUER" >/dev/null 2>&1; then
    echo "    ok  signature: keyless, by the release workflow of $src at a v* tag"
  else
    echo "    FAIL signature"; failed=1
  fi
done

if [ "$failed" -ne 0 ]; then
  echo "==> FAILED"; exit 1
fi
echo "==> every digest with a published build verified; the connector dials only digests in this list"
