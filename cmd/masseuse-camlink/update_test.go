package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/provenance"
	"github.com/FemLed/masseuse-camlink/internal/update"
)

// acceptAll stands in for the Sigstore checks (tested in internal/provenance
// and internal/update); here the flow around them is what is tested.
type acceptAll struct{ tag string }

func (a *acceptAll) VerifyBlob(context.Context, []byte, []byte, provenance.BlobSigner) (*provenance.BlobResult, error) {
	return &provenance.BlobResult{Tag: a.tag, Identity: "release.yml@refs/tags/" + a.tag}, nil
}

func (a *acceptAll) VerifyBlobProvenance(_ context.Context, name string, _ []byte, _ []byte, _ provenance.BlobSigner) (*provenance.BlobResult, error) {
	return &provenance.BlobResult{Tag: a.tag, Identity: "generic generator for " + name}, nil
}

// tarOf is the release archive for this platform: a tar.gz, or on Windows
// the zip the release ships there.
func tarOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if runtime.GOOS == "windows" {
		zw := zip.NewWriter(&buf)
		for name, content := range files {
			h := &zip.FileHeader{Name: name, Method: zip.Deflate}
			h.SetMode(0o755)
			w, err := zw.CreateHeader(h)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write([]byte(content))
		}
		_ = zw.Close()
		return buf.Bytes()
	}
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(content))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// fakeRelease serves v0.11.0 for an archive install: the archive, its
// checksums (right or wrong) and a provenance file. hits counts artifact
// downloads.
func fakeRelease(t *testing.T, in update.Install, archive []byte, rightHash bool, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	name, _, prov := in.Artifact("v0.11.0")
	sum := sha256.Sum256(archive)
	hash := hex.EncodeToString(sum[:])
	if !rightHash {
		hash = strings.Repeat("0", 64)
	}
	files := map[string][]byte{
		"checksums.txt":               []byte(hash + "  " + name + "\n"),
		"checksums.txt.sigstore.json": []byte("{}"),
		prov:                          []byte("{}"),
		name:                          archive,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asset := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		b, ok := files[asset]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if asset == name {
			hits.Add(1)
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(b)))
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// testUpdater is an updater over a fake install (a file named like the
// program in a directory of its own) and a fake release.
func testUpdater(t *testing.T, srv *httptest.Server, in update.Install, out *strings.Builder) *updater {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	stateDir := t.TempDir()
	u := &updater{
		client: &update.Client{ReleasesURL: srv.URL, Verifier: &acceptAll{tag: "v0.11.0"}, Current: "v0.10.0", Install: in, StateDir: stateDir, Logger: log},
		inst: &update.Installer{Install: in, Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if len(args) == 1 && args[0] == "--version" {
				return []byte("v0.11.0 go1.27.1\n"), nil
			}
			return nil, errors.New("unexpected " + name)
		}},
		state:          update.LoadState(stateDir),
		log:            log,
		out:            func(format string, args ...any) { fmt.Fprintf(out, format, args...) },
		idle:           func() bool { return true },
		handoff:        func() {},
		args:           []string{"-console", "-state-dir", stateDir},
		env:            []string{"X=1"},
		now:            time.Now,
		firstAfter:     time.Millisecond,
		every:          time.Hour,
		idlePoll:       5 * time.Millisecond,
		handoffTimeout: time.Second,
		warnedAt:       map[string]time.Time{},
		restart:        func([]string, []string) error { return nil },
		exit:           func(int) {},
	}
	return u
}

func fakeInstall(t *testing.T) (update.Install, string) {
	t.Helper()
	root := t.TempDir()
	exe := filepath.Join(root, "masseuse-camlink")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	in := update.Detect(exe, runtime.GOOS, runtime.GOARCH)
	in.Layout = update.LayoutArchive
	return in, exe
}

