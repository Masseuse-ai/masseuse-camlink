package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/estim"
	"github.com/FemLed/masseuse-camlink/internal/serve"
)

// lines decodes every JSON line written so far.
func ipcLines(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("not JSON: %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestIPCReporterHelloComesFirst(t *testing.T) {
	var out lockedBuffer
	r := newIPCReporter(&out, slog.New(slog.DiscardHandler))
	// Startup facts, in main.go's order, with events that arrive before
	// startup is over (the unit runtime starts early).
	r.Banner("v0.13.0", "ab12cd34", "/state")
	r.Update(updateOff, "", "Updates are off (-no-update).")
	r.Units([]estim.Unit{{ID: "u1", Kind: "mastago", Label: "Mastago TENS G-12AB"}}, nil, false)
	r.Source(sourceReport{Kind: "capture", Label: "FaceTime HD Camera + MacBook Pro Microphone", Ready: true, Shape: "1280x720 30 fps, h264_videotoolbox", Camera: "FaceTime HD Camera", Mic: "MacBook Pro Microphone",
		Share: &shareReport{Ready: true, Address: "rtsp://127.0.0.1:7446/phone-abc"}})
	r.Drivers("Unit drivers: Mastago (built in); no helpers in /x/units.")
	r.PairedCount(2)
	r.Awake(awakeHeld, "")
	if got := []byte(out.String()); len(got) != 0 {
		t.Fatalf("said something before Ready: %s", got)
	}
	r.Ready()
	lines := ipcLines(t, []byte(out.String()))
	if len(lines) != 4 {
		t.Fatalf("got %d lines, want hello, source, update, units:\n%s", len(lines), []byte(out.String()))
	}
	if lines[0]["type"] != "hello" {
		t.Fatalf("first line %v", lines[0])
	}
	hello := lines[0]["hello"].(map[string]any)
	if hello["version"] != "v0.13.0" || hello["identity"] != "ab12cd34" || hello["stateDir"] != "/state" ||
		hello["updates"] != "off" || hello["updatesNote"] != "Updates are off (-no-update)." ||
		hello["awake"] != true || hello["phones"] != float64(2) || !strings.HasPrefix(hello["drivers"].(string), "Unit drivers:") {
		t.Fatalf("hello %v", hello)
	}
	if lines[1]["type"] != "source" || lines[1]["camera"] != "FaceTime HD Camera" || lines[1]["face"] != nil {
		t.Fatalf("source %v", lines[1])
	}
	if share := lines[1]["share"].(map[string]any); share["ready"] != true || share["address"] != "rtsp://127.0.0.1:7446/phone-abc" || share["receiving"] != false {
		t.Fatalf("share %v", share)
	}
	if lines[2]["type"] != "update" || lines[2]["state"] != "off" {
		t.Fatalf("update %v", lines[2])
	}
	if lines[3]["type"] != "units" || lines[3]["scanning"] != false {
		t.Fatalf("units %v", lines[3])
	}
	// After Ready everything goes out at once, and the phone's picture
	// arriving re-says the source with receiving set.
	r.Share(true, "rtsp://127.0.0.1:7446/phone-abc")
	r.Link(linkReset, "no longer the session's camera")
	r.Code("123456", time.Date(2026, 9, 19, 20, 0, 0, 0, time.UTC))
	lines = ipcLines(t, []byte(out.String()))
	if len(lines) != 7 {
		t.Fatalf("got %d lines:\n%s", len(lines), []byte(out.String()))
	}
	if share := lines[4]["share"].(map[string]any); lines[4]["type"] != "source" || share["receiving"] != true {
		t.Fatalf("share arriving: %v", lines[4])
	}
	if lines[5]["type"] != "link" || lines[5]["state"] != "closed" || lines[5]["reason"] != "no longer the session's camera" {
		t.Fatalf("link %v", lines[5])
	}
	if lines[6]["type"] != "code" || lines[6]["code"] != "123456" || lines[6]["expiresAt"] != "2026-09-19T20:00:00Z" {
		t.Fatalf("code %v", lines[6])
	}
}

func TestIPCReporterBlockedGoesOutAtOnce(t *testing.T) {
	var out lockedBuffer
	r := newIPCReporter(&out, slog.New(slog.DiscardHandler))
	r.Banner("v0.13.0", "ab12cd34", "/state")
	r.Blocked(blockedAlreadyRunning, errAlreadyRunning.Error())
	lines := ipcLines(t, []byte(out.String()))
	if len(lines) != 1 || lines[0]["type"] != "blocked" || lines[0]["kind"] != "already-running" {
		t.Fatalf("lines %v", lines)
	}
}

func TestIPCReporterDeviceCarriesStatusAndArm(t *testing.T) {
	var out lockedBuffer
	r := newIPCReporter(&out, slog.New(slog.DiscardHandler))
	r.Ready()
	level, battery := 7, 80
	d := estim.Descriptor{Kind: "mastago", Label: "Mastago TENS G-12AB", ID: "u1", Connected: true,
		Capabilities: estim.Capabilities{LevelMax: 25, Channels: []string{"A", "B"}, Modes: []int{1, 2}}}
	r.DeviceState(d, estim.Status{LevelA: &level, BatteryPercent: &battery}, true, 12)
	lines := ipcLines(t, []byte(out.String()))
	desc := lines[1]["descriptor"].(map[string]any)
	if desc["label"] != "Mastago TENS G-12AB" || desc["connected"] != true {
		t.Fatalf("descriptor %v", desc)
	}
	if st := desc["status"].(map[string]any); st["levelA"] != float64(7) || st["batteryPercent"] != float64(80) {
		t.Fatalf("status %v", st)
	}
	if armed := desc["armed"].(map[string]any); armed["levelBound"] != float64(12) {
		t.Fatalf("armed %v", armed)
	}
	if caps := desc["capabilities"].(map[string]any); caps["levelMax"] != float64(25) {
		t.Fatalf("capabilities %v", caps)
	}
}

func TestSourceChoiceFlags(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		choice sourceChoice
		want   sourceFlags
	}{
		{sourceChoice{Camera: "OBS Virtual Camera", Mic: "none"}, sourceFlags{camera: "OBS Virtual Camera", mic: "none"}},
		{sourceChoice{FaceCamera: "phone"}, sourceFlags{faceCamera: "none"}},
		{sourceChoice{FaceCamera: "OBS Virtual Camera"}, sourceFlags{faceCamera: "OBS Virtual Camera"}},
		{sourceChoice{Share: &on}, sourceFlags{sharePhone: "on"}},
		{sourceChoice{Share: &off, SharePort: 7500}, sourceFlags{sharePhone: "off", sharePort: 7500}},
		{sourceChoice{URL: "rtsps://cam/live", Fingerprint: "ab"}, sourceFlags{cameraURL: "rtsps://cam/live", cameraFingerprint: "ab"}},
	} {
		if got := tc.choice.flags(); got != tc.want {
			t.Errorf("flags(%+v) = %+v, want %+v", tc.choice, got, tc.want)
		}
	}
	if (sourceChoice{}).flags().any() {
		t.Fatal("an empty choice speaks of something")
	}
}

