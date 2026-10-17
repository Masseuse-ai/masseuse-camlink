package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Masseuse-ai/masseuse-camlink/internal/capture"
)

// What source.json keeps of the devices: their names as resolved, and for
// one not connected the name it was chosen by, never what stands in.
func TestRememberedDevicesKeepTheChoice(t *testing.T) {
	cam := capture.Device{Kind: capture.Video, ID: "1", Name: "Insta360 Link"}
	mic := capture.Device{Kind: capture.Audio, ID: "0", Name: "Yeti Stereo Microphone"}
	builtIn := capture.Device{Kind: capture.Audio, ID: "2", Name: "MacBook Pro Microphone"}
	for _, tc := range []struct {
		name      string
		cfg       sourceConfig
		cam       capture.Device
		mic       *capture.Device
		subs      []capture.Substitution
		wantCam   string
		wantMic   string
		unchanged bool
	}{
		{name: "numbers become names", cfg: sourceConfig{Camera: "1", Mic: "0"}, cam: cam, mic: &mic, wantCam: "Insta360 Link", wantMic: "Yeti Stereo Microphone"},
		{name: "the default becomes a name", cfg: sourceConfig{}, cam: cam, mic: &mic, wantCam: "Insta360 Link", wantMic: "Yeti Stereo Microphone"},
		{name: "a stand-in microphone", cfg: sourceConfig{Camera: "Insta360 Link", Mic: "USB Mic"}, cam: cam, mic: &builtIn,
			subs: []capture.Substitution{{Kind: capture.Audio, Wanted: "USB Mic", Using: builtIn}}, wantCam: "Insta360 Link", wantMic: "USB Mic"},
		{name: "nothing in the microphone's place", cfg: sourceConfig{Camera: "Insta360 Link", Mic: "USB Mic"}, cam: cam,
			subs: []capture.Substitution{{Kind: capture.Audio, Wanted: "USB Mic", Using: capture.Device{Kind: capture.Audio}}}, wantCam: "Insta360 Link", wantMic: "USB Mic"},
		{name: "a stand-in camera", cfg: sourceConfig{Camera: "Studio Cam 4K", Mic: "Yeti Stereo Microphone"}, cam: cam, mic: &mic,
			subs: []capture.Substitution{{Kind: capture.Video, Wanted: "Studio Cam 4K", Using: cam}}, wantCam: "Studio Cam 4K", wantMic: "Yeti Stereo Microphone"},
		{name: "no microphone connected is not a choice of none", cfg: sourceConfig{}, cam: cam, wantCam: "Insta360 Link", wantMic: ""},
		{name: "none stays none", cfg: sourceConfig{Mic: "NONE"}, cam: cam, wantCam: "Insta360 Link", wantMic: "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := rememberedDevices(tc.cfg, tc.cam, tc.mic, tc.subs)
			if got.Camera != tc.wantCam || got.Mic != tc.wantMic {
				t.Fatalf("remembered camera %q mic %q, want %q %q", got.Camera, got.Mic, tc.wantCam, tc.wantMic)
			}
		})
	}
}

// fakeCapture stands in for the capture behind an offer: its standing is
// the test's, a microphone the system refuses reading as left out.
type fakeCapture struct {
	mu      sync.Mutex
	st      capture.Standing
	refused bool
}

func (f *fakeCapture) standing() capture.Standing {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.st
	if f.refused && st.Mic != nil && st.MicOut == "" {
		st.MicOut = capture.MicRefused
	}
	return st
}

func (f *fakeCapture) refuseMic(refused bool) {
	f.mu.Lock()
	f.refused = refused
	f.mu.Unlock()
}

func (f *fakeCapture) set(st capture.Standing) {
	f.mu.Lock()
	f.st = st
	f.mu.Unlock()
}

func (f *fakeCapture) isRefused() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refused
}

