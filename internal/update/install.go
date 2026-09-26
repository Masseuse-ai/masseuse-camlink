package update

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Installer puts a verified download in place of the running install and
// hands the process over to it. Every layout goes through the same three
// steps: Stage (unpack and check, nothing touched yet), Swap (the current
// install moved aside as the previous one, the staged one moved in; any
// failure puts things back) and Restart.
type Installer struct {
	Install Install
	Logger  *slog.Logger
	// Run runs a program for its combined output, with a timeout; tests
	// replace it. Nil is the real thing.
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
	// Aside is the directory a bundle moved aside by Swap goes to (the
	// state directory's `previous`), when it is on the bundle's volume: an
	// Applications folder shows one application, not the new one and the
	// old one beside it. Empty, or another volume, and the previous bundle
	// is kept beside the new one under a hidden name (PreviousName).
	Aside string
}

// RelaunchExitCode is the exit code with which the program asks the shell
// that started it to start it again: the new version is in place at the
// same path (Restart, ErrRelaunch). The .command file the macOS bundle
// writes for Terminal runs the program in a loop on it.
const RelaunchExitCode = 75

// RelaunchEnv is the environment variable that shell sets ("1") so the
// program knows it will be started again on RelaunchExitCode.
const RelaunchEnv = "MASSEUSE_CAMLINK_RELAUNCH"

// ErrRelaunch is Restart's answer when the process is not to be replaced
// in place but ended with RelaunchExitCode: the shell that started it
// (RelaunchEnv) starts the new version. An execve from this process is
// not safe on a Mac: Go's runtime, before an exec on Darwin, waits for
// every preemption signal it sent to be received (golang.org/issue/41702),
// and a thread that never takes delivery (this program runs Go code on
// CoreBluetooth's own threads) leaves it waiting forever: 2026-09-17, the
// bundle swapped, "installed; restarting" logged, the old program still
// on screen an hour later.
var ErrRelaunch = errors.New("update: the shell that started the program starts the new version")

// ErrStartedApart is Restart's answer when the new version has been
// started as a program of its own (the bundle opened through
// LaunchServices, which gives it a Terminal window of its own) and this
// process is to end.
var ErrStartedApart = errors.New("update: the new version is starting on its own; this program ends")

func (i *Installer) logger() *slog.Logger {
	if i.Logger != nil {
		return i.Logger
	}
	return slog.New(slog.DiscardHandler)
}