// testManager is a manager with a camera sink and no service, enough for
// the source commands.
func testManager(t *testing.T, ui reporter) *manager {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	stateDir := t.TempDir()
	sink, err := serve.New(serve.Config{StateDir: stateDir, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sink.Close)
	cam := &camControl{sink: sink, log: log, ui: ui, offer: &offer{kind: "capture", label: "This computer's camera", note: "no camera", save: sourceConfig{Kind: "capture"}}}
	m := &manager{log: log, ui: ui, cam: cam, stateDir: stateDir, sink: sink, cfg: sourceConfig{Kind: "capture"}, tunnels: map[string]*active{}}
	cam.onOff = func() { go m.applyPending() }
	return m
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func TestApplySourceStartsAndStopsThePhonePicture(t *testing.T) {
	var out lockedBuffer
	ui := newIPCReporter(&out, slog.New(slog.DiscardHandler))
	ui.Ready()
	m := testManager(t, ui)
	port := freePort(t)
	on := true
	if err := m.applySource(context.Background(), sourceChoice{Share: &on, SharePort: port}); err != nil {
		t.Fatal(err)
	}
	if m.shared == nil || m.shared.Port() != port {
		t.Fatalf("no share server on %d", port)
	}
	src, ok := m.CurrentSource()
	if !ok || !src.Share.Wanted {
		t.Fatalf("source report %+v %v", src, ok)
	}
	saved := loadSourceConfig(m.stateDir)
	if !saved.SharePhone || saved.SharePort != port || saved.ShareSecret == "" {
		t.Fatalf("source.json %+v", saved)
	}
	lines := ipcLines(t, []byte(out.String()))
	last := lines[len(lines)-1]
	share, _ := last["share"].(map[string]any)
	if last["type"] != "source" || share == nil || share["ready"] != true || !strings.Contains(share["address"].(string), fmt.Sprint(port)) {
		t.Fatalf("source event %v", last)
	}
	// The same choice again keeps the server; off closes it.
	before := m.shared
	if err := m.applySource(context.Background(), sourceChoice{Share: &on}); err != nil {
		t.Fatal(err)
	}
	if m.shared != before {
		t.Fatal("the server was replaced for nothing")
	}
	off := false
	if err := m.applySource(context.Background(), sourceChoice{Share: &off}); err != nil {
		t.Fatal(err)
	}
	if m.shared != nil {
		t.Fatal("the server is still there")
	}
	if saved := loadSourceConfig(m.stateDir); saved.SharePhone || saved.ShareSecret == "" {
		t.Fatalf("source.json after off: %+v (the secret is kept)", saved)
	}
	lines = ipcLines(t, []byte(out.String()))
	if last := lines[len(lines)-1]; last["type"] != "source" || last["share"] != nil {
		t.Fatalf("source event after off: %v", last)
	}
	if err := m.applySource(context.Background(), sourceChoice{}); err == nil {
		t.Fatal("an empty choice was taken")
	}
}

func TestApplySourceWaitsWhileTheCameraIsOn(t *testing.T) {
	var out lockedBuffer
	ui := newIPCReporter(&out, slog.New(slog.DiscardHandler))
	ui.Ready()
	m := testManager(t, ui)
	// A camera that is on: the offer's start and stop are no-ops.
	started := atomic.Int32{}
	m.cam.offer = &offer{kind: "capture", label: "Cam", ready: true, start: func() { started.Add(1) }, stop: func() {}, save: sourceConfig{Kind: "capture", Camera: "Cam"}}
	if _, err := m.cam.dialLocal(); err != nil {
		t.Fatal(err)
	}
	if on, _ := m.cam.busy(); !on {
		t.Fatal("camera not on")
	}
	// The front-facing camera is off, so putting the phone's own camera
	// back applies at once, camera or not.
	if err := m.applySource(context.Background(), sourceChoice{FaceCamera: "phone"}); err != nil {
		t.Fatal(err)
	}
	lines := ipcLines(t, []byte(out.String()))
	if last := lines[len(lines)-1]; m.pending != nil || last["type"] != "source" || last["face"] != nil {
		t.Fatalf("the face change did not apply: %v", last)
	}
	// A change of the camera itself waits while a session reads it; it is
	// applied when the camera goes off (and then says how it went: a
	// source report, or why the new camera could not be prepared).
	if err := m.applySource(context.Background(), sourceChoice{Camera: "Some Other Camera", Mic: "none"}); err != nil {
		t.Fatal(err)
	}
	if m.pending == nil {
		t.Fatal("the change did not wait")
	}
	lines = ipcLines(t, []byte(out.String()))
	if last := lines[len(lines)-1]; last["type"] != "notice" || !strings.Contains(last["text"].(string), "applied when it lets go") {
		t.Fatalf("no word of waiting: %v", last)
	}
	m.cam.off(false)
	waitUntil(t, "the pending change", func() bool {
		m.srcMu.Lock()
		defer m.srcMu.Unlock()
		return m.pending == nil
	})
	waitUntil(t, "a word on the change", func() bool {
		lines := ipcLines(t, []byte(out.String()))
		last := lines[len(lines)-1]
		return last["type"] == "source" || (last["type"] == "notice" && last["level"] == "error")
	})
	if on, _ := m.cam.busy(); on {
		t.Fatal("camera still on")
	}
}

func TestIPCCommandsRunStopsAtEOF(t *testing.T) {
	var out lockedBuffer
	ui := newIPCReporter(&out, slog.New(slog.DiscardHandler))
	ui.Ready()
	stopped := make(chan struct{})
	var once sync.Once
	c := &ipcCommands{ui: ui, mgr: testManager(t, ui), ffmpeg: filepath.Join(t.TempDir(), "no-ffmpeg"), log: slog.New(slog.DiscardHandler),
		stop: func() { once.Do(func() { close(stopped) }) }}
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() { defer close(done); c.run(context.Background(), pr) }()
	fmt.Fprintln(pw, `{"type":"list_devices"}`)
	fmt.Fprintln(pw, `not json`)
	fmt.Fprintln(pw, `{"type":"whatever"}`)
	waitUntil(t, "the devices answer", func() bool {
		for _, l := range ipcLines(t, []byte(out.String())) {
			if l["type"] == "devices" {
				return true
			}
		}
		return false
	})
	pw.Close()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("standard input ended and the program was not stopped")
	}
	<-done
	var devices, notice map[string]any
	for _, l := range ipcLines(t, []byte(out.String())) {
		switch l["type"] {
		case "devices":
			devices = l
		case "notice":
			notice = l
		}
	}
	if devices == nil || devices["error"] == nil || devices["cameras"] == nil || devices["mics"] == nil {
		t.Fatalf("devices %v", devices)
	}
	if notice == nil || !strings.Contains(notice["text"].(string), `Unknown command "whatever"`) {
		t.Fatalf("notice %v", notice)
	}
}

func TestIPCCommandQuitStops(t *testing.T) {
	var out lockedBuffer
	ui := newIPCReporter(&out, slog.New(slog.DiscardHandler))
	stopped := false
	c := &ipcCommands{ui: ui, mgr: testManager(t, ui), log: slog.New(slog.DiscardHandler), stop: func() { stopped = true }}
	if !c.handle(context.Background(), ipcCommand{Type: "quit"}) || !stopped {
		t.Fatal("quit did not stop the program")
	}
}

func TestConsoleReporterLines(t *testing.T) {
	var out bytes.Buffer
	c := newConsole(&out)
	c.Banner("v0.13.0", "ab12cd34", "/state")
	c.Update(updateOff, "", "Updates are off (-no-update).")
	c.Source(sourceReport{Kind: "capture", Label: "Cam + Mic", Ready: true, Shape: "1280x720 30 fps, libx264",
		Face:  &faceReport{Label: "OBS Virtual Camera", Ready: true, Shape: "1280x720 30 fps, libx264"},
		Share: &shareReport{Ready: true, Address: "rtsp://127.0.0.1:7446/phone-abc"}})
	c.Drivers("Unit drivers: Mastago (built in); no helpers in /x/units.")
	c.PairedCount(0)
	c.PairedCount(2)
	c.Awake(awakeUnsupported, "")
	c.Awake(awakeHeld, "")
	c.Ready()
	c.Online(true)
	c.Code("123456", time.Time{})
	c.Units([]estim.Unit{{Label: "One"}}, nil, false)
	c.Units([]estim.Unit{{ID: "a", Label: "One"}, {ID: "b", Label: "Two", Held: true}}, &estim.Descriptor{Connected: true, ID: "a"}, true)
	c.Device(estim.Descriptor{Connected: true, Label: "One", Held: true}, false)
	c.Device(estim.Descriptor{Reason: estim.ReasonButtonOff}, false)
	c.Device(estim.Descriptor{Reason: estim.ReasonLetGo}, true)
	c.Link(linkActive, "")
	c.Link(linkReset, "gone")
	c.Enclave(enclaveProof{Image: "sha256:0123456789abcdef0123", Release: "v1.2.3", Commit: "abcdef0123456789", Source: "github.com/FemLed/masseuse-video-tee", Cached: true})
	c.Enclave(enclaveProof{Image: "sha256:0123456789abcdef0123", Release: "v1.2.3", Commit: "abcdef0123456789", Source: "github.com/FemLed/masseuse-video-tee"})
	c.Sending("1280x720 30 fps", 2.5e6, 64e3, true, 1500*time.Millisecond)
	c.NotSending("")
	c.NotSending("ffmpeg exited")
	c.UpdateQuiet(updateChecking, "", "Checking…")
	c.Stopped()
	want := `Masseuse.ai for your computer  (masseuse-camlink v0.13.0)
Identity ab12cd34… (state in /state)
Updates are off (-no-update).
Camera: Cam + Mic (1280x720 30 fps, libx264). It is on only while a session reads it.
Front-facing camera: OBS Virtual Camera (1280x720 30 fps, libx264). It is on only while a session shows it as your face; until then your phone's own camera is.
Your phone's picture: sessions are asked to send it here, and programs on this computer can open it at
  rtsp://127.0.0.1:7446/phone-abc
  (OBS: a Media Source with Local File unticked, that address as the Input, Network Buffering 0 MB.)
Unit drivers: Mastago (built in); no helpers in /x/units.
Paired with 2 phone(s). Sessions that use this camera connect automatically.
This computer stays awake while Masseuse.ai runs (the screen may go dark; keep the lid open).

Pairing code: 123456
Type it into the masseuse.ai app on your phone when it asks for the code from your computer; the dash is added for you.
Stimulation units in reach (2):
  1  One  (serving this one)
  2  Two  (another program on this computer has it open)
Type a number and Enter to serve another unit; the phone can pick one too. A unit in use by a session is switched once the session stops it.
Stimulation device connected: One. It is held at zero until a session on your phone uses this computer.
` + heldByAnotherLine + `
Stimulation device disconnected: it was switched off at its power button. It reconnects on its own when it is on again.
Camera link active: connected to the verified enclave.
The enclave closed the camera link (gone); waiting for the service.
Enclave image 0123456789ab… is github.com/FemLed/masseuse-video-tee v1.2.3 (commit abcdef0): signature and build provenance verified in the public registry and the Sigstore log.
Sending 1280x720 30 fps: video 2.5 Mb/s, audio 64 kb/s
Connection congested: dropping video to keep up (backlog 1.5 s).
Camera on but not sending yet (waiting for the source).
Camera on but not sending yet: ffmpeg exited.

Stopped.
`
	if out.String() != want {
		t.Fatalf("console:\n%s\nwant:\n%s", out.String(), want)
	}
}

// TestIPCModeOverPipes runs the built program with -ipc against no
// service: its first line is the hello, a source report follows, and its
// standard input ending stops it cleanly.
func TestIPCModeOverPipes(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the program")
	}
	bin := filepath.Join(t.TempDir(), "masseuse-camlink")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	stateDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-ipc", "-state-dir", stateDir, "-service", "https://127.0.0.1:1",
		"-no-update", "-allow-sleep", "-estim-ble", "off", "-estim-helpers", "none", "-ffmpeg", filepath.Join(stateDir, "no-ffmpeg"))
	cmd.Env = append(os.Environ(), "MASSEUSE_CAMLINK_UPDATE_AS=")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr lockedBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), ipcMaxLine)
	read := func(want string) map[string]any {
		t.Helper()
		for sc.Scan() {
			var m map[string]any
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				t.Fatalf("not JSON: %q", sc.Text())
			}
			if m["type"] == want {
				return m
			}
			if m["type"] == "blocked" {
				t.Fatalf("blocked: %v\n%s", m, stderr.String())
			}
		}
		t.Fatalf("no %s event; stderr:\n%s", want, stderr.String())
		return nil
	}
	hello := read("hello")["hello"].(map[string]any)
	if hello["stateDir"] != stateDir || hello["updates"] != "off" || hello["awake"] != false || hello["phones"] != float64(0) {
		t.Fatalf("hello %v", hello)
	}
	if src := read("source"); src["ready"] != false || src["kind"] != "capture" {
		t.Fatalf("source %v", src)
	}
	// The window asks for the devices: with no ffmpeg the answer says so.
	fmt.Fprintln(stdin, `{"type":"list_devices"}`)
	if devs := read("devices"); devs["error"] == nil {
		t.Fatalf("devices %v", devs)
	}
	stdin.Close()
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	select {
	case err := <-waitErr:
		if err != nil {
			t.Fatalf("exit: %v\n%s", err, stderr.String())
		}
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("did not stop after standard input ended\n%s", stderr.String())
	}
}
