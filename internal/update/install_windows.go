package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// stage unpacks the archive's zip (the package is one file and is staged
// by stagePackage).
func (i *Installer) stage(_ context.Context, s *Staged, dir string) (string, error) {
	if i.Install.Layout == LayoutBundle {
		return "", errors.New("update: no application bundles on this system")
	}
	return i.stageFiles(s, dir)
}

func (i *Installer) swapBundle(string, string) error {
	return errors.New("update: no application bundles on this system")
}

// sameVolume is for bundles, which this system has none of.
func sameVolume(string, string) bool { return false }

// Restart starts the new program at the install's path with args, sharing
// this process's console (the window stays while the child lives), and
// returns; the caller exits. Windows has no exec.
func (i *Installer) Restart(args []string, env []string) error {
	cmd := exec.Command(i.Install.Exe, args...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%w: %v", ErrRestart, err)
	}
	// Not waited on: the child outlives this process.
	_ = cmd.Process.Release()
	return nil
}