func TestUpdaterWaitsForIdleThenSwapsAndHandsOver(t *testing.T) {
	in, exe := fakeInstall(t)
	bin := filepath.Base(exe)
	archive := tarOf(t, map[string]string{bin: "new binary", "units/camlink-unit-x": "helper"})
	var hits atomic.Int32
	srv := fakeRelease(t, in, archive, true, &hits)
	var out strings.Builder
	u := testUpdater(t, srv, in, &out)

	var polls atomic.Int32
	u.idle = func() bool { return polls.Add(1) > 3 } // busy for a moment, then idle
	var mu sync.Mutex
	var sequence []string
	u.handoff = func() { mu.Lock(); sequence = append(sequence, "handoff"); mu.Unlock() }
	var restartedWith []string
	u.restart = func(args, env []string) error {
		mu.Lock()
		sequence = append(sequence, "restart")
		mu.Unlock()
		restartedWith = args
		return nil
	}
	var code = -1
	u.exit = func(c int) { code = c }

	if !u.once(context.Background()) {
		t.Fatalf("once did not hand over:\n%s", out.String())
	}
	text := out.String()
	if !strings.Contains(text, "Update: Masseuse.ai v0.11.0 downloaded and verified; installing when the session ends.") {
		t.Fatalf("no waiting line:\n%s", text)
	}
	if !strings.Contains(text, "Updating to v0.11.0; back in a moment.") {
		t.Fatalf("no updating line:\n%s", text)
	}
	if !strings.Contains(text, "Handing over: camera off, unit released, restarting.") {
		t.Fatalf("no handing-over line:\n%s", text)
	}
	if strings.Join(sequence, ",") != "handoff,restart" {
		t.Fatalf("sequence %v", sequence)
	}
	if strings.Join(restartedWith, " ") != strings.Join(u.args, " ") || code != 0 {
		t.Fatalf("restarted with %v, exit %d", restartedWith, code)
	}
	// The new files are in place, the old ones aside, the state says so.
	if b, _ := os.ReadFile(exe); string(b) != "new binary" {
		t.Fatal("the new program is not in place")
	}
	if b, _ := os.ReadFile(filepath.Join(in.Root, "units", "camlink-unit-x")); string(b) != "helper" {
		t.Fatal("the helper did not come along")
	}
	if b, _ := os.ReadFile(filepath.Join(in.Root, ".previous", bin)); string(b) != "old binary" {
		t.Fatal("the old program was not kept aside")
	}
	st := update.LoadState(u.stateDir())
	if st.Installed != "v0.11.0" || st.From != "v0.10.0" || st.Previous != filepath.Join(in.Root, ".previous") || st.Staged != nil {
		t.Fatalf("state %+v", st)
	}
	if entries, _ := os.ReadDir(filepath.Join(u.stateDir(), "updates")); len(entries) != 0 {
		t.Fatalf("downloads left: %v", entries)
	}
	// The new program, starting: it says so once, and after its first
	// connection the previous version goes.
	var out2 strings.Builder
	next := testUpdater(t, srv, in, &out2)
	next.client.Current = "v0.11.0"
	next.state = st
	next.client.StateDir = u.stateDir()
	next.announce()
	if !strings.Contains(out2.String(), "Updated to v0.11.0.") {
		t.Fatalf("no updated line: %q", out2.String())
	}
	next.announce()
	if strings.Count(out2.String(), "Updated to") != 1 {
		t.Fatal("said twice")
	}
	next.proven()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(in.Root, ".previous")); errors.Is(err, os.ErrNotExist) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(in.Root, ".previous")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the previous version stayed after the new one proved itself")
	}
	if hits.Load() != 1 {
		t.Fatalf("the artifact was downloaded %d times", hits.Load())
	}
}

