package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Masseuse-ai/masseuse-camlink/internal/capture"
)

// picture is the stand-in preview's every picture.
var picture = []byte{0xFF, 0xD8, 0xFF, 0xD9}

// previewCam is a computer camera whose preview sends a picture every 10 ms
// until it is stopped, counting its runs and the ones running; with fail,
// each run ends at once with it.
type previewCam struct {
	runs, running atomic.Int32
	fail          error
}

func (p *previewCam) offer() *offer {
	return &offer{kind: "capture", label: "Cam + Mic", ready: true, start: func() {}, stop: func() {}, size: "1920x1080",
		save: sourceConfig{Kind: "capture", Camera: "Cam", Mic: "Mic"},
		preview: func(ctx context.Context, frame func([]byte)) error {
			p.runs.Add(1)
			p.running.Add(1)
			defer p.running.Add(-1)
			if p.fail != nil {
				return p.fail
			}
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-tick.C:
					frame(picture)
				}
			}
		}}
}

// previewEvents is what the preview and camera events said, in order: the
// preview's standing (and its note when unavailable), the camera's on.
func previewEvents(t *testing.T, out *lockedBuffer) []string {
	t.Helper()
	var got []string
	for _, line := range ipcLines(t, []byte(out.String())) {
		switch line["type"] {
		case "preview":
			s := fmt.Sprintf("preview %v", line["state"])
			if note, _ := line["note"].(string); note != "" {
				s += ": " + note
			}
			got = append(got, s)
		case "camera":
			got = append(got, fmt.Sprintf("camera on=%v", line["on"]))
		}
	}
	return got
}

// pictures counts the preview_frame lines, checking each carries picture.
func pictures(t *testing.T, out *lockedBuffer) int {
	t.Helper()
	n := 0
	for _, line := range ipcLines(t, []byte(out.String())) {
		if line["type"] != "preview_frame" {
			continue
		}
		if line["jpeg"] != base64.StdEncoding.EncodeToString(picture) {
			t.Fatalf("picture %v", line["jpeg"])
		}
		n++
	}
	return n
}

func previewSetup(t *testing.T) (*lockedBuffer, *manager, *ipcCommands, *previewCam) {
	t.Helper()
	out := &lockedBuffer{}
	ui := newIPCReporter(out, slog.New(slog.DiscardHandler))
	ui.Ready()
	m := testManager(t, ui)
	cam := &previewCam{}
	m.cam.offer = cam.offer()
	t.Cleanup(func() { m.cam.off(false) })
	return out, m, &ipcCommands{ui: ui, mgr: m, log: slog.New(slog.DiscardHandler), stop: func() {}}, cam
}

func ask(c *ipcCommands, on bool) {
	c.handle(context.Background(), ipcCommand{Type: "preview", On: &on})
}

