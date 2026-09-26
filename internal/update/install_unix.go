//go:build !darwin && !windows

package update

import (
	"context"
	"errors"
	"fmt"
	"syscall"
)

// stage unpacks the archive; there are no bundles here.
func (i *Installer) stage(_ context.Context, s *Staged, dir string) (string, error) {
	if i.Install.Layout == LayoutBundle {
		return "", errors.New("update: no application bundles on this system")
	}
	return i.stageFiles(s, dir)
}

func (i *Installer) swapBundle(string, string) error {
	return errors.New("update: no application bundles on this system")
}

// Restart replaces this process with the new program at the install's
// path, with args; the pid and the terminal are kept.
func (i *Installer) Restart(args []string, env []string) error {
	argv := append([]string{i.Install.Exe}, args...)
	if err := syscall.Exec(i.Install.Exe, argv, env); err != nil {
		return fmt.Errorf("%w: %v", ErrRestart, err)
	}
	return nil
}
