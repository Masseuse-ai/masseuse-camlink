package main

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// hideWindow keeps the connector, a console program, from opening a
// console window of its own beside the shell's: it gets no console at all
// (its standard streams are the shell's pipes), so its exit never waits
// for Enter either (cmd/masseuse-camlink/console_windows.go).
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}
