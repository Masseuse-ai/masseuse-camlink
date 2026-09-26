//go:build !darwin

package main

import (
	"errors"
	"fmt"
	"os"
)

// The application bundle is macOS's; elsewhere the program is started from
// a shell, or by Windows opening a console window for it.

func launchedFromBundle() bool { return false }

// stdinInteractive says whether standard input is a console someone types
// at, so the unit picker (estim.go) may read from it: a character device
// (a terminal, or the console window Windows opens), not a pipe or a file.
func stdinInteractive() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func handToTerminal(string) error {
	return errors.New("-app is for the macOS application bundle; on this system run the program in a terminal")
}

func reportHandoffFailure(err error) {
	fmt.Fprintln(os.Stderr, err)
}
