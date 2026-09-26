#!/bin/sh
# Put Masseuse.ai in this desktop's menu: the entry (masseuse.desktop) with
# this directory's path filled in, under ~/.local/share/applications. The
# program itself stays here, where the archive was unpacked; it updates
# itself in place (README.md, "Updates"). Run again after moving the
# directory; remove the entry with -u.
#
# usage: sh install.sh [-u]
set -eu
here=$(cd "$(dirname "$0")" && pwd)
apps="${XDG_DATA_HOME:-$HOME/.local/share}/applications"
entry="$apps/ai.masseuse.camlink.desktop"
if [ "${1:-}" = "-u" ]; then
  rm -f "$entry"
  echo "removed $entry"
  exit 0
fi
[ -x "$here/Masseuse" ] || { echo "no Masseuse in $here" >&2; exit 1; }
mkdir -p "$apps"
sed "s|@DIR@|$here|g" "$here/masseuse.desktop" > "$entry"
chmod 644 "$entry"
command -v update-desktop-database >/dev/null 2>&1 && update-desktop-database "$apps" >/dev/null 2>&1 || true
echo "installed $entry (Masseuse.ai in the applications menu; the program runs from $here)"