func (f *fakeCapture) offer() *offer {
	return &offer{kind: "capture", label: "Cam + Mic", ready: true, start: func() {}, stop: func() {},
		standing: f.standing, refuseMic: f.refuseMic, save: sourceConfig{Kind: "capture"}}
}

// lastEvent is the last event of the kind the window was sent, nil for none.
func lastEvent(t *testing.T, out *lockedBuffer, kind string) map[string]any {
	t.Helper()
	var last map[string]any
	for _, ev := range ipcLines(t, []byte(out.String())) {
		if ev["type"] == kind {
			last = ev
		}
	}
	return last
}

// The window is told what the capture uses: the devices in use, what
// stands in for what, a microphone left out and why, as it changes; the
// shell's word that macOS refuses the microphone reaches the capture, and
// every capture after it.
func TestTheWindowIsToldWhatTheCaptureUses(t *testing.T) {
	var out lockedBuffer
	ui := newIPCReporter(&out, slog.New(slog.DiscardHandler))
	ui.Ready()
	m := testManager(t, ui)
	var reports atomic.Int32
	m.cam.onSource = func() { m.reportSource(); reports.Add(1) }
	cmds := &ipcCommands{ui: ui, mgr: m, log: slog.New(slog.DiscardHandler)}
	cam := capture.Device{Kind: capture.Video, ID: "0", Name: "FaceTime HD Camera"}
	builtIn := capture.Device{Kind: capture.Audio, ID: "0", Name: "MacBook Pro Microphone"}
	plain := capture.Standing{Label: "FaceTime HD Camera + MacBook Pro Microphone", Camera: cam, Mic: &builtIn}
	f := &fakeCapture{st: plain}
	if err := m.cam.setOffer(f.offer()); err != nil {
		t.Fatal(err)
	}
	m.reportSource()
	src := lastEvent(t, &out, "source")
	if src["camera"] != "FaceTime HD Camera" || src["mic"] != "MacBook Pro Microphone" || src["micOut"] != nil || src["substitutions"] != nil {
		t.Fatalf("source %v", src)
	}

	// macOS refuses the microphone: the capture leaves it out, the window
	// hears of it, and the next capture chosen is told at once.
	cmds.handle(context.Background(), ipcCommand{Type: "media", Camera: "authorized", Mic: "denied"})
	if !f.isRefused() {
		t.Fatal("the capture was not told")
	}
	waitUntil(t, "the refusal reported", func() bool { return lastEvent(t, &out, "source")["micOut"] == "refused" })
	next := &fakeCapture{st: plain}
	if err := m.cam.setOffer(next.offer()); err != nil {
		t.Fatal(err)
	}
	if !next.isRefused() {
		t.Fatal("a new capture did not learn the refusal")
	}
	cmds.handle(context.Background(), ipcCommand{Type: "media", Camera: "authorized", Mic: "authorized"})
	if next.isRefused() {
		t.Fatal("still refused once allowed")
	}
	waitUntil(t, "the allowance reported", func() bool {
		src := lastEvent(t, &out, "source")
		return src["micOut"] == nil && src["mic"] == "MacBook Pro Microphone"
	})

	// While a session runs: the microphone chosen is gone and another
	// stands in; then it does not work; then the one chosen is back.
	o := m.cam.offer
	yeti := capture.Device{Kind: capture.Audio, ID: "1", Name: "Yeti Stereo Microphone"}
	next.set(capture.Standing{Label: "FaceTime HD Camera + MacBook Pro Microphone", Camera: cam, Mic: &builtIn,
		Substitutions: []capture.Substitution{{Kind: capture.Audio, Wanted: yeti.Name, Using: builtIn}}})
	m.cam.watchStanding(o)
	waitUntil(t, "the stand-in reported", func() bool {
		subs, _ := lastEvent(t, &out, "source")["substitutions"].([]any)
		return len(subs) == 1 && subs[0].(map[string]any)["wanted"] == yeti.Name && subs[0].(map[string]any)["using"] == builtIn.Name
	})
	if n := lastEvent(t, &out, "notice"); n["text"] != "Yeti Stereo Microphone is not connected; using MacBook Pro Microphone." {
		t.Fatalf("notice %v", n)
	}
	st := next.standing()
	st.MicOut = capture.MicFailed
	next.set(st)
	m.cam.watchStanding(o)
	waitUntil(t, "the failure reported", func() bool { return lastEvent(t, &out, "source")["micOut"] == "failed" })
	if n := lastEvent(t, &out, "notice"); !strings.HasPrefix(n["text"].(string), "MacBook Pro Microphone did not work, so the picture goes without this computer's sound") {
		t.Fatalf("notice %v", n)
	}
	next.set(capture.Standing{Label: "FaceTime HD Camera + Yeti Stereo Microphone", Camera: cam, Mic: &yeti})
	m.cam.watchStanding(o)
	waitUntil(t, "the microphone back", func() bool { return lastEvent(t, &out, "source")["mic"] == yeti.Name })
	if n := lastEvent(t, &out, "notice"); n["text"] != "Yeti Stereo Microphone is connected again and in use." {
		t.Fatalf("notice %v", n)
	}
	// Nothing changed, nothing said: once the five reports so far are out.
	waitUntil(t, "the reports", func() bool { return reports.Load() == 5 })
	before := out.String()
	m.cam.watchStanding(o)
	if out.String() != before {
		t.Fatal("said something with nothing changed")
	}
}