// TestPreviewGivesTheCameraToTheSession: asked for, the preview says it is
// starting, then live, and its pictures follow in base64; a session's
// stream takes the camera from it before the capture starts, and while the
// session has the camera the window's ask is refused, until it lets go.
func TestPreviewGivesTheCameraToTheSession(t *testing.T) {
	out, m, c, cam := previewSetup(t)
	ask(c, true)
	waitUntil(t, "pictures", func() bool { return pictures(t, out) >= 3 })
	lines := ipcLines(t, []byte(out.String()))
	var first map[string]any
	for _, l := range lines {
		if l["type"] == "preview" {
			first = l
			break
		}
	}
	if first["state"] != "starting" || first["camera"] != "Cam" || first["width"] != float64(1920) || first["height"] != float64(1080) {
		t.Fatalf("first preview event %v", first)
	}
	ask(c, true) // renewed, not started twice
	if cam.runs.Load() != 1 {
		t.Fatalf("%d runs", cam.runs.Load())
	}

	conn, err := m.cam.dialLocal()
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if cam.running.Load() != 0 {
		t.Fatal("the preview still runs while the session has the camera")
	}
	ask(c, true)
	if cam.runs.Load() != 1 {
		t.Fatal("the preview started while a session had the camera")
	}
	m.cam.off(false)
	ask(c, true)
	waitUntil(t, "the preview again", func() bool { return cam.running.Load() == 1 })
	waitUntil(t, "live again", func() bool {
		ev := previewEvents(t, out)
		return ev[len(ev)-1] == "preview live"
	})
	want := []string{
		"camera on=false", // Ready's standing
		"preview starting",
		"preview live",
		"preview off", // the session's stream takes the camera...
		"camera on=true",
		"preview unavailable: a session has the camera",
		"camera on=false",
		"preview starting",
		"preview live",
	}
	if got := previewEvents(t, out); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestPreviewEndsWhenTheWindowStopsAsking: the window's asks keep the
// preview on; without one for the lease it goes off, and off goes at once.
func TestPreviewEndsWhenTheWindowStopsAsking(t *testing.T) {
	out, m, c, cam := previewSetup(t)
	m.cam.lease = 150 * time.Millisecond
	ask(c, true)
	for range 6 {
		time.Sleep(50 * time.Millisecond)
		ask(c, true)
	}
	if cam.runs.Load() != 1 || cam.running.Load() != 1 {
		t.Fatalf("runs %d, running %d while asked for", cam.runs.Load(), cam.running.Load())
	}
	waitUntil(t, "the lease to lapse", func() bool { return cam.running.Load() == 0 })
	ask(c, true)
	waitUntil(t, "a second run", func() bool { return cam.running.Load() == 1 })
	ask(c, false)
	if cam.running.Load() != 0 {
		t.Fatal("off left the preview running")
	}
	if got := previewEvents(t, out); got[len(got)-1] != "preview off" {
		t.Fatalf("events %v", got)
	}
}

// TestPreviewStopsForTheSwitchAndANewCamera: the window's switch stops
// the preview and refuses it while off; a camera on the network has none;
// a change of camera ends the old camera's.
func TestPreviewStopsForTheSwitchAndANewCamera(t *testing.T) {
	out, m, c, cam := previewSetup(t)
	ask(c, true)
	waitUntil(t, "the preview", func() bool { return cam.running.Load() == 1 })
	off, on := false, true
	c.handle(context.Background(), ipcCommand{Type: "set_camera", View: "body", Enabled: &off})
	if cam.running.Load() != 0 {
		t.Fatal("switched off, the preview runs on")
	}
	ask(c, true)
	if got := previewEvents(t, out); got[len(got)-1] != "preview unavailable: the camera is switched off" || cam.runs.Load() != 1 {
		t.Fatalf("asked while switched off: %v", got)
	}
	c.handle(context.Background(), ipcCommand{Type: "set_camera", View: "body", Enabled: &on})
	ask(c, true)
	waitUntil(t, "the preview again", func() bool { return cam.running.Load() == 1 })

	other := &previewCam{}
	if err := m.cam.setOffer(other.offer()); err != nil {
		t.Fatal(err)
	}
	if cam.running.Load() != 0 {
		t.Fatal("the old camera's preview runs on")
	}
	ask(c, true)
	waitUntil(t, "the new camera's preview", func() bool { return other.running.Load() == 1 })

	network := &offer{kind: "camera", label: "Porch", ready: true, start: func() {}, stop: func() {}}
	if err := m.cam.setOffer(network); err != nil {
		t.Fatal(err)
	}
	ask(c, true)
	if got := previewEvents(t, out); got[len(got)-1] != "preview unavailable: a camera on your network shows its picture in its own app" {
		t.Fatalf("network camera: %v", got)
	}
	if err := m.cam.setOffer(&offer{kind: "capture", label: "This computer's camera", note: "no camera found"}); err != nil {
		t.Fatal(err)
	}
	ask(c, true)
	if got := previewEvents(t, out); got[len(got)-1] != "preview unavailable: no camera found" {
		t.Fatalf("no camera: %v", got)
	}
}

// TestPreviewSaysWhyItEndedAndTriesAgain: a preview that ends on its own
// says why in the person's words and is tried again while the window asks.
func TestPreviewSaysWhyItEndedAndTriesAgain(t *testing.T) {
	out, m, c, cam := previewSetup(t)
	cam.fail = &capture.PreviewError{Reason: "the camera could not be opened; another program may have it, or it was unplugged", Err: errors.New("ffmpeg: exit status 1")}
	m.cam.retry = 20 * time.Millisecond
	ask(c, true)
	waitUntil(t, "tries again", func() bool { return cam.runs.Load() >= 3 })
	ask(c, false)
	got := previewEvents(t, out)
	if got[1] != "preview starting" || got[2] != "preview unavailable: the camera could not be opened; another program may have it, or it was unplugged" {
		t.Fatalf("events %v", got)
	}
	if pictures(t, out) != 0 {
		t.Fatal("pictures from a camera that would not open")
	}
}

// TestIPCPreviewFrames: pictures go out only after the hello, as base64
// JPEG, and one still waiting when the preview ends is dropped.
func TestIPCPreviewFrames(t *testing.T) {
	out := &lockedBuffer{}
	r := newIPCReporter(out, slog.New(slog.DiscardHandler))
	r.PreviewFrame(picture)
	time.Sleep(50 * time.Millisecond)
	if out.String() != "" {
		t.Fatalf("written before the hello: %s", out.String())
	}
	r.Ready()
	r.Preview(previewLive, "", "Cam", "1552x1552")
	r.PreviewFrame(picture)
	waitUntil(t, "a picture", func() bool { return pictures(t, out) == 1 })
	var live map[string]any
	for _, l := range ipcLines(t, []byte(out.String())) {
		if l["type"] == "preview" {
			live = l
		}
	}
	if live["state"] != "live" || live["width"] != float64(1552) || live["height"] != float64(1552) || live["note"] != nil {
		t.Fatalf("preview %v", live)
	}
	// The console has no preview at all.
	var console lockedBuffer
	con := newConsole(&console)
	con.Preview(previewLive, "", "Cam", "1920x1080")
	con.PreviewFrame(picture)
	if console.String() != "" {
		t.Fatalf("the console said %q", console.String())
	}
}
