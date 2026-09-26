package main

import (
	"path/filepath"
	"strings"
)

// The macOS application bundle (packaging/macos) has this very program as
// its executable. Opened from the Finder it has no terminal to print the
// pairing code to, so it hands itself to one: it writes a small .command
// file into the state directory and asks the system to open it, which
// Terminal does by running it, and the file runs this program again, in
// that window, in console mode. The pieces that need no macOS are here so
// they can be tested anywhere; desktop_darwin.go does the opening.

// commandFile is the name of the file Terminal runs, in the state directory.
const commandFile = "masseuse-camlink.command"

// bundleExecutable reports whether exe is the main executable of a macOS
// application bundle: it lies in a <name>.app/Contents/MacOS directory.
func bundleExecutable(exe string) bool {
	dir := filepath.ToSlash(filepath.Dir(exe))
	return strings.HasSuffix(dir, ".app/Contents/MacOS")
}

// commandScript is the .command file Terminal runs: it names the window and
// replaces itself with this program in console mode, on the same state
// directory, so the connector in the window is the one the bundle carries.
func commandScript(exe, stateDir string) string {
	return "#!/bin/sh\n" +
		"# Written by masseuse-camlink when opened from its application bundle:\n" +
		"# Terminal runs this file, and this file runs the program in that window.\n" +
		"printf '\\033]0;masseuse-camlink\\007'\n" +
		"exec " + shellQuote(exe) + " -console -state-dir " + shellQuote(stateDir) + " \"$@\"\n"
}

// shellQuote quotes s for /bin/sh: single quotes, with any single quote in
// s closed, escaped and reopened.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
