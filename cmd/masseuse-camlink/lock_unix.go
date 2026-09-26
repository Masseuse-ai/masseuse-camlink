//go:build !windows

package main

import (
	"errors"
	"os"
	"syscall"
)

// lockExclusive takes a flock(2) on f without waiting. The lock belongs to
// the open file, so closing f releases it, and a second open of the same
// file, in this process or another, is refused while it is held.
func lockExclusive(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return errAlreadyRunning
	}
	return err
}
