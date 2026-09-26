package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// AppleTeamID is the Developer ID team the releases are signed with
// (VERIFY.md, "The macOS binaries"); a bundle signed by anyone else is
// refused before it is copied anywhere.
const AppleTeamID = "B8Z4RP3846"

// stage for a bundle: mount the disk image, check the application inside
// as Gatekeeper would (code signature whole and strict, notarized, signed
// by the team), copy it out with ditto (signatures and attributes intact),
// check the copy the same way, unmount. An archive install on a Mac is
// staged like anywhere else.
func (i *Installer) stage(ctx context.Context, s *Staged, dir string) (string, error) {
	if i.Install.Layout != LayoutBundle {
		return i.stageFiles(s, dir)
	}
	mnt := filepath.Join(dir, "mnt")
	if err := os.MkdirAll(mnt, 0o700); err != nil {
		return "", fmt.Errorf("update: %w", err)
	}
	if out, err := i.run(ctx, "/usr/bin/hdiutil", "attach", "-nobrowse", "-readonly", "-noautoopen", "-mountpoint", mnt, s.Path); err != nil {
		return "", fmt.Errorf("update: mounting %s: %w: %s", s.Name, err, strings.TrimSpace(string(out)))
	}
	defer func() {
		if out, err := i.run(ctx, "/usr/bin/hdiutil", "detach", "-force", mnt); err != nil {
			i.logger().Warn("update: unmounting the disk image", "err", err, "out", strings.TrimSpace(string(out)))
		}
	}()
	app, err := findApp(mnt)
	if err != nil {
		return "", err
	}
	if err := i.assess(ctx, app); err != nil {
		return "", err
	}
	staged := filepath.Join(dir, filepath.Base(i.Install.Root))
	if out, err := i.run(ctx, "/usr/bin/ditto", app, staged); err != nil {
		return "", fmt.Errorf("update: copying the application: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if err := i.assess(ctx, staged); err != nil {
		return "", fmt.Errorf("the copy: %w", err)
	}
	return staged, nil
}

// findApp is the one application bundle at the top of a mounted image.
func findApp(mnt string) (string, error) {
	entries, err := os.ReadDir(mnt)
	if err != nil {
		return "", fmt.Errorf("update: %w", err)
	}
	var apps []string
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), ".app") {
			apps = append(apps, filepath.Join(mnt, e.Name()))
		}
	}
	if len(apps) != 1 {
		return "", fmt.Errorf("update: the disk image holds %d applications, not one", len(apps))
	}
	return apps[0], nil
}

// assess is Gatekeeper's own view of an application: the signature verifies
// deep and strict, spctl accepts it for execution (notarized Developer ID),
// and the signer is the team.
func (i *Installer) assess(ctx context.Context, app string) error {
	if out, err := i.run(ctx, "/usr/bin/codesign", "--verify", "--deep", "--strict", "--verbose=2", app); err != nil {
		return fmt.Errorf("update: the application's signature does not verify: %w: %s", err, strings.TrimSpace(string(out)))
	}
	out, err := i.run(ctx, "/usr/sbin/spctl", "--assess", "--type", "execute", "-vv", app)
	if err != nil || !strings.Contains(string(out), "accepted") {
		return fmt.Errorf("update: Gatekeeper does not accept the application: %s", strings.TrimSpace(string(out)))
	}
	out, err = i.run(ctx, "/usr/bin/codesign", "-dvv", app)
	if err != nil {
		return fmt.Errorf("update: reading the application's signature: %w", err)
	}
	if !strings.Contains(string(out), "TeamIdentifier="+AppleTeamID) {
		return fmt.Errorf("update: the application is not signed by team %s", AppleTeamID)
	}
	if !strings.Contains(string(out), "flags=0x10000(runtime)") {
		return errors.New("update: the application is not signed with the hardened runtime")
	}
	return nil
}

// swapBundle renames the running bundle aside and the staged one into its
// place. The staged copy lives under the state directory; when that is
// another volume, it is copied beside the bundle first so the final step
// is a rename on one volume.
func (i *Installer) swapBundle(staged, previous string) error {
	root := i.Install.Root
	src := staged
	beside := filepath.Join(filepath.Dir(root), "."+filepath.Base(root)+".update")
	if !sameVolume(staged, filepath.Dir(root)) {
		_ = os.RemoveAll(beside)
		if out, err := i.run(context.Background(), "/usr/bin/ditto", staged, beside); err != nil {
			_ = os.RemoveAll(beside)
			return fmt.Errorf("update: copying the application beside the old one: %w: %s", err, strings.TrimSpace(string(out)))
		}
		src = beside
	}
	if err := os.Rename(root, previous); err != nil {
		_ = os.RemoveAll(beside)
		return fmt.Errorf("update: moving the application aside: %w", err)
	}
	if err := os.Rename(src, root); err != nil {
		_ = os.Rename(previous, root)
		_ = os.RemoveAll(beside)
		return fmt.Errorf("update: putting the application in place: %w", err)
	}
	return nil
}

func sameVolume(a, b string) bool {
	var sa, sb syscall.Stat_t
	if syscall.Stat(a, &sa) != nil || syscall.Stat(b, &sb) != nil {
		return false
	}
	return sa.Dev == sb.Dev
}

// Restart replaces this process with the new program at the install's
// path, with args (the arguments this run was started with). The pid and
// the terminal session are kept, so the Terminal window the bundle opened
// stays the program's.
func (i *Installer) Restart(args []string, env []string) error {
	argv := append([]string{i.Install.Exe}, args...)
	if err := syscall.Exec(i.Install.Exe, argv, env); err != nil {
		return fmt.Errorf("%w: %v", ErrRestart, err)
	}
	return nil
}
