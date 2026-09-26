#!/bin/sh
# Fetch the unit driver helpers for one platform from masseuse.ai and verify
# them (docs/UNITS.md, section 4): the helpers version's manifest and its
# cosign bundle are downloaded, the bundle is checked against cosign.pub
# here (the helpers' key, a public PR to change), then each file the
# manifest lists for OS/ARCH is downloaded and its SHA-256 compared with the
# manifest's. Anything that does not match is an error and nothing is left
# in OUTDIR. Nothing here needs a secret; the release workflow runs it on
# every runner that bundles helpers, and anyone may run it to see what a
# release bundled.
#
# usage: sh packaging/units/fetch.sh VERSION OS ARCH OUTDIR
#   VERSION  the helpers version (packaging/units/VERSION), bare: 1.0.0
#   OS       darwin, windows or linux
#   ARCH     all (the universal macOS binaries), amd64, arm64 or arm
#   OUTDIR   where the helpers land, one file each (camlink-unit-<name>[.exe]);
#            (re)created
# environment:
#   UNITS_BASE_URL  where the versions are served (default
#                   https://masseuse.ai/app/units); tests point it elsewhere
# needs: curl, cosign, jq, sha256sum or shasum
set -eu

version="${1:-}"; os="${2:-}"; arch="${3:-}"; outdir="${4:-}"
[ -n "$version" ] && [ -n "$os" ] && [ -n "$arch" ] && [ -n "$outdir" ] ||
  { echo "usage: $0 VERSION OS ARCH OUTDIR" >&2; exit 2; }
case "$version" in
  [0-9]*.[0-9]*.[0-9]*) ;;
  *) echo "version $version is not X.Y.Z" >&2; exit 2 ;;
esac
case "$os" in darwin|windows|linux) ;; *) echo "os must be darwin, windows or linux" >&2; exit 2 ;; esac
case "$arch" in all|amd64|arm64|arm) ;; *) echo "arch must be all, amd64, arm64 or arm" >&2; exit 2 ;; esac
for tool in curl cosign jq; do
  command -v "$tool" >/dev/null 2>&1 || { echo "$tool is needed" >&2; exit 2; }
done
if command -v sha256sum >/dev/null 2>&1; then SHA="sha256sum"; else SHA="shasum -a 256"; fi

here=$(cd "$(dirname "$0")" && pwd)
pub="$here/cosign.pub"
[ -f "$pub" ] || { echo "no $pub" >&2; exit 2; }
base="${UNITS_BASE_URL:-https://masseuse.ai/app/units}/$version"

work=$(mktemp -d "${TMPDIR:-/tmp}/camlink-units.XXXXXX")
trap 'rm -rf "$work"' EXIT INT TERM

fetch() { # URL FILE
  curl -fsSL --retry 3 --retry-delay 5 -o "$2" "$1" || { echo "could not fetch $1" >&2; exit 1; }
}

echo "==> helpers $version for $os/$arch from $base"
fetch "$base/manifest.json" "$work/manifest.json"
fetch "$base/manifest.json.sigstore.json" "$work/manifest.json.sigstore.json"
cosign verify-blob --key "$pub" --bundle "$work/manifest.json.sigstore.json" "$work/manifest.json" >/dev/null 2>"$work/cosign.err" ||
  { cat "$work/cosign.err" >&2; echo "the manifest's signature does not verify with $pub" >&2; exit 1; }
echo "manifest signed by the helpers' key"
# jq on Windows ends its lines with CR LF; the CR is dropped everywhere.
mversion=$(jq -r '.version' "$work/manifest.json" | tr -d '\r')
[ "$mversion" = "$version" ] || { echo "the manifest says version $mversion, not $version" >&2; exit 1; }

# The files for this platform: "name sha256" per line.
jq -r --arg os "$os" --arg arch "$arch" '.files[] | select(.os == $os and .arch == $arch) | "\(.name) \(.sha256)"' \
  "$work/manifest.json" | tr -d '\r' > "$work/files.txt"
[ -s "$work/files.txt" ] || { echo "the manifest lists no helper for $os/$arch" >&2; exit 1; }

mkdir -p "$work/out"
while read -r name sum; do
  case "$name" in
    camlink-unit-*) ;;
    *) echo "the manifest names $name, which is not a helper" >&2; exit 1 ;;
  esac
  case "$name" in
    */*|*\\*|*..*) echo "the manifest names $name, which is not a file name" >&2; exit 1 ;;
  esac
  fetch "$base/${os}_${arch}/$name" "$work/out/$name"
  got=$($SHA "$work/out/$name" | cut -d' ' -f1)
  [ "$got" = "$sum" ] || { echo "$name: sha256 $got, the manifest says $sum" >&2; exit 1; }
  chmod 755 "$work/out/$name"
  echo "verified $name ($sum)"
done < "$work/files.txt"

rm -rf "$outdir"
mkdir -p "$outdir"
cp "$work/out"/* "$outdir/"
echo "helpers $version for $os/$arch in $outdir: $(ls "$outdir" | tr '\n' ' ')"
