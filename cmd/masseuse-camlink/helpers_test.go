package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/FemLed/masseuse-camlink/internal/estim"
	"github.com/FemLed/masseuse-camlink/internal/estim/helper"
)

func TestHelpersDirIsTheFlagOrTheBundlesPlace(t *testing.T) {
	if got := helpersDir("none", "/x/masseuse-camlink"); got != "" {
		t.Fatalf("none = %q", got)
	}
	if got := helpersDir(" /opt/units ", "/x/masseuse-camlink"); got != "/opt/units" {
		t.Fatalf("flag = %q", got)
	}
	// Paths are compared with the host's separators: filepath does the joining.
	app := filepath.Join("/", "Applications", "Masseuse.app", "Contents", "MacOS", "masseuse-camlink")
	if got := bundledHelpersDir(app, "darwin"); got != filepath.Join("/", "Applications", "Masseuse.app", "Contents", "Helpers", "units") {
		t.Fatalf("bundle = %q", got)
	}
	if got := bundledHelpersDir(filepath.Join("C:", "Masseuse", "masseuse-camlink.exe"), "windows"); filepath.Base(got) != "units" || filepath.Base(filepath.Dir(got)) != "Masseuse" {
		t.Fatalf("windows = %q", got)
	}
	if got := bundledHelpersDir(filepath.Join("/", "usr", "local", "bin", "masseuse-camlink"), "linux"); got != filepath.Join("/", "usr", "local", "bin", "units") {
		t.Fatalf("linux = %q", got)
	}
}

func TestHelperProgramsAreTheExecutablesWithThePrefix(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, mode os.FileMode) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("camlink-unit-b", 0o755)
	write("camlink-unit-a", 0o755)
	write("camlink-unit-notes.txt", 0o644) // not executable
	write("ffmpeg", 0o755)                 // not a helper
	write("camlink-unit-w.exe", 0o755)
	if err := os.Mkdir(filepath.Join(dir, "camlink-unit-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" { // the executable bit exists here
		got := helperPrograms(dir, "linux")
		want := []string{filepath.Join(dir, "camlink-unit-a"), filepath.Join(dir, "camlink-unit-b"), filepath.Join(dir, "camlink-unit-w.exe")}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("unix programs = %v", got)
		}
	}
	got := helperPrograms(dir, "windows")
	if len(got) != 1 || filepath.Base(got[0]) != "camlink-unit-w.exe" {
		t.Fatalf("windows programs = %v", got)
	}
	if got := helperPrograms(filepath.Join(dir, "missing"), "linux"); got != nil {
		t.Fatalf("missing dir = %v", got)
	}
	if got := helperPrograms("", "linux"); got != nil {
		t.Fatalf("no dir = %v", got)
	}
	if n := helperName(filepath.Join(dir, "camlink-unit-w.exe")); n != "w" {
		t.Fatalf("name = %q", n)
	}
	if n := helperName("/opt/units/camlink-unit-serial"); n != "serial" {
		t.Fatalf("name = %q", n)
	}
}

func TestHelpersLine(t *testing.T) {
	if got := helpersLine("", nil); !strings.Contains(got, "helpers off") {
		t.Fatalf("off = %q", got)
	}
	if got := helpersLine("/opt/units", nil); !strings.Contains(got, "no helpers in /opt/units") {
		t.Fatalf("none = %q", got)
	}
	found := []deviceFamily{{name: "alpha (helper)"}, {name: "beta (helper)"}}
	if got := helpersLine("/opt/units", found); got != "Unit drivers: Mastago (built in) + 2 helper(s): alpha, beta." {
		t.Fatalf("two = %q", got)
	}
}

// A helper written in shell: answers every request with a hello, which is
// all the registry asks at startup.
const shellHelper = `#!/bin/sh
echo "shelly here, state dir: $2" >&2
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  [ -n "$id" ] && printf '{"id":%s,"result":{"protocol":1,"name":"shelly","kinds":["tens"]}}\n' "$id"
done
`

func TestHelperFamiliesGreetEachProgramAndLeaveOutTheBroken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell helpers")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "camlink-unit-shelly"), []byte(shellHelper), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "camlink-unit-mute"), []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var logged lockedBuffer // the helper's stderr is relayed from another goroutine
	log := slog.New(slog.NewTextHandler(&logged, nil))
	stateDir := t.TempDir()
	fams := helperFamilies(context.Background(), dir, stateDir, log)
	if len(fams) != 1 || fams[0].name != "shelly (helper)" {
		t.Fatalf("families = %+v\n%s", fams, logged.String())
	}
	f := fams[0].finder(finderConfig{stateDir: stateDir, log: log})
	h, ok := f.(*helper.Host)
	if !ok {
		t.Fatalf("finder = %T", f)
	}
	t.Cleanup(func() { _ = h.Close() })
	if h.Name() != "shelly" || len(h.Kinds()) != 1 || h.Kinds()[0] != estim.KindTENS {
		t.Fatalf("host = %s %v", h.Name(), h.Kinds())
	}
	// The greeting's context has ended; the process has not. A later call
	// reaches the same process (the shell answers everything with hello,
	// which reads as an empty description).
	var out bytes.Buffer
	if err := h.Describe(context.Background(), &out); err != nil {
		t.Fatalf("the helper did not outlive the greeting: %v", err)
	}
	// The state directory is the helper's own, under the connector's.
	if want := filepath.Join(stateDir, "units", "shelly"); h.StateDir != want || !strings.Contains(logged.String(), want) {
		t.Fatalf("state dir = %q; log:\n%s", h.StateDir, logged.String())
	}
	if !strings.Contains(logged.String(), "camlink-unit-mute") || !strings.Contains(logged.String(), "left out") {
		t.Fatalf("the broken helper is not reported:\n%s", logged.String())
	}
	if !strings.Contains(logged.String(), "shelly here") {
		t.Fatalf("the helper's stderr is not relayed:\n%s", logged.String())
	}
	// A finder that is a Lister and a Selector, so the console picker and
	// the phone's device_select reach it like any family.
	if _, ok := f.(estim.Lister); !ok {
		t.Fatal("a helper family lists")
	}
	if _, ok := f.(estim.Selector); !ok {
		t.Fatal("a helper family selects")
	}
	if _, ok := f.(io.Closer); !ok {
		t.Fatal("a helper family closes with the finders")
	}
}

// lockedBuffer is a bytes.Buffer the relay goroutine may write while the
// test reads.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
