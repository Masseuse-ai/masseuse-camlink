#!/bin/sh
# Assemble Masseuse.app: the connector (universal or thin) as the
# bundle's executable, ffmpeg as a helper, the icon, the notices. Nothing is
# signed here (sign-notarize.sh does that); the script needs only sh, so
# ci.yml builds the bundle unsigned on every pull request.
#
# usage: sh packaging/macos/build-app.sh -v VERSION -b CONNECTOR -f FFMPEG_DIR -o OUTDIR
#   VERSION     the release version without the v (CFBundleShortVersionString)
#   CONNECTOR   the masseuse-camlink binary to bundle (lipo -create'd for a
#               universal app); it becomes Contents/MacOS/Masseuse
#   FFMPEG_DIR  packaging/ffmpeg/build.sh's output directory: ffmpeg and
#               licenses/ (their absence is an error: the bundle promises
#               a camera without an install step)
#   OUTDIR      OUTDIR/Masseuse.app is (re)created
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
    -h|--help) sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
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
[ -f "$ffmpegdir/ffmpeg" ] || { echo "no ffmpeg at $ffmpegdir/ffmpeg (packaging/ffmpeg/build.sh)" >&2; exit 2; }
[ -d "$ffmpegdir/licenses" ] || { echo "no $ffmpegdir/licenses (packaging/ffmpeg/build.sh)" >&2; exit 2; }

# The bundle, its executable and the name under the icon. It is Masseuse,
# not Masseuse.ai: the Finder and Launchpad refuse to hide .app when the rest
# of the name ends in a file extension the system knows (a guard against
# Invoice.pdf.app), and .ai is Adobe Illustrator's, declared by macOS itself,
# so Masseuse.ai.app was shown as "Masseuse.ai.app" (v0.8.0 and v0.8.1). The
# disk image, its volume, the window title and the first line printed still
# say Masseuse.ai (packaging/macos/README.md).
name="Masseuse"
app="$outdir/$name.app"
rm -rf "$app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Helpers" "$app/Contents/Resources/licenses"

cp "$connector" "$app/Contents/MacOS/$name"
cp "$ffmpegdir/ffmpeg" "$app/Contents/Helpers/ffmpeg"
chmod 755 "$app/Contents/MacOS/$name" "$app/Contents/Helpers/ffmpeg"
cp "$here/masseuse-camlink.icns" "$app/Contents/Resources/masseuse-camlink.icns"
cp "$repo/LICENSE" "$repo/NOTICE" "$app/Contents/Resources/"
cp "$here/../ffmpeg/THIRD_PARTY.md" "$app/Contents/Resources/THIRD_PARTY.md"
cp "$ffmpegdir"/licenses/* "$app/Contents/Resources/licenses/"
sed "s/@VERSION@/$version/g" "$here/Info.plist" > "$app/Contents/Info.plist"
printf 'APPL????' > "$app/Contents/PkgInfo"

if grep -q '@VERSION@' "$app/Contents/Info.plist"; then
  echo "Info.plist still has the placeholder" >&2; exit 1
fi
if command -v plutil >/dev/null 2>&1; then
  plutil -lint "$app/Contents/Info.plist"
  for key in CFBundleExecutable CFBundleName CFBundleDisplayName; do
    got=$(plutil -extract "$key" raw -o - "$app/Contents/Info.plist")
    [ "$got" = "$name" ] || { echo "Info.plist $key is $got, not $name" >&2; exit 1; }
  done
fi
echo "assembled $app ($version)"
find "$app" -type f | sort | sed "s|^$outdir/|  |"