func TestUpdaterRefusesOnceADayAndLeavesTheProgramAlone(t *testing.T) {
	in, exe := fakeInstall(t)
	archive := tarOf(t, map[string]string{filepath.Base(exe): "new binary"})
	var hits atomic.Int32
	srv := fakeRelease(t, in, archive, false, &hits) // the served bytes do not match the signed list
	var out strings.Builder
	u := testUpdater(t, srv, in, &out)
	if u.once(context.Background()) {
		t.Fatal("a refused update handed over")
	}
	if !strings.Contains(out.String(), "Update v0.11.0 did not verify; staying on v0.10.0.") {
		t.Fatalf("no refusal line:\n%s", out.String())
	}
	if b, _ := os.ReadFile(exe); string(b) != "old binary" {
		t.Fatal("the program was touched")
	}
	st := update.LoadState(u.stateDir())
	if !st.FailedRecently("v0.11.0", time.Now()) {
		t.Fatal("the failure was not remembered")
	}
	// Not tried again within the day, and not said again.
	if u.once(context.Background()) {
		t.Fatal("handed over on the second pass")
	}
	if hits.Load() != 1 || strings.Count(out.String(), "did not verify") != 1 {
		t.Fatalf("downloads %d, lines %d", hits.Load(), strings.Count(out.String(), "did not verify"))
	}
}

func TestUpdaterPutsThePreviousBackWhenTheNewOneWillNotStart(t *testing.T) {
	in, exe := fakeInstall(t)
	archive := tarOf(t, map[string]string{filepath.Base(exe): "new binary"})
	var hits atomic.Int32
	srv := fakeRelease(t, in, archive, true, &hits)
	var out strings.Builder
	u := testUpdater(t, srv, in, &out)
	u.restart = func([]string, []string) error { return fmt.Errorf("%w: exec format error", update.ErrRestart) }
	code := -1
	u.exit = func(c int) { code = c }
	u.once(context.Background())
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old binary" {
		t.Fatal("the previous program was not put back")
	}
	if _, err := os.Stat(filepath.Join(in.Root, ".previous")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(".previous stayed")
	}
	if !strings.Contains(out.String(), "could not be started") || !strings.Contains(out.String(), "the previous one is back") {
		t.Fatalf("no word of it:\n%s", out.String())
	}
	st := update.LoadState(u.stateDir())
	if st.Installed != "" || st.Previous != "" || !st.FailedRecently("v0.11.0", time.Now()) {
		t.Fatalf("state %+v", st)
	}
}

func TestUpdaterEndsWithTheRelaunchCodeWhenTheShellStartsTheNewProgram(t *testing.T) {
	// On a Mac the program is not replaced by an exec of its own (the
	// runtime's wait before an exec on Darwin never ended, 2026-09-17): the
	// .command shell that started it runs it again on the relaunch code.
	in, exe := fakeInstall(t)
	archive := tarOf(t, map[string]string{filepath.Base(exe): "new binary"})
	var hits atomic.Int32
	srv := fakeRelease(t, in, archive, true, &hits)
	var out strings.Builder
	u := testUpdater(t, srv, in, &out)
	u.restart = func([]string, []string) error { return update.ErrRelaunch }
	code := -1
	u.exit = func(c int) { code = c }
	if !u.once(context.Background()) {
		t.Fatalf("once did not hand over:\n%s", out.String())
	}
	if code != update.RelaunchExitCode {
		t.Fatalf("exit %d, want %d", code, update.RelaunchExitCode)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new binary" {
		t.Fatal("the new program is not in place for the shell to start")
	}
	st := update.LoadState(u.stateDir())
	if st.Installed != "v0.11.0" || st.Previous == "" {
		t.Fatalf("state %+v", st)
	}
	if strings.Contains(out.String(), "could not be started") {
		t.Fatalf("read as a failure:\n%s", out.String())
	}

	// Started some other way, the new version opens on its own and this
	// program ends cleanly.
	var out2 strings.Builder
	in2, exe2 := fakeInstall(t)
	srv2 := fakeRelease(t, in2, tarOf(t, map[string]string{filepath.Base(exe2): "new binary"}), true, &hits)
	u2 := testUpdater(t, srv2, in2, &out2)
	u2.restart = func([]string, []string) error { return update.ErrStartedApart }
	code = -1
	u2.exit = func(c int) { code = c }
	if !u2.once(context.Background()) || code != 0 {
		t.Fatalf("exit %d:\n%s", code, out2.String())
	}
	if !strings.Contains(out2.String(), "Masseuse.ai v0.11.0 is starting in a window of its own; this one is done.") {
		t.Fatalf("no word of the new window:\n%s", out2.String())
	}
}

