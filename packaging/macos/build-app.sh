#!/bin/sh
# Assemble Masseuse.app: the desktop window (universal or thin) as the
# bundle's executable, the connector beside it, ffmpeg as a helper, the
# icon, the notices. Nothing is signed here (sign-notarize.sh does that);
# the script needs sh and Xcode 26's actool (for the icon, below), so
# ci.yml builds the bundle unsigned on every pull request.
#
# usage: sh packaging/macos/build-app.sh -v VERSION -s SHELL -b CONNECTOR -f FFMPEG_DIR [-u UNITS_DIR] -o OUTDIR
#   VERSION     the release version without the v (CFBundleShortVersionString)
#   SHELL       the desktop window (desktop/, built by `wails3 task build`,
#               lipo -create'd for a universal app); it becomes
#               Contents/MacOS/Masseuse, the bundle's executable
#   CONNECTOR   the masseuse-camlink binary (lipo -create'd likewise); it
#               becomes Contents/MacOS/masseuse-camlink, which the window
#               runs (docs/DESKTOP.md) and which `-console` runs in a
#               terminal
#   FFMPEG_DIR  packaging/ffmpeg/build.sh's output directory: ffmpeg and
#               licenses/ (their absence is an error: the bundle promises
#               a camera without an install step)
#   UNITS_DIR   packaging/units/fetch.sh's output for darwin/all: the unit
#               driver helpers (camlink-unit-*, docs/UNITS.md), copied into
#               Contents/Helpers/units/; without -u the bundle has none and
#               serves the Mastago alone
#   OUTDIR      OUTDIR/Masseuse.app is (re)created
set -eu

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
version="" shell="" connector="" ffmpegdir="" unitsdir="" outdir=""
while [ $# -gt 0 ]; do
  case "$1" in
    -v) version="$2"; shift 2 ;;
    -s) shell="$2"; shift 2 ;;
    -b) connector="$2"; shift 2 ;;
    -f) ffmpegdir="$2"; shift 2 ;;
    -u) unitsdir="$2"; shift 2 ;;
    -o) outdir="$2"; shift 2 ;;
    -h|--help) sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
[ -n "$version" ] && [ -n "$shell" ] && [ -n "$connector" ] && [ -n "$ffmpegdir" ] && [ -n "$outdir" ] ||
  { echo "usage: $0 -v VERSION -s SHELL -b CONNECTOR -f FFMPEG_DIR [-u UNITS_DIR] -o OUTDIR" >&2; exit 2; }
case "$version" in
  [0-9]*.[0-9]*.[0-9]*) ;;
  *) echo "version $version is not X.Y.Z" >&2; exit 2 ;;
esac
[ -f "$shell" ] || { echo "no desktop window at $shell (cd desktop && wails3 task build)" >&2; exit 2; }
[ -f "$connector" ] || { echo "no connector at $connector" >&2; exit 2; }
[ -f "$ffmpegdir/ffmpeg" ] || { echo "no ffmpeg at $ffmpegdir/ffmpeg (packaging/ffmpeg/build.sh)" >&2; exit 2; }
[ -d "$ffmpegdir/licenses" ] || { echo "no $ffmpegdir/licenses (packaging/ffmpeg/build.sh)" >&2; exit 2; }
if [ -n "$unitsdir" ]; then
  [ -d "$unitsdir" ] || { echo "no units directory at $unitsdir (packaging/units/fetch.sh)" >&2; exit 2; }
  ls "$unitsdir"/camlink-unit-* >/dev/null 2>&1 || { echo "no camlink-unit-* in $unitsdir (packaging/units/fetch.sh)" >&2; exit 2; }
fi

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