// A window's choice naming a microphone that is not connected gets the
// first one in its place, as a remembered choice does, and the choice is
// remembered by its own name, through any later change of something else.
// A fake ffmpeg stands in as the bundled one, so no device of this
// computer's is opened.
func TestTheWindowsChoiceOutlastsAMissingMicrophone(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("lists devices through a fake of ffmpeg's avfoundation input")
	}
	dir := t.TempDir()
	fake := `#!/bin/sh
for a in "$@"; do
	if [ "$a" = "-list_devices" ]; then
		cat >&2 <<'EOF'
[AVFoundation indev @ 0x1] AVFoundation video devices:
[AVFoundation indev @ 0x1] [0] Test Camera
[AVFoundation indev @ 0x1] AVFoundation audio devices:
[AVFoundation indev @ 0x1] [0] Test Microphone
[in#0 @ 0x2] Error opening input: Input/output error
EOF
		exit 1
	fi
done
echo "[AVFoundation indev @ 0x1] Selected video size (1x1) is not supported by the device." >&2
echo "[AVFoundation indev @ 0x1] Supported modes:" >&2
echo "[AVFoundation indev @ 0x1]   1280x720@[1.000000 30.000030]fps" >&2
exit 1
`
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	was := capture.BundledDir
	capture.BundledDir = dir
	t.Cleanup(func() { capture.BundledDir = was })

	var out lockedBuffer
	ui := newIPCReporter(&out, slog.New(slog.DiscardHandler))
	ui.Ready()
	m := testManager(t, ui)
	if err := m.applySource(context.Background(), sourceChoice{Camera: "Test Camera", Mic: "Gone Microphone"}); err != nil {
		t.Fatal(err)
	}
	src := lastEvent(t, &out, "source")
	subs, _ := src["substitutions"].([]any)
	if src["ready"] != true || src["camera"] != "Test Camera" || src["mic"] != "Test Microphone" || len(subs) != 1 ||
		!reflect.DeepEqual(subs[0], map[string]any{"kind": "audio", "wanted": "Gone Microphone", "using": "Test Microphone"}) {
		t.Fatalf("source %v", src)
	}
	if saved := loadSourceConfig(m.stateDir); saved.Camera != "Test Camera" || saved.Mic != "Gone Microphone" {
		t.Fatalf("remembered %+v", saved)
	}
	// Something else changes: the microphone chosen is still the one kept.
	if err := m.applySource(context.Background(), sourceChoice{FaceCamera: "phone"}); err != nil {
		t.Fatal(err)
	}
	if saved := loadSourceConfig(m.stateDir); saved.Mic != "Gone Microphone" {
		t.Fatalf("remembered after a change of the face %+v", saved)
	}
}

