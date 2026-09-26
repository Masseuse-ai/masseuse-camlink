#!/bin/sh
# Gatekeeper's verdict on the signed bundle or disk image, the way a Mac
# reaches it when the file is opened: the checks the release fails on if any
# is false, and the ones VERIFY.md gives Mac users. Output leaves out the
# certificate's subject (spctl's origin line, codesign's Authority lines).
#
# usage: sh packaging/macos/assess.sh app PATH/Masseuse.ai.app
#        sh packaging/macos/assess.sh dmg PATH/Masseuse.ai-X.Y.Z.dmg
set -eu

APPLE_TEAM_ID="B8Z4RP3846"

kind="${1:-}"; target="${2:-}"
[ -n "$target" ] && [ -e "$target" ] || { echo "usage: $0 app|dmg PATH" >&2; exit 2; }

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

case "$kind" in
  app)
    echo "==> $target"
    codesign --verify --deep --strict --verbose=2 "$target"
    signature "$target" runtime
    signature "$target/Contents/Helpers/ffmpeg" runtime >/dev/null
    verdict "$target" --type execute
    xcrun stapler validate "$target"
    ;;
  dmg)
    echo "==> $target"
    codesign --verify --strict --verbose=2 "$target"
    signature "$target"
    verdict "$target" --type open --context context:primary-signature
    xcrun stapler validate "$target"
    ;;
  *) echo "usage: $0 app|dmg PATH" >&2; exit 2 ;;
esac
echo "accepted: Notarized Developer ID, team $APPLE_TEAM_ID, ticket stapled"
