#!/bin/sh
# Assemble the Linux desktop archive, Masseuse.ai-<version>-linux-<arch>.tar.gz:
# the desktop window (desktop/), the connector, the unit driver helpers,
# the desktop entry and its installer, the icon and the notices, all at the
# archive's root (the updater unpacks it flat over the install directory,
# internal/update LayoutDesktop). Nothing is signed: the release's
# checksums-linux.txt and its keyless signature and provenance cover it.
#
# usage: sh packaging/linux/build-archive.sh -v VERSION -a ARCH -s SHELL -b CONNECTOR [-u UNITS_DIR] -o OUTDIR
#   VERSION     the release version without the v
#   ARCH        amd64 (the one architecture the window is built for)
#   SHELL       the desktop window (cd desktop && wails3 task build)
#   CONNECTOR   the masseuse-camlink binary for linux/ARCH (the published archive's)
#   UNITS_DIR   the unit driver helpers for linux/ARCH (camlink-unit-*); without -u none
#   OUTDIR      OUTDIR/Masseuse.ai-VERSION-linux-ARCH.tar.gz is (re)created
set -eu

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
version="" arch="" shell="" connector="" unitsdir="" outdir=""
while [ $# -gt 0 ]; do
  case "$1" in
    -v) version="$2"; shift 2 ;;
    -a) arch="$2"; shift 2 ;;
    -s) shell="$2"; shift 2 ;;
    -b) connector="$2"; shift 2 ;;
    -u) unitsdir="$2"; shift 2 ;;
    -o) outdir="$2"; shift 2 ;;
    -h|--help) sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
[ -n "$version" ] && [ -n "$arch" ] && [ -n "$shell" ] && [ -n "$connector" ] && [ -n "$outdir" ] ||
  { echo "usage: $0 -v VERSION -a ARCH -s SHELL -b CONNECTOR [-u UNITS_DIR] -o OUTDIR" >&2; exit 2; }
case "$version" in
  [0-9]*.[0-9]*.[0-9]*) ;;
  *) echo "version $version is not X.Y.Z" >&2; exit 2 ;;
esac
[ -f "$shell" ] || { echo "no desktop window at $shell (cd desktop && wails3 task build)" >&2; exit 2; }
[ -f "$connector" ] || { echo "no connector at $connector" >&2; exit 2; }
if [ -n "$unitsdir" ]; then
  ls "$unitsdir"/camlink-unit-* >/dev/null 2>&1 || { echo "no camlink-unit-* in $unitsdir" >&2; exit 2; }
fi

# GNU tar, for the ownership and time options below (gtar on a Mac).
tar=tar
tar --version 2>/dev/null | grep -q 'GNU tar' || tar=gtar
command -v "$tar" >/dev/null 2>&1 || { echo "GNU tar is needed (gtar on a Mac: brew install gnu-tar)" >&2; exit 2; }

name="Masseuse.ai-$version-linux-$arch"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
stage="$work/stage"
mkdir -p "$stage"
cp "$shell" "$stage/Masseuse"
cp "$connector" "$stage/masseuse-camlink"
chmod 755 "$stage/Masseuse" "$stage/masseuse-camlink"
if [ -n "$unitsdir" ]; then
  mkdir -p "$stage/units"
  for f in "$unitsdir"/camlink-unit-*; do
    case "$(basename "$f")" in *.exe) echo "$f is a Windows helper" >&2; exit 2 ;; esac
    cp "$f" "$stage/units/$(basename "$f")"
    chmod 755 "$stage/units/$(basename "$f")"
  done
fi
cp "$here/masseuse.desktop" "$here/install.sh" "$here/README.txt" "$stage/"
chmod 755 "$stage/install.sh"
cp "$repo/packaging/macos/masseuse-camlink-icon-1024.png" "$stage/masseuse.png"
cp "$repo/LICENSE" "$repo/NOTICE" "$repo/README.md" "$repo/VERIFY.md" "$stage/"

mkdir -p "$outdir"
out="$outdir/$name.tar.gz"
rm -f "$out"
# Flat, owned by no one in particular, with fixed times: the same input
# gives the same archive.
(cd "$stage" && find . -type f | sort | sed 's|^\./||' | \
  "$tar" --owner=0 --group=0 --numeric-owner --mtime='2026-01-01 00:00:00Z' --format=gnu -cf - -T - ) | gzip -n -9 > "$out"
echo "assembled $out"
tar -tzf "$out" | sed 's/^/  /'