func TestResolveSourceConfigKeepsEachPartUnlessSpoken(t *testing.T) {
	dir := t.TempDir()
	// Nothing saved, no flags: the computer's first camera and microphone,
	// no front-facing camera, the phone's picture not wanted.
	cfg, err := resolveSourceConfig(dir, sourceFlags{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Kind != "capture" || cfg.FaceCamera != "" || cfg.SharePhone {
		t.Fatalf("defaults %+v", cfg)
	}
	// The camera chosen and remembered.
	cfg, err = resolveSourceConfig(dir, sourceFlags{camera: "1", mic: "0"})
	if err != nil {
		t.Fatal(err)
	}
	if err := saveSourceConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}
	// A front-facing camera added to it, and the phone's picture asked for:
	// the camera part is as remembered.
	cfg, err = resolveSourceConfig(dir, sourceFlags{faceCamera: "OBS Virtual Camera", sharePhone: "on"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Kind != "capture" || cfg.Camera != "1" || cfg.Mic != "0" || cfg.FaceCamera != "OBS Virtual Camera" || !cfg.SharePhone {
		t.Fatalf("added: %+v", cfg)
	}
	cfg.ShareSecret = "s3cret"
	cfg.SharePort = 7446
	if err := saveSourceConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}
	// A new camera on the network replaces the camera part alone: the face
	// and the share, secret included, stay.
	cfg, err = resolveSourceConfig(dir, sourceFlags{cameraURL: "rtsps://cam.local:322/live"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Kind != "camera" || cfg.URL != "rtsps://cam.local:322/live" || cfg.Camera != "" ||
		cfg.FaceCamera != "OBS Virtual Camera" || !cfg.SharePhone || cfg.ShareSecret != "s3cret" || cfg.SharePort != 7446 {
		t.Fatalf("replaced camera: %+v", cfg)
	}
	if err := saveSourceConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}
	// The face's encoding alone, with a face remembered.
	cfg, err = resolveSourceConfig(dir, sourceFlags{faceFPS: 24, faceBitrate: "1500k"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FaceCamera != "OBS Virtual Camera" || cfg.FaceFPS != 24 || cfg.FaceBitrate != "1500k" {
		t.Fatalf("face encoding: %+v", cfg)
	}
	// `-face-camera none` takes the front-facing camera away; the share
	// stays until told off.
	cfg, err = resolveSourceConfig(dir, sourceFlags{faceCamera: "none"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FaceCamera != "" || cfg.FaceFPS != 0 || !cfg.SharePhone || cfg.Kind != "camera" {
		t.Fatalf("face none: %+v", cfg)
	}
	if err := saveSourceConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}
	cfg, err = resolveSourceConfig(dir, sourceFlags{sharePhone: "off"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SharePhone || cfg.ShareSecret != "s3cret" {
		t.Fatalf("share off keeps the secret for next time: %+v", cfg)
	}
	// What is refused.
	for _, bad := range []sourceFlags{
		{faceCamera: "none", faceFPS: 30},
		{faceFPS: 30}, // no face remembered now
		{sharePhone: "maybe"},
		{sharePort: 70000},
		{cameraURL: "rtsps://x/live", camera: "0"},
		{cameraFingerprint: "ab"},
	} {
		if _, err := resolveSourceConfig(dir, bad); err == nil {
			t.Fatalf("%+v accepted", bad)
		}
	}
	// The saved file carries the new fields by their names.
	b, err := os.ReadFile(filepath.Join(dir, sourceFile))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["sharePhone"] != true || raw["shareSecret"] != "s3cret" || raw["kind"] != "camera" {
		t.Fatalf("saved %v", raw)
	}
}
