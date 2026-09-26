#!/bin/sh
# Generate the Windows resource objects the connector is linked with: the
# icon Explorer shows for Masseuse.exe and the strings its Details tab
# lists (winres.json: ProductName Masseuse.ai, CompanyName FemLed, Inc.,
# FileDescription "Masseuse.ai for your computer"). The Go linker picks up
# cmd/masseuse-camlink/rsrc_windows_<arch>.syso by name on a windows build,
# without cgo, so the objects are committed and every build, goreleaser's
# proxy build and the reproduce rebuild alike, links the same bytes. No
# version number is put in: the files change only when the icon or the
# strings do, and the version is what the binary's build information says
# (VERIFY.md, "What the version string tells you").
#
# usage: sh packaging/windows/make-syso.sh     (then commit the two .syso files)
# needs: go; go-winres is fetched at the pinned version below and run with
#        go run, so nothing is installed.
set -eu

GO_WINRES=github.com/tc-hib/go-winres@v0.3.3

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
out="$repo/cmd/masseuse-camlink"
command -v go >/dev/null 2>&1 || { echo "missing: go" >&2; exit 2; }

# go-winres resolves the icon path against the working directory.
cd "$here"
GOFLAGS='' go run "$GO_WINRES" make --in winres.json --out "$out/rsrc" --arch amd64,arm64
for arch in amd64 arm64; do
  f="$out/rsrc_windows_$arch.syso"
  [ -s "$f" ] || { echo "go-winres did not write $f" >&2; exit 1; }
  echo "wrote $f ($(wc -c < "$f" | tr -d ' ') bytes)"
done
