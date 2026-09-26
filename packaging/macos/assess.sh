#!/bin/sh
# Gatekeeper's verdict on the signed bundle or disk image, the way a Mac
# reaches it when the file is opened: the checks the release fails on if any
# is false, and the ones VERIFY.md gives Mac users. Output leaves out the
# certificate's subject (spctl's origin line, codesign's Authority lines).
#
# usage: sh packaging/macos/assess.sh app PATH/Masseuse.app
#        sh packaging/macos/assess.sh dmg PATH/Masseuse.ai-X.Y.Z.dmg
#        sh packaging/macos/assess.sh entitlements PATH/Masseuse.app
#
# `entitlements` reads the signatures' entitlements back (also part of
# `app`): the bundle's executable and Contents/Helpers/ffmpeg must carry
# the camera and microphone entitlements, the connector and the unit
# driver helpers must carry neither (sign-app.sh, device.entitlements).
# Without them on the executable, macOS refuses the camera to ffmpeg
# without a prompt, as v0.16.0 and v0.17.0 found out; this check runs on
# the release's bundle, on the bundle after it has updated itself
# (update-check-macos) and, ad hoc, on every pull request's.
set -eu

APPLE_TEAM_ID="B8Z4RP3846"

kind="${1:-}"; target="${2:-}"
[ -n "$target" ] && [ -e "$target" ] || { echo "usage: $0 app|dmg|entitlements PATH" >&2; exit 2; }

# signature FILE [runtime]: the signature's identifier, team, hashes and
# timestamp; the team must be FemLed's, and the hardened runtime set if asked.
signature() {
  info=$(codesign -dvv "$1" 2>&1)
  echo "$info" | grep -E '^(Identifier|TeamIdentifier|CodeDirectory|Timestamp)' | sed 's/^/  /'
  echo "$info" | grep -q "^TeamIdentifier=$APPLE_TEAM_ID\$" || { echo "$1: not signed by team $APPLE_TEAM_ID" >&2; exit 1; }
  if [ "${2:-}" = runtime ]; then
    echo "$info" | grep -q 'flags=0x10000(runtime)' || { echo "$1: hardened runtime not set" >&2; exit 1; }
  fi
}

# verdict FILE TYPE [spctl options]: Gatekeeper's assessment, which must be
# accepted with a notarization ticket behind it.
verdict() {
  file="$1"; shift
  out=$(spctl --assess -vv "$@" "$file" 2>&1 || true)
  echo "$out" | grep -v '^origin=' | sed 's/^/  /'
  echo "$out" | grep -q ': accepted$' || { echo "$file: not accepted by Gatekeeper" >&2; exit 1; }
  echo "$out" | grep -q '^source=Notarized Developer ID$' || { echo "$file: accepted, but not as Notarized Developer ID" >&2; exit 1; }
}

# entitlement FILE KEY: the value of KEY in FILE's signed entitlements, or
# nothing when the signature carries no such key (or no entitlements). The
# dots are escaped: plutil reads an unescaped one as a step down a key path.
entitlement() {
  codesign -d --entitlements - --xml "$1" 2>/dev/null \
    | plutil -extract "$(printf '%s' "$2" | sed 's/\./\\./g')" raw -o - - 2>/dev/null || true
}

# entitled FILE: FILE's signature grants the camera and the microphone.
entitled() {
  for key in com.apple.security.device.camera com.apple.security.device.audio-input; do
    [ "$(entitlement "$1" "$key")" = true ] || { echo "$1: the signature does not carry $key" >&2; exit 1; }
  done
  echo "  $1: camera and microphone entitled"
}

# unentitled FILE: FILE's signature grants neither device.
unentitled() {
  for key in com.apple.security.device.camera com.apple.security.device.audio-input; do
    [ -z "$(entitlement "$1" "$key")" ] || { echo "$1: the signature carries $key, which it is not to have" >&2; exit 1; }
  done
  echo "  $1: no device entitlements"
}

# entitlements BUNDLE: who may open the camera and the microphone. The
# executable (the responsible process) and ffmpeg (the accessing one) must;
# the connector and the helpers must not.
entitlements() {
  exe=$(plutil -extract CFBundleExecutable raw -o - "$1/Contents/Info.plist")
  entitled "$1/Contents/MacOS/$exe"
  entitled "$1/Contents/Helpers/ffmpeg"
  unentitled "$1/Contents/MacOS/masseuse-camlink"
  for unit in "$1"/Contents/Helpers/units/camlink-unit-*; do
    [ -f "$unit" ] || continue
    unentitled "$unit"
  done
}

case "$kind" in
  app)
    echo "==> $target"
    codesign --verify --deep --strict --verbose=2 "$target"
    signature "$target" runtime
    signature "$target/Contents/MacOS/masseuse-camlink" runtime >/dev/null
    signature "$target/Contents/Helpers/ffmpeg" runtime >/dev/null
    entitlements "$target"
    verdict "$target" --type execute
    xcrun stapler validate "$target"
    ;;
  entitlements)
    echo "==> $target"
    [ -d "$target/Contents" ] || { echo "$target is not an application bundle" >&2; exit 2; }
    entitlements "$target"
    echo "entitled: the executable and ffmpeg may open the camera and the microphone; the connector and the helpers may not"
    exit 0
    ;;
  dmg)
    echo "==> $target"
    codesign --verify --strict --verbose=2 "$target"
    signature "$target"
    verdict "$target" --type open --context context:primary-signature
    xcrun stapler validate "$target"
    ;;
  *) echo "usage: $0 app|dmg|entitlements PATH" >&2; exit 2 ;;
esac
echo "accepted: Notarized Developer ID, team $APPLE_TEAM_ID, ticket stapled"
