#!/bin/sh
# Assemble the Windows download, Masseuse.ai-<version>-windows.zip: the
# published windows_amd64 connector under the name people see, ffmpeg.exe
# beside it, the notices and a README. Everything sits at the zip's root, so
# Explorer's "Extract All" gives one folder with the program in it. Plain
# sh: runs on the Linux CI runner (a stand-in ffmpeg on pull requests) and
# under Git's bash on the Windows release runner. Nothing is signed here.
#
# usage: sh packaging/windows/build-zip.sh -v VERSION -b CONNECTOR -f FFMPEG_DIR -o OUTDIR
#   VERSION     the release version without the v
#   CONNECTOR   the masseuse-camlink.exe to pack (goreleaser's windows_amd64
#               binary); it becomes Masseuse.ai.exe, the same bytes
#   FFMPEG_DIR  packaging/ffmpeg/build.sh -t windows's output directory:
#               ffmpeg.exe and licenses/ (their absence is an error: the
#               package promises a camera without an install step)
#   OUTDIR      OUTDIR/Masseuse.ai-VERSION-windows.zip is (re)created
# needs: zip (Info-ZIP), or 7z where there is none (the Windows runner)
set -eu

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
version="" connector="" ffmpegdir="" outdir=""
while [ $# -gt 0 ]; do
  case "$1" in
    -v) version="$2"; shift 2 ;;
    -b) connector="$2"; shift 2 ;;
    -f) ffmpegdir="$2"; shift 2 ;;
    -o) outdir="$2"; shift 2 ;;
    -h|--help) sed -n '2,17p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
[ -n "$version" ] && [ -n "$connector" ] && [ -n "$ffmpegdir" ] && [ -n "$outdir" ] ||
  { echo "usage: $0 -v VERSION -b CONNECTOR -f FFMPEG_DIR -o OUTDIR" >&2; exit 2; }
case "$version" in
  [0-9]*.[0-9]*.[0-9]*) ;;
  *) echo "version $version is not X.Y.Z" >&2; exit 2 ;;
esac
[ -f "$connector" ] || { echo "no connector at $connector" >&2; exit 2; }
[ -f "$ffmpegdir/ffmpeg.exe" ] || { echo "no ffmpeg.exe at $ffmpegdir/ffmpeg.exe (packaging/ffmpeg/build.sh -t windows)" >&2; exit 2; }
[ -d "$ffmpegdir/licenses" ] || { echo "no $ffmpegdir/licenses (packaging/ffmpeg/build.sh)" >&2; exit 2; }
if command -v zip >/dev/null 2>&1; then
  ziptool=zip
elif command -v 7z >/dev/null 2>&1; then
  ziptool=7z
else
  echo "neither zip nor 7z is available" >&2; exit 2
fi
mkdir -p "$outdir"
outdir=$(cd "$outdir" && pwd)

name="Masseuse.ai"
zipfile="$outdir/$name-$version-windows.zip"
staging=$(mktemp -d "${TMPDIR:-/tmp}/camlink-zip.XXXXXX")
trap 'rm -rf "$staging"' EXIT
mkdir -p "$staging/licenses"

cp "$connector" "$staging/$name.exe"
cp "$ffmpegdir/ffmpeg.exe" "$staging/ffmpeg.exe"
cp "$here/README.txt" "$staging/README.txt"
cp "$repo/LICENSE" "$repo/NOTICE" "$staging/"
cp "$here/../ffmpeg/THIRD_PARTY.md" "$staging/THIRD_PARTY.md"
cp "$ffmpegdir"/licenses/* "$staging/licenses/"
# Text files with Windows line endings, so Notepad shows them as lines.
(cd "$staging" && for f in README.txt LICENSE NOTICE THIRD_PARTY.md licenses/*; do
  sed 's/\r$//; s/$/\r/' "$f" > "$f.crlf" && mv "$f.crlf" "$f"
done)

# The executable in the package is the connector byte for byte: the gate
# the workflow relies on to tie the zip to the archive it verified.
if command -v sha256sum >/dev/null 2>&1; then SHA="sha256sum"; else SHA="shasum -a 256"; fi
a=$($SHA "$connector" | cut -d' ' -f1)
b=$($SHA "$staging/$name.exe" | cut -d' ' -f1)
[ "$a" = "$b" ] || { echo "$name.exe differs from $connector" >&2; exit 1; }

rm -f "$zipfile"
case "$ziptool" in
  zip) (cd "$staging" && zip -q -X -r "$zipfile" .) ;;
  7z) (cd "$staging" && 7z a -tzip -mx=9 -bso0 -bsp0 "$zipfile" . >/dev/null) ;;
esac
echo "wrote $zipfile ($(wc -c < "$zipfile" | tr -d ' ') bytes); $name.exe sha256 $a"
case "$ziptool" in
  zip) unzip -Z1 "$zipfile" | sort | sed 's/^/  /' ;;
  7z) 7z l -ba -slt "$zipfile" | tr -d '\r' | sed -n 's/^Path = //p' | sort | sed 's/^/  /' ;;
esac
