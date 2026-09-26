#!/bin/sh
# Wrap Masseuse.app in a disk image the way Mac users expect: the app and
# an Applications shortcut side by side, the volume wearing the app's icon.
# macOS only (hdiutil). The image is signed and notarized afterwards by
# sign-notarize.sh.
#
# usage: sh packaging/macos/build-dmg.sh -a APP -v VERSION -o OUTDIR
#   writes OUTDIR/Masseuse.ai-VERSION.dmg (compressed, read-only), the
#   volume named Masseuse.ai
set -eu

here=$(cd "$(dirname "$0")" && pwd)
app="" version="" outdir=""
while [ $# -gt 0 ]; do
  case "$1" in
    -a) app="$2"; shift 2 ;;
    -v) version="$2"; shift 2 ;;
    -o) outdir="$2"; shift 2 ;;
    -h|--help) sed -n '2,10p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
[ -n "$app" ] && [ -n "$version" ] && [ -n "$outdir" ] || { echo "usage: $0 -a APP -v VERSION -o OUTDIR" >&2; exit 2; }
[ -d "$app/Contents" ] || { echo "$app is not an application bundle" >&2; exit 2; }
command -v hdiutil >/dev/null 2>&1 || { echo "hdiutil is needed (macOS)" >&2; exit 2; }
mkdir -p "$outdir"
outdir=$(cd "$outdir" && pwd)

volume="Masseuse.ai"
dmg="$outdir/Masseuse.ai-${version}.dmg"
work=$(mktemp -d "${TMPDIR:-/tmp}/camlink-dmg.XXXXXX")
mount=""
cleanup() {
  [ -z "$mount" ] || hdiutil detach "$mount" -quiet -force >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

staging="$work/staging"
mkdir -p "$staging"
cp -R "$app" "$staging/"
ln -s /Applications "$staging/Applications"
cp "$here/masseuse-camlink.icns" "$staging/.VolumeIcon.icns"

# A writable image first, so the volume's custom-icon flag can be set on the
# mounted root (the flag lives in the directory's Finder info), then a
# compressed read-only one from it.
rw="$work/rw.dmg"
hdiutil create -quiet -volname "$volume" -srcfolder "$staging" -fs HFS+ -format UDRW -ov "$rw"
mount=$(hdiutil attach -readwrite -noverify -noautoopen -nobrowse "$rw" | awk -F'\t' '/\/Volumes\//{print $NF}')
[ -n "$mount" ] || { echo "the image did not mount" >&2; exit 1; }
if setfile=$(xcrun -f SetFile 2>/dev/null) || setfile=$(command -v SetFile); then
  "$setfile" -a C "$mount"
else
  echo "SetFile not found; the volume keeps the default icon" >&2
fi
sync
hdiutil detach "$mount" -quiet
mount=""

rm -f "$dmg"
hdiutil convert -quiet "$rw" -format UDZO -imagekey zlib-level=9 -o "$dmg"
hdiutil verify -quiet "$dmg"
echo "wrote $dmg ($(wc -c < "$dmg" | tr -d ' ') bytes)"
