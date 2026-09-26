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

func handToTerminal(string) error {
	return errors.New("-app is for the macOS application bundle; on this system run the program in a terminal")
}

func reportHandoffFailure(err error) {
	fmt.Fprintln(os.Stderr, err)
}
