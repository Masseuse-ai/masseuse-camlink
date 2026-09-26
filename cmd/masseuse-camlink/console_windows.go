package main

import (
	"bufio"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// On Windows the program is a console application: opened from Explorer
// (Masseuse.ai.exe in the zip), Windows gives it a console window of its
// own, so there is nothing to hand off to as the macOS bundle does. Two
// things make that window a fit place to read the pairing code: it carries
// the program's name, and it stays open when the program stops with an
// error, since a window that closes with its process takes the message
// with it. Closing the window is CTRL_CLOSE_EVENT, which Go delivers as
// SIGTERM (os/signal): the unit is put back to zero and released on the way
// out, as on Ctrl-C, within the seconds Windows allows for it.

var (
	kernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	procSetConsoleTitleW      = kernel32.NewProc("SetConsoleTitleW")
	procGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
)

// consoleSetup names the console window after the program. Started from a
// shell, the title is the shell's again once the program ends (cmd and
// PowerShell restore theirs); started from Explorer, the window is the
// program's alone.
func consoleSetup() {
	title, err := windows.UTF16PtrFromString(appName)
	if err != nil {
		return
	}
	_, _, _ = procSetConsoleTitleW.Call(uintptr(unsafe.Pointer(title)))
}

// ownsConsole reports whether this process is the only one attached to its
// console: the case when Explorer opened the program and gave it a window
// of its own, which closes the moment the process ends. Started from a
// shell, the shell is attached too. Without a console at all (a service, a
// redirected start) there is nothing to keep open.
func ownsConsole() bool {
	var pids [2]uint32
	n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	return n == 1
}

// exit ends the program with code. When the code is an error's and the
// console window would vanish with the process, it first waits for Enter,
// so the message above it can be read.
func exit(code int) {
	if code != 0 && ownsConsole() {
		fmt.Fprint(os.Stderr, "\nPress Enter to close this window.")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	}
	os.Exit(code)
}