func (i *Installer) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if i.Run != nil {
		return i.Run(ctx, name, args...)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// MaxUnpacked bounds what one archive may unpack to.
const MaxUnpacked = 1 << 30

// Stage unpacks the artifact into dir (created; removed on failure),
// verifies it as the platform does (the bundle's code signature and
// Gatekeeper verdict on macOS; the archive's contents elsewhere) and runs
// the new program's --version, which must name the tag. It returns the
// staged root: the new Masseuse.app, or the directory holding the new
// files.
func (i *Installer) Stage(ctx context.Context, s *Staged, dir string) (string, error) {
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("update: %w", err)
	}
	root, err := i.stage(ctx, s, dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	exe := i.stagedExe(root)
	if err := i.checkVersion(ctx, exe, s.Tag); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return root, nil
}

// stagedExe is the new program inside a staged root.
func (i *Installer) stagedExe(root string) string {
	switch i.Install.Layout {
	case LayoutBundle:
		return filepath.Join(root, "Contents", "MacOS", filepath.Base(i.Install.Exe))
	case LayoutPackage:
		return filepath.Join(root, PackageExe)
	}
	name := "masseuse-camlink"
	if i.Install.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(root, name)
}

// checkVersion runs the new program's --version and reads the tag off it
// ("vX.Y.Z go1.N.M").
func (i *Installer) checkVersion(ctx context.Context, exe, tag string) error {
	if _, err := os.Stat(exe); err != nil {
		return fmt.Errorf("update: the download has no program at %s: %w", exe, err)
	}
	out, err := i.run(ctx, exe, "--version")
	if err != nil {
		return fmt.Errorf("update: the new program does not run (%w): %s", err, strings.TrimSpace(string(out)))
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 || fields[0] != tag {
		return fmt.Errorf("update: the new program says it is %q, not %s", strings.TrimSpace(string(out)), tag)
	}
	return nil
}

// stageFiles unpacks an archive or the Windows zip into dir and makes the
// program executable; the staged root is dir itself.
func (i *Installer) stageFiles(s *Staged, dir string) (string, error) {
	if err := Unpack(s.Path, dir, MaxUnpacked); err != nil {
		return "", err
	}
	exe := i.stagedExe(dir)
	if err := os.Chmod(exe, 0o755); err != nil {
		return "", fmt.Errorf("update: %w", err)
	}
	if units, err := os.ReadDir(filepath.Join(dir, "units")); err == nil {
		for _, e := range units {
			if !e.IsDir() {
				_ = os.Chmod(filepath.Join(dir, "units", e.Name()), 0o755)
			}
		}
	}
	return dir, nil
}

// PreviousName is where the install moved aside goes: a bundle under Aside
// (the state directory's `previous/Masseuse.app`) when Aside is on the
// bundle's volume, otherwise beside the new one under a hidden name
// (.Masseuse.previous.app: the Finder and Launchpad do not list it);
// .previous/ inside the install for the files of a package or archive.
// Until 2026-09-17 a bundle went aside as Masseuse.previous.app in
// Applications, and a person saw two applications for a while.
func (i *Installer) PreviousName() string {
	if i.Install.Layout == LayoutBundle {
		base := strings.TrimSuffix(filepath.Base(i.Install.Root), ".app")
		if i.Aside != "" {
			// The directory has to be there to be on a volume at all.
			if err := os.MkdirAll(i.Aside, 0o700); err == nil && sameVolume(i.Aside, filepath.Dir(i.Install.Root)) {
				return filepath.Join(i.Aside, base+".app")
			}
		}
		return filepath.Join(filepath.Dir(i.Install.Root), "."+base+".previous.app")
	}
	return filepath.Join(i.Install.Root, ".previous")
}

// Swap puts the staged root in place: the bundle is renamed aside and the
// new one renamed in; for the other layouts each file of the staged set is
// moved aside into .previous/ and the new one moved in (a running program
// may be renamed on every platform, not overwritten). It returns the path
// of the previous install, for the new program to remove once it runs. On
// any failure the moves made so far are undone.
func (i *Installer) Swap(root string) (previous string, err error) {
	previous = i.PreviousName()
	_ = os.RemoveAll(previous)
	if i.Install.Layout == LayoutBundle {
		return previous, i.swapBundle(root, previous)
	}
	return previous, i.swapFiles(root, previous)
}

// swapFiles moves the staged set into the install directory.
func (i *Installer) swapFiles(staged, previous string) error {
	entries, err := os.ReadDir(staged)
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}
	if err := os.MkdirAll(previous, 0o700); err != nil {
		return fmt.Errorf("update: %w", err)
	}
	exeName := filepath.Base(i.Install.Exe)
	stagedExe := filepath.Base(i.stagedExe(staged))
	type move struct{ from, to string }
	var done []move
	undo := func() {
		for k := len(done) - 1; k >= 0; k-- {
			_ = os.Rename(done[k].to, done[k].from)
		}
		_ = os.RemoveAll(previous)
	}
	mv := func(from, to string) error {
		if err := os.Rename(from, to); err != nil {
			return err
		}
		done = append(done, move{from, to})
		return nil
	}
	for _, e := range entries {
		name := e.Name()
		if name == filepath.Base(previous) {
			continue
		}
		target := name
		if name == stagedExe {
			// The running program keeps its name, whatever it was renamed to.
			target = exeName
		}
		cur := filepath.Join(i.Install.Root, target)
		if _, err := os.Lstat(cur); err == nil {
			if err := mv(cur, filepath.Join(previous, target)); err != nil {
				undo()
				return fmt.Errorf("update: moving %s aside: %w", target, err)
			}
		}
		if err := mv(filepath.Join(staged, name), cur); err != nil {
			undo()
			return fmt.Errorf("update: putting %s in place: %w", target, err)
		}
	}
	return nil
}

// Cleanup removes a staged download directory after a swap (what was moved
// out of it is gone; the artifact and any leftovers go too).
func Cleanup(dir string) { _ = os.RemoveAll(dir) }

// ErrRestart says the new program could not be started; the caller keeps
// running the old one, which is still whole on disk as the previous install.
var ErrRestart = errors.New("update: the new program could not be started")
