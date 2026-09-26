package main

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockExclusive locks the first byte of f without waiting (LockFileEx). The
// lock belongs to the handle, so closing f releases it, and another handle
// on the file, in this process or another, is refused while it is held.
func lockExclusive(f *os.File) error {
	var ov windows.Overlapped
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ov)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return errAlreadyRunning
	}
	return err
}
