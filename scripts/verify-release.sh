#!/usr/bin/env sh
# Verify a masseuse-camlink release end to end (VERIFY.md):
#   1. the checksum file's keyless signature was made by this repository's
#      release workflow on the tag,
#   2. every downloaded artifact matches the checksum file,
#   3. the SLSA provenance names each artifact and this source at this tag,
#   4. (optional) the container image is signed and has provenance,
#   5. (optional, needs Go) the gateway binary rebuilds to the same bytes,
#   6. (optional, needs Go) so does the connector in the linux/amd64 archive.
#
# usage: scripts/verify-release.sh vX.Y.Z [download dir]
# needs: curl, sha256sum (or shasum), cosign (3 or later for the image),
# slsa-verifier; docker or crane and go optional.
set -eu

REPO="FemLed/masseuse-camlink"
IMAGE="ghcr.io/femled/masseuse-camlink"
WORKFLOW_RE='^https://github.com/FemLed/masseuse-camlink/\.github/workflows/release\.yml@refs/tags/v'
ISSUER="https://token.actions.githubusercontent.com"

tag="${1:?usage: $0 vX.Y.Z [dir]}"
version="${tag#v}"
dir="${2:-release-$tag}"
mkdir -p "$dir"
cd "$dir"

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing: $1" >&2; exit 2; }; }
need curl; need cosign; need slsa-verifier
if command -v sha256sum >/dev/null 2>&1; then SHA="sha256sum"; else need shasum; SHA="shasum -a 256"; fi

base="https://github.com/$REPO/releases/download/$tag"
fetch() { [ -s "$1" ] || curl -fsSL -o "$1" "$base/$1"; }

echo "==> downloading release files for $tag"
fetch checksums.txt
fetch checksums.txt.sigstore.json
fetch multiple.intoto.jsonl
# Every artifact named in the checksum file.
awk '{print $2}' checksums.txt | while read -r f; do fetch "$f"; done

echo "==> 1. checksum signature (keyless, release workflow at a v* tag)"
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp "$WORKFLOW_RE" \
  --certificate-oidc-issuer "$ISSUER" \
  checksums.txt

echo "==> 2. checksums"
$SHA -c checksums.txt

echo "==> 3. SLSA provenance for every artifact"
awk '{print $2}' checksums.txt | while read -r f; do
  slsa-verifier verify-artifact "$f" \
    --provenance-path multiple.intoto.jsonl \
    --source-uri "github.com/$REPO" \
    --source-tag "$tag" >/dev/null
  echo "    ok  $f"
done

if ! command -v docker >/dev/null 2>&1 && ! command -v crane >/dev/null 2>&1; then
  echo "==> 4. container image: skipped (no docker or crane)"
elif [ "$(cosign version 2>&1 | sed -n 's/^GitVersion: *v\([0-9]*\).*/\1/p')" -lt 3 ] 2>/dev/null; then
  # The release workflow signs the image with cosign 3, which attaches the
  # signature as a Sigstore bundle (an OCI referrer); cosign 2 cannot read
  # it and reports "no signatures found".
  echo "==> 4. container image: skipped (cosign 3 or later is needed for the image signature; $(cosign version 2>&1 | sed -n 's/^GitVersion: *//p') found)"
else
  echo "==> 4. container image"
  if command -v crane >/dev/null 2>&1; then
    digest=$(crane digest "$IMAGE:$tag")
  else
    # The digest of the index is the hash of its bytes; --raw prints them
    # exactly, where --format templates differ between buildx versions.
    digest="sha256:$(docker buildx imagetools inspect --raw "$IMAGE:$tag" | $SHA | cut -d' ' -f1)"
  fi
  echo "    $IMAGE@$digest"
  cosign verify "$IMAGE@$digest" \
    --certificate-identity-regexp "$WORKFLOW_RE" \
    --certificate-oidc-issuer "$ISSUER" >/dev/null
  echo "    ok  keyless signature"
  slsa-verifier verify-image "$IMAGE@$digest" \
    --source-uri "github.com/$REPO" --source-tag "$tag" >/dev/null
  echo "    ok  provenance"
fi

if command -v go >/dev/null 2>&1; then
  echo "==> 5. rebuild the gateway from the module proxy and compare (VERIFY.md 3)"
  export GOTOOLCHAIN=go1.27.1 CGO_ENABLED=0 GOPROXY=https://proxy.golang.org,direct GOSUMDB=sum.golang.org GOFLAGS=
  # A scratch GOPATH so the cross-compiled binary has a known home (GOBIN is
  # not allowed for cross builds); the module cache stays shared, so it is
  # resolved before GOPATH is overridden (a shell applies the assignments
  # before a command left to right).
  work="$(mktemp -d)"
  modcache="$(go env GOMODCACHE)"
  GOPATH="$work/gopath" GOMODCACHE="$modcache" GOOS=linux GOARCH=amd64 \
    go install -trimpath -buildvcs=false -ldflags='-s -w -buildid=' \
    "github.com/FemLed/masseuse-camlink/cmd/masseuse-camlink-gateway@$tag"
  built=$(find "$work/gopath/bin" -type f -name masseuse-camlink-gateway | head -n 1)
  [ -n "$built" ] || { echo "    go install produced no binary" >&2; exit 1; }
  rebuilt=$($SHA "$built" | cut -d' ' -f1)
  published=$(grep -F "masseuse-camlink-gateway_${version}_linux_amd64" checksums.txt | cut -d' ' -f1)
  if [ "$rebuilt" = "$published" ]; then
    echo "    ok  linux/amd64 gateway reproduces: $rebuilt"
  else
    echo "    MISMATCH: rebuilt $rebuilt, published $published" >&2
    exit 1
  fi

  echo "==> 6. rebuild the connector the way goreleaser's proxy mode does and compare"
  mkdir -p "$work/connector"
  printf 'module connector\n' > "$work/connector/go.mod"
  (cd "$work/connector" \
    && go get "github.com/FemLed/masseuse-camlink@$tag" >/dev/null 2>&1 \
    && GOOS=linux GOARCH=amd64 go build -mod=mod -trimpath -buildvcs=false -ldflags='-s -w -buildid=' \
         -o masseuse-camlink github.com/FemLed/masseuse-camlink/cmd/masseuse-camlink)
  rebuilt=$($SHA "$work/connector/masseuse-camlink" | cut -d' ' -f1)
  published=$(tar -xzOf "masseuse-camlink_${version}_linux_amd64.tar.gz" masseuse-camlink | $SHA | cut -d' ' -f1)
  if [ "$rebuilt" = "$published" ]; then
    echo "    ok  linux/amd64 connector reproduces: $rebuilt"
  else
    echo "    MISMATCH: rebuilt $rebuilt, in archive $published" >&2
    exit 1
  fi
  chmod -R u+w "$work" 2>/dev/null || true
  rm -rf "$work"
else
  echo "==> 5-6. rebuild: skipped (no go)"
fi

echo "all checks passed for $tag"