func TestUpdaterRestartsEvenWhenTheHandoffHangs(t *testing.T) {
	// A unit runtime that will not close, a camera that will not stop: the
	// handoff is bounded, and the restart goes ahead past the bound with a
	// warning. Before, any step could hold the update forever.
	in, exe := fakeInstall(t)
	archive := tarOf(t, map[string]string{filepath.Base(exe): "new binary"})
	var hits atomic.Int32
	srv := fakeRelease(t, in, archive, true, &hits)
	var out strings.Builder
	u := testUpdater(t, srv, in, &out)
	u.handoffTimeout = 50 * time.Millisecond
	var logged strings.Builder
	u.log = slog.New(slog.NewTextHandler(&logged, nil))
	release := make(chan struct{})
	u.handoff = func() { <-release }
	restarted := false
	u.restart = func([]string, []string) error { restarted = true; return update.ErrRelaunch }
	code := -1
	u.exit = func(c int) { code = c }
	started := time.Now()
	if !u.once(context.Background()) {
		t.Fatalf("once did not hand over:\n%s", out.String())
	}
	close(release)
	if !restarted || code != update.RelaunchExitCode {
		t.Fatalf("restarted %v, exit %d", restarted, code)
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Fatalf("the hung handoff held the restart for %v", took)
	}
	if !strings.Contains(logged.String(), "the handoff did not finish in time; restarting anyway") {
		t.Fatalf("no warning:\n%s", logged.String())
	}
}

func TestUpdaterIsQuietWhenUpToDateOrOffline(t *testing.T) {
	in, _ := fakeInstall(t)
	var hits atomic.Int32
	srv := fakeRelease(t, in, tarOf(t, map[string]string{"x": "y"}), true, &hits)
	var out strings.Builder
	u := testUpdater(t, srv, in, &out)
	u.client.Current = "v0.11.0" // the latest already
	if u.once(context.Background()) || out.Len() != 0 {
		t.Fatalf("up to date said something: %q", out.String())
	}
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	t.Cleanup(down.Close)
	u = testUpdater(t, down, in, &out)
	if u.once(context.Background()) || out.Len() != 0 {
		t.Fatalf("offline said something: %q", out.String())
	}
}

func TestManagerIsIdleWithoutTunnelsOrAnArmedUnit(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := &manager{log: log, tunnels: map[string]*active{}, estim: newEstimLink(t.TempDir(), log, func(string, ...any) {})}
	if !mgr.idle() {
		t.Fatal("nothing running, not idle")
	}
	mgr.tunnels["s1"] = &active{}
	if mgr.idle() {
		t.Fatal("a tunnel up, idle")
	}
	delete(mgr.tunnels, "s1")
	if !mgr.idle() {
		t.Fatal("tunnel gone, not idle")
	}
	// The first connection to the service, once.
	var proven int
	mgr.onFirstOnline = func() { proven++ }
	mgr.OnOnline(true)
	mgr.OnOnline(false)
	mgr.OnOnline(true)
	if proven != 1 {
		t.Fatalf("proven %d times", proven)
	}
}

func TestOnlyReleaseBuildsUpdate(t *testing.T) {
	for v, want := range map[string]bool{
		"v0.10.0": true, "v1.2.3": true,
		"(devel)": false, "v0.9.5-0.20260916163132-3eb75584a348+dirty": false, "v0.11.0-rc.1": false, "v0.10.0+dirty": false, "0.10.0": false, "": false,
	} {
		if got := isReleaseBuild(v); got != want {
			t.Fatalf("%q: %v", v, got)
		}
	}
}

func TestTheNewProgramDoesNotInheritThePretence(t *testing.T) {
	env := withoutEnv([]string{"A=1", "MASSEUSE_CAMLINK_UPDATE_AS=v0.9.4", "MASSEUSE_CAMLINK_UPDATE_AS_X=keep", "B=2"}, "MASSEUSE_CAMLINK_UPDATE_AS")
	if strings.Join(env, ",") != "A=1,MASSEUSE_CAMLINK_UPDATE_AS_X=keep,B=2" {
		t.Fatalf("env %v", env)
	}
}
