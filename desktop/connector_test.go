package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/payload"
	"github.com/FemLed/masseuse-camlink/internal/pesig/petest"
)

// The test binary doubles as a fake connector when FAKE_CONNECTOR is set:
// it speaks the -ipc protocol's shapes (docs/DESKTOP.md section 3), echoes
// every command as a notice, ends with the relaunch code on update_now,
// with 0 on quit or when standard input ends, and with FAKE_EXIT at once
// when that is set, a line on standard error first.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_CONNECTOR") == "" {
		os.Exit(m.Run())
	}
	fmt.Fprintln(os.Stderr, "fake: starting", strings.Join(os.Args[1:], " "))
	if code := os.Getenv("FAKE_EXIT"); code != "" {
		fmt.Fprintln(os.Stderr, "fake: something went wrong")
		var n int
		fmt.Sscan(code, &n)
		os.Exit(n)
	}
	out := json.NewEncoder(os.Stdout)
	_ = out.Encode(map[string]any{"type": "hello", "hello": map[string]any{"version": "v9.9.9", "identity": "abcd1234", "stateDir": "/tmp/x", "updates": "on", "awake": true, "drivers": "Unit drivers: Mastago (built in).", "phones": 1}})
	_ = out.Encode(map[string]any{"type": "source", "kind": "capture", "label": "Cam", "ready": true, "face": nil})
	_ = out.Encode(map[string]any{"type": "link", "state": "active"})
	_ = out.Encode(map[string]any{"type": "device", "descriptor": map[string]any{"kind": "mastago", "label": "Unit", "connected": true, "capabilities": map[string]any{"levelMax": 25}, "armed": map[string]any{"levelBound": 10}}})
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var cmd map[string]any
		if json.Unmarshal(sc.Bytes(), &cmd) != nil {
			continue
		}
		switch cmd["type"] {
		case "quit":
			os.Exit(0)
		case "update_now":
			_ = out.Encode(map[string]any{"type": "update", "state": "installing", "tag": "v10.0.0", "text": "Updating to v10.0.0; back in a moment."})
			os.Exit(relaunchExitCode)
		default:
			_ = out.Encode(map[string]any{"type": "notice", "level": "info", "text": "got " + fmt.Sprint(cmd["type"])})
		}
	}
	os.Exit(0)
}

