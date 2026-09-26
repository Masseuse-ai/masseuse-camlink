//go:build !windows

package main

import "os/exec"

// hideWindow is for Windows, where a console program opens a window of its
// own; nothing to do elsewhere.
func hideWindow(*exec.Cmd) {}