cp "$shell" "$app/Contents/MacOS/$name"
cp "$connector" "$app/Contents/MacOS/masseuse-camlink"
cp "$ffmpegdir/ffmpeg" "$app/Contents/Helpers/ffmpeg"
chmod 755 "$app/Contents/MacOS/$name" "$app/Contents/MacOS/masseuse-camlink" "$app/Contents/Helpers/ffmpeg"
# The unit driver helpers, where the connector looks for them
# (cmd/masseuse-camlink/helpers.go: Contents/Helpers/units in a bundle).
# Each must be universal like the app, or one architecture would run the
# connector without its units.
if [ -n "$unitsdir" ]; then
  mkdir -p "$app/Contents/Helpers/units"
  for f in "$unitsdir"/camlink-unit-*; do
    case "$(basename "$f")" in *.exe) echo "$f is a Windows helper" >&2; exit 2 ;; esac
    if command -v lipo >/dev/null 2>&1; then
      archs=$(lipo -archs "$f" 2>/dev/null || true)
      case "$archs" in
        *arm64*x86_64*|*x86_64*arm64*) ;;
        *) echo "$(basename "$f") is not universal ($archs); fetch darwin/all" >&2; exit 2 ;;
      esac
    fi
    cp "$f" "$app/Contents/Helpers/units/$(basename "$f")"
    chmod 755 "$app/Contents/Helpers/units/$(basename "$f")"
  done
fi
cp "$here/masseuse-camlink.icns" "$app/Contents/Resources/masseuse-camlink.icns"

# The icon a second time, for macOS 26, which draws app icons itself from
# layers (an Icon Composer document compiled into an asset catalog) and
# shrinks one that comes only as a bitmap onto a grey rounded square, the
# shape it did not fill. masseuse-camlink.icon is the same mark as the icns,
# as layers on the full canvas; actool (Xcode 26 or later) compiles it to
# Assets.car, which CFBundleIconName names; macOS 13 to 15 keep the icns
# (CFBundleIconFile). actool also writes a small icns of its own beside the
# catalog: that one is not taken.
actool=$(xcrun --find actool 2>/dev/null) ||
  { echo "actool not found: Xcode 26 or later compiles masseuse-camlink.icon (DEVELOPER_DIR selects an Xcode)" >&2; exit 1; }
actool_version=$("$actool" --version --output-format human-readable-text 2>/dev/null | sed -n 's/^short-bundle-version: //p')
case "$actool_version" in
  2[6-9].*|[3-9][0-9].*) ;;
  *) echo "actool $actool_version is too old for masseuse-camlink.icon: Xcode 26 or later (DEVELOPER_DIR selects an Xcode)" >&2; exit 1 ;;
esac
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
"$actool" "$here/masseuse-camlink.icon" --compile "$work" --app-icon masseuse-camlink \
  --platform macosx --minimum-deployment-target 13.0 --target-device mac \
  --output-partial-info-plist "$work/icon.plist" --output-format human-readable-text > "$work/actool.log" 2>&1 ||
  { cat "$work/actool.log" >&2; echo "actool could not compile masseuse-camlink.icon" >&2; exit 1; }
[ -f "$work/Assets.car" ] || { cat "$work/actool.log" >&2; echo "actool wrote no Assets.car" >&2; exit 1; }
cp "$work/Assets.car" "$app/Contents/Resources/Assets.car"

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
  # The catalog's icon is the one the plist names, and the icns is its namesake.
  for key in CFBundleIconName CFBundleIconFile; do
    got=$(plutil -extract "$key" raw -o - "$app/Contents/Info.plist")
    [ "$got" = "masseuse-camlink" ] || { echo "Info.plist $key is $got, not masseuse-camlink" >&2; exit 1; }
  done
  compiled=$(plutil -extract CFBundleIconName raw -o - "$work/icon.plist")
  [ "$compiled" = "masseuse-camlink" ] || { echo "actool compiled the icon as $compiled, not masseuse-camlink" >&2; exit 1; }
fi
echo "assembled $app ($version)"
find "$app" -type f | sort | sed "s|^$outdir/|  |"
