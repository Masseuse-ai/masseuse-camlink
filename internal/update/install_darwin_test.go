package update

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeMac stands in for hdiutil, codesign, spctl and ditto: the image
// "mounts" as a directory holding one application, whose signature reads
// as the team's unless told otherwise.
type fakeMac struct {
	team     string
	verdict  string
	runtime  bool
	version  string
	commands []string
}

func (m *fakeMac) run(_ context.Context, name string, args ...string) ([]byte, error) {
	m.commands = append(m.commands, filepath.Base(name)+" "+strings.Join(args, " "))
	switch filepath.Base(name) {
	case "hdiutil":
		if args[0] == "attach" {
			mnt := args[len(args)-2]
			app := filepath.Join(mnt, "Masseuse.app", "Contents", "MacOS")
			if err := os.MkdirAll(app, 0o755); err != nil {
				return nil, err
			}
			_ = os.WriteFile(filepath.Join(app, "Masseuse"), []byte("new app"), 0o755)
			_ = os.WriteFile(filepath.Join(mnt, "Masseuse.app", "Contents", "Info.plist"), []byte("<plist/>"), 0o644)
			_ = os.Symlink("/Applications", filepath.Join(mnt, "Applications"))
		}
		return nil, nil
	case "codesign":
		if args[0] == "-dvv" {
			out := "Identifier=ai.masseuse.camlink\nTeamIdentifier=" + m.team + "\n"
			if m.runtime {
				out += "CodeDirectory v=20500 flags=0x10000(runtime)\n"
			}
			return []byte(out), nil
		}
		return []byte("valid on disk"), nil
	case "spctl":
		if m.verdict != "accepted" {
			return []byte(m.verdict), errors.New("exit 3")
		}
		return []byte("accepted\nsource=Notarized Developer ID"), nil
	case "ditto":
		return exec.Command("/usr/bin/ditto", args...).CombinedOutput()
	case "Masseuse":
		return []byte(m.version + " go1.27.1\n"), nil
	case "open":
		return nil, nil
	}
	return nil, errors.New("unexpected " + name)
}

