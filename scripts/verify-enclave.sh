#!/usr/bin/env sh
# Verify where the enclave your camera streams to was built from
# (VERIFY.md, "The enclave your camera streams to").
#
# The connector dials an enclave only after verifying its Confidential
# Space attestation against the service's policy
# (https://masseuse.ai/api/tee-policy): the image must carry a signature
# from the enclave repository's release key, be at or above the policy's
# minimum release, and pass the rest of the checks. The token names the
# image digest and, for images built since the release stamp exists, the
# release tag and source commit (TEE_IMAGE_VERSION, TEE_IMAGE_COMMIT in the
# attested environment). This script takes that digest and tag, from a live
# enclave's attestation or from the connector's "enclave verified" log line,
# and checks against the public registry:
#   1. slsa-verifier: the provenance attached to the image names the source
#      repository (at that tag, when known) as what produced this digest;
#   2. cosign: the image is signed keyless by that repository's release
#      workflow at a v* tag.
# The digest is the identity: the enclave attests it, the registry serves
# it, the provenance covers it. Nothing here needs an account anywhere.
#
# usage: scripts/verify-enclave.sh [--service URL] --origin https://slot-N.tee.masseuse.ai
#        scripts/verify-enclave.sh [--service URL] sha256:<digest>[@vX.Y.Z] ...
# needs: curl, jq, slsa-verifier, cosign (3 or later); openssl for --origin.
set -eu

SERVICE="https://masseuse.ai"
ORIGIN=""
ISSUER="https://token.actions.githubusercontent.com"

while [ $# -gt 0 ]; do
  case "$1" in
    --service) SERVICE="${2:?--service needs a URL}"; shift 2 ;;
    --service=*) SERVICE="${1#--service=}"; shift ;;
    --origin) ORIGIN="${2:?--origin needs an https origin}"; shift 2 ;;
    --origin=*) ORIGIN="${1#--origin=}"; shift ;;
    -h|--help) sed -n '2,24p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    sha256:*) break ;;
    *) echo "unexpected argument: $1" >&2; exit 2 ;;
  esac
done

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing: $1" >&2; exit 2; }; }
need curl; need jq; need slsa-verifier; need cosign

# base64url -> text, padding restored (the token's segments carry none).
b64url_decode() {
  s=$(printf '%s' "$1" | tr '_-' '/+')
  case $(( ${#s} % 4 )) in 2) s="$s==" ;; 3) s="$s=" ;; esac
  printf '%s' "$s" | base64 -d 2>/dev/null || printf '%s' "$s" | base64 -D
}

echo "==> policy from ${SERVICE%/}/api/tee-policy"
policy=$(curl -fsSL "${SERVICE%/}/api/tee-policy")
src=$(printf '%s' "$policy" | jq -r '.sourceUri // empty')
repo=$(printf '%s' "$policy" | jq -r '.imageRepo // empty')
min=$(printf '%s' "$policy" | jq -r '.minRelease // empty')
[ -n "$min" ] && echo "    the connector requires release $min or later"

if [ -n "$ORIGIN" ]; then
  [ $# -eq 0 ] || { echo "--origin and digests are exclusive" >&2; exit 2; }
  need openssl
  nonce=$(openssl rand 24 | base64 | tr '+/' '-_' | tr -d =)
  echo "==> attestation from ${ORIGIN%/}/attestation"
  token=$(curl -fsSL "${ORIGIN%/}/attestation?nonce=$nonce" | jq -r .token)
  claims=$(b64url_decode "$(printf '%s' "$token" | cut -d. -f2)")
  digest=$(printf '%s' "$claims" | jq -r '.submods.container.image_digest // empty')
  tag=$(printf '%s' "$claims" | jq -r '.submods.container.env.TEE_IMAGE_VERSION // empty')
  commit=$(printf '%s' "$claims" | jq -r '.submods.container.env.TEE_IMAGE_COMMIT // empty')
  keys=$(printf '%s' "$claims" | jq -r '[.submods.container.image_signatures[]?.key_id] | join(",")')
  [ -n "$digest" ] || { echo "the attestation names no image digest" >&2; exit 1; }
  echo "    image   $digest"
  echo "    release ${tag:-unstamped (built before the release stamp existed)}${commit:+ at commit $commit}"
  echo "    signed by key id(s) ${keys:-none} (the connector requires one of: $(printf '%s' "$policy" | jq -r '.imageSignatures | join(",")'))"
  echo "    (this script reads the token; the connector verifies its signature and every claim before it dials)"
  set -- "$digest${tag:+@$tag}"
fi

[ $# -gt 0 ] || { echo "name an enclave (--origin) or a digest" >&2; exit 2; }

failed=0
for arg in "$@"; do
  digest=${arg%%@*}
  tag=""
  case "$arg" in *@*) tag=${arg#*@} ;; esac
  case "$digest" in sha256:????????????????????????????????????????????????????????????????) ;; *)
    echo "not a digest: $digest" >&2; exit 2 ;;
  esac
  if printf '%s' "$policy" | jq -e '.allowedImageDigests | length > 0' >/dev/null \
      && ! printf '%s' "$policy" | jq -e --arg d "$digest" '.allowedImageDigests | index($d)' >/dev/null; then
    echo "==> $digest is NOT in the policy's digest list: the connector would refuse this enclave"
    failed=1
    continue
  fi
  drepo=$repo; dsrc=$src
  if [ -z "$dsrc" ]; then
    # Older policies record the source per digest.
    source=$(printf '%s' "$policy" | jq -c --arg d "$digest" '.imageSources[$d] // empty')
    if [ -z "$source" ]; then
      echo "==> $digest: the policy names no source repository; the attestation holds, the source cannot be checked here"
      continue
    fi
    drepo=$(printf '%s' "$source" | jq -r .repo)
    dsrc=$(printf '%s' "$source" | jq -r .sourceUri)
    [ -n "$tag" ] || tag=$(printf '%s' "$source" | jq -r .tag)
  fi
  echo "==> $digest"
  echo "    built from $dsrc${tag:+ at $tag}, published at $drepo"
  if [ -n "$tag" ]; then
    set -- slsa-verifier verify-image "$drepo@$digest" --source-uri "$dsrc" --source-tag "$tag"
  else
    set -- slsa-verifier verify-image "$drepo@$digest" --source-uri "$dsrc"
  fi
  if "$@" >/dev/null; then
    echo "    ok  provenance: $dsrc${tag:+@$tag} produced this digest"
  else
    echo "    FAIL provenance"; failed=1
  fi
  if cosign verify "$drepo@$digest" \
      --certificate-identity-regexp "^https://github.com/${dsrc#github.com/}/\\.github/workflows/release\\.yml@refs/tags/v" \
      --certificate-oidc-issuer "$ISSUER" >/dev/null 2>&1; then
    echo "    ok  signature: keyless, by the release workflow of $dsrc at a v* tag"
  else
    echo "    FAIL signature"; failed=1
  fi
done

if [ "$failed" -ne 0 ]; then
  echo "==> FAILED"; exit 1
fi
echo "==> verified: the digest was built by the source repository's release workflow${tag:+ at $tag}"
