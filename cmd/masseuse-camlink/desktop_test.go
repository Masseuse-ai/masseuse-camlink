package main

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBundleExecutable(t *testing.T) {
	for _, tc := range []struct {
		exe  string
		want bool
	}{
		{"/Applications/Masseuse.ai.app/Contents/MacOS/Masseuse.ai", true},
		{"/Users/me/Downloads/Masseuse.ai.app/Contents/MacOS/Masseuse.ai", true},
		{"/Applications/masseuse-camlink.app/Contents/MacOS/masseuse-camlink", true}, // the bundle's name before 0.8.0
		{"/Users/me/Downloads/masseuse-camlink_0.7.0_darwin_arm64/masseuse-camlink", false},
		{"/Applications/Masseuse.ai.app/Contents/Helpers/ffmpeg", false},
		{"/opt/homebrew/bin/masseuse-camlink", false},
		{"masseuse-camlink", false},
	} {
		if got := bundleExecutable(tc.exe); got != tc.want {
			t.Errorf("bundleExecutable(%q) = %v, want %v", tc.exe, got, tc.want)
		}
	}
}

func TestAppName(t *testing.T) {
	// The name on the download, the .command file Terminal shows in its
	// title bar and the window title all agree.
	if appName != "Masseuse.ai" {
		t.Fatalf("appName = %q", appName)
	}
	if commandFile != "Masseuse.ai.command" {
		t.Fatalf("commandFile = %q", commandFile)
	}
}

func TestCommandScript(t *testing.T) {
	exe := "/Applications/Masseuse.ai.app/Contents/MacOS/Masseuse.ai"
	state := "/Users/o'brien/Library/Application Support/masseuse-camlink"
	script := commandScript(exe, state)
	if !strings.HasPrefix(script, "#!/bin/sh\n") {
		t.Fatalf("no sh shebang:\n%s", script)
	}
	want := `exec '/Applications/Masseuse.ai.app/Contents/MacOS/Masseuse.ai' -console -state-dir '/Users/o'\''brien/Library/Application Support/masseuse-camlink' "$@"` + "\n"
	if !strings.HasSuffix(script, want) {
		t.Fatalf("exec line wrong:\n%s\nwant suffix:\n%s", script, want)
	}
	if !strings.Contains(script, "printf '\\033]0;Masseuse.ai\\007'\n") {
		t.Fatalf("window title line wrong:\n%s", script)
	}
	if strings.Count(script, "\n") != 5 {
		t.Fatalf("script has %d lines, want 5:\n%s", strings.Count(script, "\n"), script)
	}
}

// TestCommandScriptRuns has /bin/sh, where there is one, run the script
// with a stand-in for the program: the quoting must survive a space and a
// quote in the paths, and the arguments must arrive in order.
func TestCommandScriptRuns(t *testing.T) {
	if runtime.GOOS == "windows" {
		// The .command file is for macOS's Terminal; a Git sh on Windows
		// would run it against Windows paths it was never written for.
		t.Skip("the .command file is not for Windows")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on this system")
	}
	dir := filepath.Join(t.TempDir(), "it's here")
	exe := filepath.Join(dir, "program")
	if err := writeExecutable(exe, "#!/bin/sh\nprintf '%s\\n' \"$@\"\n"); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, commandFile)
	if err := writeExecutable(script, commandScript(exe, dir)); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(sh, script, "extra").Output()
	if err != nil {
		t.Fatalf("sh %s: %v", script, err)
	}
	// The window title comes first, for the terminal; here it lands in the pipe.
	title := "\x1b]0;Masseuse.ai\a"
	if !strings.HasPrefix(string(out), title) {
		t.Fatalf("output does not start with the window title: %q", out)
	}
	got := strings.Split(strings.TrimSpace(strings.TrimPrefix(string(out), title)), "\n")
	want := []string{"-console", "-state-dir", dir, "extra"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("program received %q, want %q", got, want)
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"plain":        `'plain'`,
		"with space":   `'with space'`,
		"it's":         `'it'\''s'`,
		`"double"`:     `'"double"'`,
		"$HOME `x` \\": "'$HOME `x` \\'",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}