func fakeConnector(t *testing.T, env ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-ipc", "-state-dir", t.TempDir())
	cmd.Env = append(os.Environ(), "FAKE_CONNECTOR=1")
	cmd.Env = append(cmd.Env, env...)
	return cmd
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestConnectorProcessSpeaksAndStops(t *testing.T) {
	var log lockedBuffer
	p, err := startConnector(fakeConnector(t), &log)
	if err != nil {
		t.Fatal(err)
	}
	first := <-p.lines
	var hello map[string]any
	if err := json.Unmarshal(first, &hello); err != nil || hello["type"] != "hello" {
		t.Fatalf("first line %s", first)
	}
	if err := p.send(map[string]any{"type": "list_devices"}); err != nil {
		t.Fatal(err)
	}
	var echoed bool
	for line := range p.lines {
		if strings.Contains(string(line), `"got list_devices"`) {
			echoed = true
			break
		}
	}
	if !echoed {
		t.Fatal("the command was not echoed")
	}
	p.stop(5 * time.Second)
	if code := p.wait(); code != 0 || !p.done() {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(log.String(), "fake: starting -ipc") {
		t.Fatalf("standard error did not reach the log: %q", log.String())
	}
}

func TestServiceReadsStateAndRelaunches(t *testing.T) {
	s := newConnectorService()
	s.log = slog.New(slog.DiscardHandler)
	p, err := startConnector(fakeConnector(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.proc = p
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { defer close(done); s.relay(p) }()
	waitFor(t, "the device event", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		_, ok := s.last["device"]
		return ok
	})
	s.mu.Lock()
	wired, active, armed := s.wired, s.linkActive, s.armed
	s.mu.Unlock()
	if !wired || !active || !armed {
		t.Fatalf("wired %v active %v armed %v", wired, active, armed)
	}
	if !s.Info().Wired {
		t.Fatal("Info says not wired")
	}
	snap := s.Snapshot()
	if len(snap) != 4 || snap[0]["type"] != "hello" || snap[1]["type"] != "source" || snap[2]["type"] != "link" || snap[3]["type"] != "device" {
		t.Fatalf("snapshot %v", snap)
	}
	// A session is on: quitting is refused until confirmed (with no
	// window there is no dialog; the refusal is the point).
	if s.shouldQuit() {
		t.Fatal("quit allowed during a session")
	}
	if err := s.UpdateNow(); err != nil {
		t.Fatal(err)
	}
	<-done
	if !s.shouldRelaunch() || !s.forceQuit.Load() {
		t.Fatal("the relaunch code was not read")
	}
	if err := s.ListDevices(); err == nil {
		t.Fatal("a command reached a connector that ended")
	}
}

func TestServiceReportsAConnectorThatStopped(t *testing.T) {
	s := newConnectorService()
	s.log = slog.New(slog.DiscardHandler)
	p, err := startConnector(fakeConnector(t, "FAKE_EXIT=3"), nil)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.proc = p
	s.mu.Unlock()
	s.relay(p)
	blocked := s.Snapshot()
	if len(blocked) != 1 || blocked[0]["type"] != "blocked" || blocked[0]["kind"] != "connector-stopped" {
		t.Fatalf("snapshot %v", blocked)
	}
	detail, _ := blocked[0]["detail"].(string)
	if !strings.Contains(detail, "exit code 3") || !strings.Contains(detail, "something went wrong") {
		t.Fatalf("detail %q", detail)
	}
	if s.shouldRelaunch() {
		t.Fatal("a crash read as a relaunch")
	}
	if !s.shouldQuit() {
		t.Fatal("quit refused with no session")
	}
}

func TestMediaStanding(t *testing.T) {
	// The words are AVAuthorizationStatus's by value; anything else reads
	// as not determined, the standing that is harmless to ask about.
	for status, want := range map[int]string{0: mediaNotDetermined, 1: mediaRestricted, 2: mediaDenied, 3: mediaAuthorized, 7: mediaNotDetermined} {
		if got := mediaWord(status); got != want {
			t.Errorf("mediaWord(%d) = %q, want %q", status, got, want)
		}
	}
	// Whatever this system says of the test binary, it says it in those
	// words (a Mac answers for the process responsible for the test; the
	// other systems always say authorized).
	words := map[string]bool{mediaNotDetermined: true, mediaRestricted: true, mediaDenied: true, mediaAuthorized: true}
	camera, mic := mediaAuthStatus()
	if !words[camera] || !words[mic] {
		t.Fatalf("mediaAuthStatus() = %q, %q", camera, mic)
	}
	if mediaWatched != (runtime.GOOS == "darwin") {
		t.Fatalf("mediaWatched %v on %s", mediaWatched, runtime.GOOS)
	}
	// The first word always goes to the page and into the snapshot, the
	// same word again does not, a change does; the snapshot keeps the last.
	s := newConnectorService()
	s.log = slog.New(slog.DiscardHandler)
	if !s.reportMedia(mediaNotDetermined, mediaNotDetermined) {
		t.Fatal("the first word did not go")
	}
	if s.reportMedia(mediaNotDetermined, mediaNotDetermined) {
		t.Fatal("the same word went twice")
	}
	if !s.reportMedia(mediaAuthorized, mediaDenied) {
		t.Fatal("a change did not go")
	}
	snap := s.Snapshot()
	if len(snap) != 1 || snap[0]["type"] != "media" || snap[0]["camera"] != mediaAuthorized || snap[0]["mic"] != mediaDenied {
		t.Fatalf("snapshot %v", snap)
	}
	// The watcher ends with its context (the application's, at shutdown).
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.watchMedia(ctx) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher outlived its context")
	}
}

func TestLocateConnectorHonoursTheEnvironment(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "connector")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MASSEUSE_CAMLINK_BIN", bin)
	got, root, err := locateConnector(t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil || got != bin || root != "" {
		t.Fatalf("%s %q %v", got, root, err)
	}
	t.Setenv("MASSEUSE_CAMLINK_BIN", filepath.Join(t.TempDir(), "gone"))
	if _, _, err := locateConnector(t.TempDir(), slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("a missing connector was found")
	}
}

func TestLocateBesideTheShell(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	// The Linux desktop archive: the connector in the shell's directory,
	// which is what an update replaces.
	dir := t.TempDir()
	shell := filepath.Join(dir, "Masseuse")
	connector := filepath.Join(dir, "masseuse-camlink")
	for _, f := range []string{shell, connector} {
		if err := os.WriteFile(f, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bin, root, err := locateBeside(shell, t.TempDir(), "linux", log)
	if err != nil || bin != connector || root != dir {
		t.Fatalf("linux: %s %s %v", bin, root, err)
	}
	// The macOS bundle: the root is the .app.
	app := filepath.Join(t.TempDir(), "Masseuse.app")
	macos := filepath.Join(app, "Contents", "MacOS")
	if err := os.MkdirAll(macos, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(macos, "masseuse-camlink"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin, root, err = locateBeside(filepath.Join(macos, "Masseuse"), t.TempDir(), "darwin", log)
	if err != nil || bin != filepath.Join(macos, "masseuse-camlink") || root != app {
		t.Fatalf("darwin: %s %s %v", bin, root, err)
	}
	// Nothing beside the shell: said, with the way out.
	if _, _, err := locateBeside(filepath.Join(t.TempDir(), "Masseuse"), t.TempDir(), "linux", log); err == nil || !strings.Contains(err.Error(), "MASSEUSE_CAMLINK_BIN") {
		t.Fatalf("missing connector: %v", err)
	}
	// The Windows package: the shell carries the connector in its payload,
	// unpacked under the state directory; the root is the shell's own file.
	packed, err := payload.Append(petest.Image([]byte("window")), "0.0.0", []payload.Entry{
		{Name: "masseuse-camlink.exe", Data: petest.Image([]byte("connector"))},
		{Name: "ffmpeg.exe", Data: petest.Image([]byte("ffmpeg"))},
	})
	if err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(t.TempDir(), "Masseuse.exe")
	if err := os.WriteFile(pkg, packed, 0o755); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	bin, root, err = locateBeside(pkg, stateDir, "windows", log)
	if err != nil || root != pkg || filepath.Base(bin) != "masseuse-camlink.exe" || !strings.HasPrefix(bin, filepath.Join(stateDir, "bin")) {
		t.Fatalf("windows: %s %s %v", bin, root, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(bin), "ffmpeg.exe")); err != nil {
		t.Fatalf("ffmpeg.exe is not beside the connector: %v", err)
	}
	// A package that carries no connector is refused.
	bare, _ := payload.Append(petest.Image([]byte("window")), "0.0.0", []payload.Entry{{Name: "ffmpeg.exe", Data: petest.Image([]byte("ffmpeg"))}})
	pkg2 := filepath.Join(t.TempDir(), "Masseuse.exe")
	_ = os.WriteFile(pkg2, bare, 0o755)
	if _, _, err := locateBeside(pkg2, t.TempDir(), "windows", log); err == nil {
		t.Fatal("a package without a connector was taken")
	}
}

func TestHelpers(t *testing.T) {
	if got := passthroughArgs([]string{"--version", "-service", "https://x", "-version"}); strings.Join(got, " ") != "-service https://x" {
		t.Fatalf("passthrough %v", got)
	}
	tb := &tailBuffer{max: 2}
	fmt.Fprint(tb, "one\ntwo\nthr")
	fmt.Fprint(tb, "ee\nfour\n")
	if tb.String() != "three\nfour" {
		t.Fatalf("tail %q", tb.String())
	}
	if psQuote("it's") != "'it''s'" {
		t.Fatal(psQuote("it's"))
	}
	// filepath is the host's: the bundle's path is spelled its way.
	app := filepath.FromSlash("/Applications/Masseuse.app")
	if bundleRoot(filepath.Join(app, "Contents", "MacOS", "Masseuse")) != app || bundleRoot(filepath.FromSlash("/usr/local/bin/Masseuse")) != "" {
		t.Fatal("bundleRoot")
	}
	dir := t.TempDir()
	lf, err := openLogFile(filepath.Join(dir, "desktop.log"))
	if err != nil {
		t.Fatal(err)
	}
	lf.size = logMaxBytes - 1 // as if full
	fmt.Fprintln(lf, "a new line")
	lf.Close()
	if _, err := os.Stat(filepath.Join(dir, "desktop.log.1")); err != nil {
		t.Fatalf("the full log was not moved aside: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "desktop.log")); string(b) != "a new line\n" {
		t.Fatalf("new log %q", b)
	}
}

// lockedBuffer is a strings.Builder safe for the process goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}