func bundleInstall(t *testing.T) Install {
	t.Helper()
	root := filepath.Join(t.TempDir(), "Masseuse.app")
	if err := os.MkdirAll(filepath.Join(root, "Contents", "MacOS"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(root, "Contents", "MacOS", "Masseuse"), []byte("old app"), 0o755)
	return Detect(filepath.Join(root, "Contents", "MacOS", "Masseuse"), "darwin", "arm64")
}

func TestStageAndSwapABundle(t *testing.T) {
	ctx := context.Background()
	in := bundleInstall(t)
	if in.Layout != LayoutBundle {
		t.Fatalf("layout %s", in.Layout)
	}
	mac := &fakeMac{team: AppleTeamID, verdict: "accepted", runtime: true, version: "v0.11.0"}
	// The previous bundle goes under the state directory (Aside), on the
	// same volume here, not beside the new one in Applications.
	aside := filepath.Join(t.TempDir(), "state", "previous")
	inst := &Installer{Install: in, Run: mac.run, Aside: aside}
	staged := &Staged{Tag: "v0.11.0", Name: "Masseuse.ai-0.11.0.dmg", Path: filepath.Join(t.TempDir(), "Masseuse.ai-0.11.0.dmg")}
	_ = os.WriteFile(staged.Path, []byte("dmg"), 0o600)
	dir := filepath.Join(t.TempDir(), "staged")
	root, err := inst.Stage(ctx, staged, dir)
	if err != nil {
		t.Fatal(err)
	}
	if root != filepath.Join(dir, "Masseuse.app") {
		t.Fatalf("staged root %s", root)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "Contents", "MacOS", "Masseuse")); string(b) != "new app" {
		t.Fatal("the application was not copied out of the image")
	}
	joined := strings.Join(mac.commands, "\n")
	for _, want := range []string{"hdiutil attach -nobrowse -readonly", "codesign --verify --deep --strict", "spctl --assess --type execute", "codesign -dvv", "ditto ", "hdiutil detach", "Masseuse --version"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
	// Both the mounted application and the copy were assessed.
	if strings.Count(joined, "spctl --assess") != 2 {
		t.Fatalf("spctl ran %d times", strings.Count(joined, "spctl --assess"))
	}
	if _, err := os.Lstat(filepath.Join(dir, "mnt", "Applications")); err == nil {
		// The fake never unmounts; the real one does. Nothing else should be
		// taken from the image than the application.
		if _, err := os.Stat(filepath.Join(root, "Applications")); err == nil {
			t.Fatal("the Applications shortcut came along")
		}
	}

	previous, err := inst.Swap(root)
	if err != nil {
		t.Fatal(err)
	}
	if previous != filepath.Join(aside, "Masseuse.app") {
		t.Fatalf("previous at %s", previous)
	}
	if entries, _ := os.ReadDir(filepath.Dir(in.Root)); len(entries) != 1 || entries[0].Name() != "Masseuse.app" {
		t.Fatalf("the Applications folder shows %v, want the one application", entries)
	}
	if b, _ := os.ReadFile(in.Exe); string(b) != "new app" {
		t.Fatal("the new application is not in place")
	}
	if b, _ := os.ReadFile(filepath.Join(previous, "Contents", "MacOS", "Masseuse")); string(b) != "old app" {
		t.Fatal("the old application was not kept aside")
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the staged copy stayed")
	}
}

func TestStageRefusesAnApplicationGatekeeperOrTheTeamWouldNot(t *testing.T) {
	ctx := context.Background()
	staged := &Staged{Tag: "v0.11.0", Name: "Masseuse.ai-0.11.0.dmg", Path: filepath.Join(t.TempDir(), "x.dmg")}
	_ = os.WriteFile(staged.Path, []byte("dmg"), 0o600)
	for _, c := range []struct {
		name string
		mac  *fakeMac
		want string
	}{
		{"another team", &fakeMac{team: "ZZZZZZZZZZ", verdict: "accepted", runtime: true, version: "v0.11.0"}, "not signed by team"},
		{"rejected by Gatekeeper", &fakeMac{team: AppleTeamID, verdict: "rejected", runtime: true, version: "v0.11.0"}, "Gatekeeper"},
		{"no hardened runtime", &fakeMac{team: AppleTeamID, verdict: "accepted", runtime: false, version: "v0.11.0"}, "hardened runtime"},
		{"another version inside", &fakeMac{team: AppleTeamID, verdict: "accepted", runtime: true, version: "v0.10.0"}, "says it is"},
	} {
		in := bundleInstall(t)
		inst := &Installer{Install: in, Run: c.mac.run}
		dir := filepath.Join(t.TempDir(), "staged")
		_, err := inst.Stage(ctx, staged, dir)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: %v", c.name, err)
		}
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s: the staging directory was left", c.name)
		}
		if b, _ := os.ReadFile(in.Exe); string(b) != "old app" {
			t.Fatalf("%s: the running application was touched", c.name)
		}
	}
}

func TestPreviousNameHidesTheBundleBesideWhenThereIsNoRoomAside(t *testing.T) {
	in := bundleInstall(t)
	// No Aside: beside the new one, hidden from the Finder.
	if got := (&Installer{Install: in}).PreviousName(); got != filepath.Join(filepath.Dir(in.Root), ".Masseuse.previous.app") {
		t.Fatalf("previous name %s", got)
	}
	// An Aside that cannot be made: the same.
	blocked := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(blocked, nil, 0o600)
	if got := (&Installer{Install: in, Aside: filepath.Join(blocked, "previous")}).PreviousName(); got != filepath.Join(filepath.Dir(in.Root), ".Masseuse.previous.app") {
		t.Fatalf("previous name %s", got)
	}
	// An Aside on the volume: under it, by the bundle's own name.
	aside := filepath.Join(t.TempDir(), "previous")
	if got := (&Installer{Install: in, Aside: aside}).PreviousName(); got != filepath.Join(aside, "Masseuse.app") {
		t.Fatalf("previous name %s", got)
	}
}

func TestRestartNeverExecsTheProgramItself(t *testing.T) {
	// Started by the .command shell: the relaunch code is the shell's cue.
	in := bundleInstall(t)
	t.Setenv(RelaunchEnv, "1")
	if err := (&Installer{Install: in}).Restart(nil, nil); !errors.Is(err, ErrRelaunch) {
		t.Fatalf("Restart = %v, want ErrRelaunch", err)
	}
	// Started otherwise, a bundle is opened through LaunchServices and this
	// program ends.
	t.Setenv(RelaunchEnv, "")
	mac := &fakeMac{}
	err := (&Installer{Install: in, Run: mac.run}).Restart(nil, nil)
	if !errors.Is(err, ErrStartedApart) {
		t.Fatalf("Restart = %v, want ErrStartedApart", err)
	}
	if joined := strings.Join(mac.commands, "\n"); !strings.Contains(joined, "open -a "+in.Root) {
		t.Fatalf("the bundle was not opened:\n%s", joined)
	}
}
