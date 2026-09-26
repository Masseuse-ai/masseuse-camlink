#!/bin/sh
# Sign, notarize and staple the application bundle or the disk image with
# FemLed's Apple Developer ID, from a temporary keychain that lives for this
# script only. The signing identity is chosen by its certificate's SHA-1
# (the one VERIFY.md lists), never by its subject, and the subject is not
# printed: this runs in a public job log.
#
# usage: sh packaging/macos/sign-notarize.sh app PATH/masseuse-camlink.app
#        sh packaging/macos/sign-notarize.sh dmg PATH/masseuse-camlink_X.Y.Z_darwin_all.dmg
# environment, all required (the release workflow's secrets):
#   MACOS_SIGN_P12          the Developer ID Application certificate with its
#                           private key and chain, .p12, base64
#   MACOS_SIGN_PASSWORD     that file's password
#   MACOS_NOTARY_KEY        the App Store Connect API key, .p8, base64
#   MACOS_NOTARY_KEY_ID     its key id
#   MACOS_NOTARY_ISSUER_ID  its issuer id
# For the bundle, ffmpeg is signed first (hardened runtime, with
# ffmpeg.entitlements for the camera and microphone), then the bundle
# (hardened runtime); the bundle is zipped for the notary service and
# stapled. The disk image is signed, notarized and stapled as it is.
set -eu

# The leaf certificate's SHA-1 fingerprint (VERIFY.md, "The macOS binaries";
# its SHA-256 is there too). A replacement certificate is listed there first.
APPLE_CERT_SHA1="F286A0BD8A535628964BF7147528B3F3E5E02879"

here=$(cd "$(dirname "$0")" && pwd)
kind="${1:-}"; target="${2:-}"
case "$kind" in
  app) [ -d "$target/Contents" ] || { echo "$target is not an application bundle" >&2; exit 2; } ;;
  dmg) [ -f "$target" ] || { echo "$target is not a file" >&2; exit 2; } ;;
  *) echo "usage: $0 app|dmg PATH" >&2; exit 2 ;;
esac
for v in MACOS_SIGN_P12 MACOS_SIGN_PASSWORD MACOS_NOTARY_KEY MACOS_NOTARY_KEY_ID MACOS_NOTARY_ISSUER_ID; do
  eval "val=\${$v:-}"
  [ -n "$val" ] || { echo "$v is not set" >&2; exit 2; }
done

work=$(mktemp -d "${TMPDIR:-/tmp}/camlink-sign.XXXXXX")
chmod 700 "$work"
keychain="$work/sign.keychain-db"
cleanup() {
  security delete-keychain "$keychain" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT INT TERM

# A keychain of its own, unlocked for this run, holding only the identity.
# set-key-partition-list lets codesign use the key without a prompt; its
# output names the certificate, so it is discarded.
kcpass=$(openssl rand -hex 24)
security create-keychain -p "$kcpass" "$keychain"
security set-keychain-settings -lut 7200 "$keychain"
security unlock-keychain -p "$kcpass" "$keychain"
printf '%s' "$MACOS_SIGN_P12" | base64 -d > "$work/sign.p12"
security import "$work/sign.p12" -k "$keychain" -P "$MACOS_SIGN_PASSWORD" -f pkcs12 \
  -T /usr/bin/codesign -T /usr/bin/security >/dev/null
security set-key-partition-list -S apple-tool:,apple:,codesign: -s -k "$kcpass" "$keychain" >/dev/null
# shellcheck disable=SC2046
security list-keychains -d user -s "$keychain" $(security list-keychains -d user | tr -d '" ')
rm -f "$work/sign.p12"
if ! security find-identity -v -p codesigning "$keychain" | grep -q "$APPLE_CERT_SHA1"; then
  echo "the certificate $APPLE_CERT_SHA1 (VERIFY.md) is not in MACOS_SIGN_P12" >&2
  exit 1
fi
echo "signing identity $APPLE_CERT_SHA1 ready"

printf '%s' "$MACOS_NOTARY_KEY" | base64 -d > "$work/notary.p8"

# notarize FILE: submit, wait, and stop with the service's log if it did not accept.
notarize() {
  echo "==> notarizing $(basename "$1")"
  xcrun notarytool submit "$1" --key "$work/notary.p8" --key-id "$MACOS_NOTARY_KEY_ID" \
    --issuer "$MACOS_NOTARY_ISSUER_ID" --wait --timeout 45m --output-format json > "$work/submit.json"
  status=$(plutil -extract status raw -o - "$work/submit.json")
  id=$(plutil -extract id raw -o - "$work/submit.json")
  echo "submission $id: $status"
  if [ "$status" != "Accepted" ]; then
    xcrun notarytool log "$id" --key "$work/notary.p8" --key-id "$MACOS_NOTARY_KEY_ID" --issuer "$MACOS_NOTARY_ISSUER_ID" || true
    exit 1
  fi
}

# describe FILE: the signature's team, flags and timestamp, without the
# certificate's subject (the Authority lines).
describe() {
  codesign -dvv "$1" 2>&1 | grep -E '^(Identifier|TeamIdentifier|CodeDirectory|Timestamp|Signature)' || true
}

sign="codesign --force --timestamp --sign $APPLE_CERT_SHA1 --keychain $keychain"
case "$kind" in
  app)
    echo "==> signing $target"
    $sign --options runtime --identifier ai.masseuse.camlink.ffmpeg \
      --entitlements "$here/ffmpeg.entitlements" "$target/Contents/Helpers/ffmpeg"
    $sign --options runtime "$target"
    codesign --verify --deep --strict --verbose=2 "$target"
    describe "$target"
    ditto -c -k --keepParent "$target" "$work/app.zip"
    notarize "$work/app.zip"
    xcrun stapler staple "$target"
    xcrun stapler validate "$target"
    ;;
  dmg)
    echo "==> signing $target"
    $sign "$target"
    codesign --verify --strict --verbose=2 "$target"
    describe "$target"
    notarize "$target"
    xcrun stapler staple "$target"
    xcrun stapler validate "$target"
    ;;
esac
echo "signed, notarized and stapled: $target"
