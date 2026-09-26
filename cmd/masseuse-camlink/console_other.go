//go:build !windows

package main

import "os"

// Outside Windows the program runs in a terminal it was started from, or
// in the Terminal window the macOS bundle opens for it (desktop_darwin.go,
// whose .command file sets the window's title): nothing to set up, and an
// error exit leaves the window as it was.

func consoleSetup() {}

func exit(code int) { os.Exit(code) }
