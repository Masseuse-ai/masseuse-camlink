#!/bin/sh
# Sign the application bundle: the one signing plan, run by sign-notarize.sh
# with FemLed's Apple Developer ID for a release and by ci.yml ad hoc
# (identity "-") on every pull request, so the plan itself is exercised
# before a tag and assess.sh's `entitlements` check has the same bundle to
# read on both.
#
# usage: sh packaging/macos/sign-app.sh -i IDENTITY [-k KEYCHAIN] PATH/Masseuse.app
#   -i IDENTITY   a codesign identity: the certificate's SHA-1, or "-" for an
#                 ad-hoc signature (no timestamp, nothing Gatekeeper accepts)
#   -k KEYCHAIN   the keychain holding the identity (sign-notarize.sh's)
#
# The programs inside are signed first, each with the hardened runtime:
# ffmpeg (identifier ai.masseuse.camlink.ffmpeg, device.entitlements: it
# opens the camera and the microphone), each unit driver helper in
# Contents/Helpers/units (ai.masseuse.camlink.unit.<name>, no entitlements:
# a helper gets no camera or microphone; docs/UNITS.md) and the connector,
# Contents/MacOS/masseuse-camlink (ai.masseuse.camlink.connector, no
# entitlements: it opens neither device, and macOS does not hold it to
# account for ffmpeg's opening them). Then the bundle, whose executable is
# the desktop window, with device.entitlements too: macOS attributes what
# ffmpeg opens to the application it runs under, the responsible process,
# and under the hardened runtime refuses a responsible process without the
# entitlement, silently (device.entitlements). codesign puts the bundle's
# entitlements on its executable alone; nothing here uses --deep for
# signing, so no program inside inherits what it should not have.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
identity=""; keychain=""
while getopts "i:k:" opt; do
  case "$opt" in
    i) identity="$OPTARG" ;;
    k) keychain="$OPTARG" ;;
    *) echo "usage: $0 -i IDENTITY [-k KEYCHAIN] PATH/Masseuse.app" >&2; exit 2 ;;
  esac
done
shift $((OPTIND - 1))
target="${1:-}"
[ -n "$identity" ] && [ -n "$target" ] || { echo "usage: $0 -i IDENTITY [-k KEYCHAIN] PATH/Masseuse.app" >&2; exit 2; }
[ -d "$target/Contents" ] || { echo "$target is not an application bundle" >&2; exit 2; }
[ -f "$target/Contents/Helpers/ffmpeg" ] || { echo "$target has no Contents/Helpers/ffmpeg" >&2; exit 2; }
[ -f "$target/Contents/MacOS/masseuse-camlink" ] || { echo "$target has no Contents/MacOS/masseuse-camlink" >&2; exit 2; }
entitlements="$here/device.entitlements"
plutil -lint "$entitlements" >/dev/null

# An ad-hoc signature cannot carry an Apple timestamp.
sign="codesign --force --sign $identity"
[ "$identity" = "-" ] || sign="$sign --timestamp"
[ -n "$keychain" ] && sign="$sign --keychain $keychain"

# describe FILE: the signature's identifier, flags and timestamp, without the
# certificate's subject (the Authority lines): this runs in a public job log.
describe() {
  codesign -dvv "$1" 2>&1 | grep -E '^(Identifier|TeamIdentifier|CodeDirectory|Timestamp|Signature)' || true
}

echo "==> signing $target"
$sign --options runtime --identifier ai.masseuse.camlink.ffmpeg \
  --entitlements "$entitlements" "$target/Contents/Helpers/ffmpeg"
describe "$target/Contents/Helpers/ffmpeg"
for unit in "$target"/Contents/Helpers/units/camlink-unit-*; do
  [ -f "$unit" ] || continue
  uname=$(basename "$unit"); uname=${uname#camlink-unit-}
  $sign --options runtime --identifier "ai.masseuse.camlink.unit.$uname" "$unit"
  describe "$unit"
done
$sign --options runtime --identifier ai.masseuse.camlink.connector "$target/Contents/MacOS/masseuse-camlink"
describe "$target/Contents/MacOS/masseuse-camlink"
$sign --options runtime --entitlements "$entitlements" "$target"
codesign --verify --deep --strict --verbose=2 "$target"
describe "$target"
echo "signed: $target"
