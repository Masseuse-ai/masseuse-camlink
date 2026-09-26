#!/bin/sh
# Fetch and verify the unit driver helpers of every helpers release this
# tree pins, for one platform, into one directory (docs/UNITS.md, section
# 4). packaging/units/VERSION names the releases, one per line, each the
# release's path under masseuse.ai/app/units/ (a bare version, or a helper
# family's prefix and its version); each is fetched and checked by fetch.sh
# on its own, and the helpers of all of them are then put together. Two
# releases naming the same helper file is an error: a release cannot stand
# in for another's helper. Anything that fails leaves nothing in OUTDIR.
#
# usage: sh packaging/units/fetch-all.sh OS ARCH OUTDIR
#   OS       darwin, windows or linux
#   ARCH     all (the universal macOS binaries), amd64, arm64 or arm
#   OUTDIR   where the helpers land, one file each; (re)created
# environment:
#   UNITS_VERSION_FILE  the list of releases (default packaging/units/VERSION
#                       beside this script); tests point it elsewhere
#   UNITS_BASE_URL      passed on to fetch.sh
set -eu

os="${1:-}"; arch="${2:-}"; outdir="${3:-}"
[ -n "$os" ] && [ -n "$arch" ] && [ -n "$outdir" ] || { echo "usage: $0 OS ARCH OUTDIR" >&2; exit 2; }

here=$(cd "$(dirname "$0")" && pwd)
list="${UNITS_VERSION_FILE:-$here/VERSION}"
[ -f "$list" ] || { echo "no releases list at $list" >&2; exit 2; }
# One release per line; blank lines and comments (#) are skipped, and a
# CR (a checkout with Windows line endings) is dropped.
releases=$(tr -d '\r' < "$list" | sed 's/#.*//' | tr -s '[:space:]' '\n' | sed '/^$/d')
[ -n "$releases" ] || { echo "$list pins no helpers release" >&2; exit 2; }

work=$(mktemp -d "${TMPDIR:-/tmp}/camlink-units-all.XXXXXX")
trap 'rm -rf "$work"' EXIT INT TERM
mkdir -p "$work/out"

n=0
for release in $releases; do
  n=$((n + 1))
  sh "$here/fetch.sh" "$release" "$os" "$arch" "$work/$n"
  for f in "$work/$n"/*; do
    name=$(basename "$f")
    if [ -e "$work/out/$name" ]; then
      echo "$name is named by two releases ($release and an earlier one); a helper belongs to one release" >&2
      exit 1
    fi
    cp "$f" "$work/out/$name"
  done
done

rm -rf "$outdir"
mkdir -p "$outdir"
cp "$work/out"/* "$outdir/"
echo "helpers of $n release(s) for $os/$arch in $outdir: $(ls "$outdir" | tr '\n' ' ')"
