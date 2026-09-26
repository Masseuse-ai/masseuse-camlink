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

// sameVolume says whether two paths are on one file system.
func sameVolume(a, b string) bool {
	var sa, sb syscall.Stat_t
	if syscall.Stat(a, &sa) != nil || syscall.Stat(b, &sb) != nil {
		return false
	}
	return sa.Dev == sb.Dev
}

// restart replaces this process with the new program at the install's
// path, with args; the pid and the terminal are kept.
func (i *Installer) restart(args []string, env []string) error {
	argv := append([]string{i.Install.Exe}, args...)
	if err := syscall.Exec(i.Install.Exe, argv, env); err != nil {
		return fmt.Errorf("%w: %v", ErrRestart, err)
	}
	return nil
}
