package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// One connector per state directory: two would offer the same identity and
// the same camera to the service, and the second one opened from the
// application bundle would otherwise silently start beside the first. The
// lock is the operating system's on a file in the state directory, so it
// goes away with the process however that ends.

// lockFile is the lock's file, in the state directory.
const lockFile = "lock"

// errAlreadyRunning is returned by lockInstance when another process holds
// the state directory's lock.
var errAlreadyRunning = errors.New("masseuse-camlink is already running on this state directory")

// lockInstance takes the state directory's lock and returns the function
// that releases it. errAlreadyRunning means another connector has it.
func lockInstance(stateDir string) (release func(), err error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(stateDir, lockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockExclusive(f); err != nil {
		f.Close()
		if errors.Is(err, errAlreadyRunning) {
			return nil, err
		}
		return nil, fmt.Errorf("lock %s: %w", f.Name(), err)
	}
	return func() { f.Close() }, nil
}
