package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/camera"
	"github.com/FemLed/masseuse-camlink/internal/capture"
	"github.com/FemLed/masseuse-camlink/internal/rendezvous"
	"github.com/FemLed/masseuse-camlink/internal/serve"
)

// sourceFile remembers the camera chosen on the command line, so the next
// start without flags uses it again.
const sourceFile = "source.json"

// sourceConfig is the saved choice: the camera (the session's body view),
// and, when the person set them up, the front-facing camera and whether the
// phone's picture is wanted on this computer.
type sourceConfig struct {
	Kind string `json:"kind"` // "capture" or "camera"
	// capture
	Camera    string `json:"camera,omitempty"`
	Mic       string `json:"mic,omitempty"`
	VideoSize string `json:"videoSize,omitempty"`
	FPS       int    `json:"fps,omitempty"`
	Bitrate   string `json:"bitrate,omitempty"`
	Encoder   string `json:"encoder,omitempty"`
	FFmpeg    string `json:"ffmpeg,omitempty"`
	// camera
	URL         string `json:"url,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	// The front-facing camera (-face-camera): a second capture of the
	// computer's, a virtual camera such as OBS's or any other, served as
	// the person's face (internal/serve, FacePath). Empty: none, and the
	// phone's own camera stays the face view.
	FaceCamera    string `json:"faceCamera,omitempty"`
	FaceVideoSize string `json:"faceVideoSize,omitempty"`
	FaceFPS       int    `json:"faceFps,omitempty"`
	FaceBitrate   string `json:"faceBitrate,omitempty"`
	// The phone's picture on this computer (-share-phone, internal/share):
	// whether it is wanted, the loopback port it is served on, and the
	// secret in its path, kept so a program set up to read it once keeps
	// working.
	SharePhone  bool   `json:"sharePhone,omitempty"`
	SharePort   int    `json:"sharePort,omitempty"`
	ShareSecret string `json:"shareSecret,omitempty"`
}

// sourceFlags are the command-line choices.
type sourceFlags struct {
	camera, mic, videoSize, bitrate, encoder, ffmpeg string
	fps                                              int
	cameraURL, cameraFingerprint                     string
	// The front-facing camera: "none" clears a remembered one.
	faceCamera, faceVideoSize, faceBitrate string
	faceFPS                                int
	// sharePhone is "on", "off" or "" (as remembered); sharePort 0 is as
	// remembered, or the default.
	sharePhone string
	sharePort  int
}

// bodyAny says whether any flag for the camera (the body view) is given.
func (f sourceFlags) bodyAny() bool {
	return f.camera != "" || f.mic != "" || f.videoSize != "" || f.bitrate != "" || f.encoder != "" ||
		f.ffmpeg != "" || f.fps != 0 || f.cameraURL != "" || f.cameraFingerprint != ""
}

// faceAny says whether any flag for the front-facing camera is given.
func (f sourceFlags) faceAny() bool {
	return f.faceCamera != "" || f.faceVideoSize != "" || f.faceBitrate != "" || f.faceFPS != 0
}

// shareAny says whether the phone's picture was spoken of.
func (f sourceFlags) shareAny() bool { return f.sharePhone != "" || f.sharePort != 0 }

func (f sourceFlags) any() bool { return f.bodyAny() || f.faceAny() || f.shareAny() }

// resolveSourceConfig turns flags into the configuration to use. Each part
// is the flags' when any of its flags is given (and is then saved), else
// the saved one: the camera (else the computer's first camera and
// microphone), the front-facing camera (else none), the phone's picture
// (else not wanted). So `-face-camera "OBS Virtual Camera"` adds a
// front-facing camera to the remembered camera, and `-face-camera none`
// takes it away again.
func resolveSourceConfig(stateDir string, f sourceFlags) (sourceConfig, error) {
	return mergeSourceConfig(loadSourceConfig(stateDir), f)
}

// loadSourceConfig is the remembered choice, or the default (the
// computer's first camera and microphone) when there is none.
func loadSourceConfig(stateDir string) sourceConfig {
	var saved sourceConfig
	if b, err := os.ReadFile(filepath.Join(stateDir, sourceFile)); err == nil {
		var cfg sourceConfig
		if json.Unmarshal(b, &cfg) == nil && (cfg.Kind == "capture" || cfg.Kind == "camera") {
			saved = cfg
		}
	}
	if saved.Kind == "" {
		saved.Kind = "capture"
	}
	return saved
}

// mergeSourceConfig applies the flags to the configuration in force: each
// part the flags speak of is theirs, the rest stays (resolveSourceConfig;
// the window's set_source, apply.go).
func mergeSourceConfig(saved sourceConfig, f sourceFlags) (sourceConfig, error) {
	if saved.Kind == "" {
		saved.Kind = "capture"
	}
	cfg := saved
	if f.bodyAny() {
		if f.cameraURL != "" {
			if f.camera != "" || f.mic != "" || f.videoSize != "" || f.bitrate != "" || f.encoder != "" || f.ffmpeg != "" || f.fps != 0 {
				return sourceConfig{}, errors.New("-camera-url names a camera on your network; the -camera, -mic and encoding flags are for the computer's own camera and cannot be combined with it")
			}
			cfg = sourceConfig{Kind: "camera", URL: f.cameraURL, Fingerprint: f.cameraFingerprint}
		} else {
			if f.cameraFingerprint != "" {
				return sourceConfig{}, errors.New("-camera-fingerprint goes with -camera-url")
			}
			cfg = sourceConfig{Kind: "capture", Camera: f.camera, Mic: f.mic, VideoSize: f.videoSize, FPS: f.fps,
				Bitrate: f.bitrate, Encoder: f.encoder, FFmpeg: f.ffmpeg}
		}
		// The other parts are as remembered.
		cfg.FaceCamera, cfg.FaceVideoSize, cfg.FaceFPS, cfg.FaceBitrate = saved.FaceCamera, saved.FaceVideoSize, saved.FaceFPS, saved.FaceBitrate
		cfg.SharePhone, cfg.SharePort, cfg.ShareSecret = saved.SharePhone, saved.SharePort, saved.ShareSecret
	}
	if f.faceAny() {
		if strings.EqualFold(strings.TrimSpace(f.faceCamera), "none") {
			if f.faceVideoSize != "" || f.faceBitrate != "" || f.faceFPS != 0 {
				return sourceConfig{}, errors.New("-face-camera none takes the front-facing camera away; its encoding flags go with a camera")
			}
			cfg.FaceCamera, cfg.FaceVideoSize, cfg.FaceFPS, cfg.FaceBitrate = "", "", 0, ""
		} else {
			if f.faceCamera == "" && saved.FaceCamera == "" {
				return sourceConfig{}, errors.New("the -face-video-size, -face-fps and -face-bitrate flags go with -face-camera")
			}
			if f.faceCamera != "" {
				cfg.FaceCamera = f.faceCamera
			}
			if f.faceVideoSize != "" {
				cfg.FaceVideoSize = f.faceVideoSize
			}
			if f.faceFPS != 0 {
				cfg.FaceFPS = f.faceFPS
			}
			if f.faceBitrate != "" {
				cfg.FaceBitrate = f.faceBitrate
			}
		}
	}
	if f.shareAny() {
		switch strings.ToLower(strings.TrimSpace(f.sharePhone)) {
		case "on", "yes", "true":
			cfg.SharePhone = true
		case "off", "no", "false":
			cfg.SharePhone = false
		case "":
		default:
			return sourceConfig{}, errors.New("-share-phone takes on or off")
		}
		if f.sharePort != 0 {
			if f.sharePort < 1 || f.sharePort > 65535 {
				return sourceConfig{}, errors.New("-share-port must be a port number")
			}
			cfg.SharePort = f.sharePort
		}
	}
	return cfg, nil
}

func saveSourceConfig(stateDir string, cfg sourceConfig) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(stateDir, sourceFile), append(b, '\n'), 0o600)
}

// offer is the camera the connector serves through its own stream: what it
// is called, whether it can be served, and how to turn it on and off.
type offer struct {
	kind  string
	label string
	ready bool
	note  string // why it is not ready, for the person
	// shape describes the picture for the console ("1280x720 30 fps").
	shape      string
	start      func()
	stop       func()
	publishing func() bool
	// trouble, when the source can say, is why it is on but not sending
	// (capture.Source.Trouble); "" when it is sending or has not failed.
	trouble func() string
	// adapt, when the source can change its bit rate (the computer's own
	// camera), runs the controller that lowers it while the connection
	// cannot keep up and raises it back (internal/capture, Adapter); it
	// runs while the camera is on.
	adapt *capture.Adapter
	// save is the configuration to remember: devices by name, so a later
	// start finds them even if their numbering changed.
	save sourceConfig
	// subs are the remembered devices that were not connected and what
	// stands in for them (capture.Substitution); the window lists them.
	subs []capture.Substitution
}

func (o *offer) source() rendezvous.Source {
	return rendezvous.Source{Kind: o.kind, Label: o.label, Ready: o.ready}
}

// buildOffer prepares the configured source; it does not turn anything on.
// Errors are the person's to fix (a wrong flag); a missing ffmpeg or camera
// is reported as an offer that is not ready, so pairing still works. With
// saved, cfg is the remembered choice rather than today's flags: a device
// it names that is not connected gives way to the first of its kind, and a
// line says so; the remembered name stands for the day it is back.
func buildOffer(ctx context.Context, cfg sourceConfig, stateDir string, sink *serve.Server, logger *slog.Logger, ui reporter, saved bool) (*offer, error) {
	switch cfg.Kind {
	case "camera":
		src, err := camera.New(ctx, sink, camera.Options{
			URL: cfg.URL, Fingerprint: cfg.Fingerprint, StateDir: stateDir, Logger: logger,
			OnTrust: func(host, fp string) {
				ui.Notice(noticeInfo, fmt.Sprintf("Trusting camera %s with certificate %s (saved; pass -camera-fingerprint to pin one yourself).", host, fp))
			},
		})
		if err != nil {
			return nil, err
		}
		o := &offer{kind: "camera", label: src.Label(), start: src.Start, stop: src.Stop, publishing: src.Publishing, save: cfg}
		pctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		desc, err := src.Probe(pctx)
		if err != nil {
			o.note = err.Error()
			return o, nil
		}
		o.ready = true
		kinds := make([]string, 0, len(desc.Medias))
		for _, m := range desc.Medias {
			if len(m.Formats) > 0 {
				kinds = append(kinds, strings.ToLower(m.Formats[0].Codec())+" "+string(m.Type))
			}
		}
		o.shape = strings.Join(kinds, ", ")
		return o, nil
	default:
		opts := capture.Options{Camera: cfg.Camera, Mic: cfg.Mic, VideoSize: cfg.VideoSize, FPS: cfg.FPS,
			Bitrate: cfg.Bitrate, Encoder: cfg.Encoder, FFmpeg: cfg.FFmpeg, Fallback: saved}
		src, err := capture.New(ctx, sink, opts, logger)
		if err != nil {
			if capture.IsNoFFmpeg(err) || (errors.Is(err, capture.ErrNoDevice) && (saved || (cfg.Camera == "" && cfg.Mic == ""))) {
				// Nothing to fix on the command line: report it and carry on.
				return &offer{kind: "capture", label: "This computer's camera", note: err.Error(), save: cfg}, nil
			}
			return nil, err
		}
		for _, sub := range src.Substitutions() {
			ui.Notice(noticeInfo, fmt.Sprintf("%s is not connected; using %s.", sub.Wanted, sub.Using.Name))
		}
		o := &offer{kind: "capture", label: src.Label(), ready: true, start: src.Start, stop: src.Stop, publishing: src.Publishing, trouble: src.Trouble, subs: src.Substitutions()}
		so := src.Options()
		o.shape = fmt.Sprintf("%s %d fps, %s", so.VideoSize, so.FPS, src.Encoder())
		o.adapt = &capture.Adapter{
			Ceiling: so.Bitrate, FPS: so.FPS, Stats: sink.Stats, Target: src, Logger: logger,
			Notify: func(line string) { ui.Notice(noticeInfo, line) },
		}
		o.save = cfg
		o.save.Camera = src.Camera().Name
		if m := src.Mic(); m != nil {
			o.save.Mic = m.Name
		} else {
			o.save.Mic = "none"
		}
		return o, nil
	}
}

// report is the offer as the reporter takes it (the camera's part of the
// sourceReport).
func (o *offer) report() sourceReport {
	s := sourceReport{Kind: o.kind, Label: o.label, Ready: o.ready, Note: o.note, Shape: o.shape}
	if o.kind == "camera" {
		s.URL = o.save.URL
	} else {
		s.Camera = o.save.Camera
		if o.save.Mic != "none" {
			s.Mic = o.save.Mic
		}
	}
	return s
}

// faceReport is the front-facing offer as the reporter takes it.
func (o *offer) faceReport() *faceReport {
	if o == nil {
		return nil
	}
	return &faceReport{Label: o.label, Camera: o.save.FaceCamera, Ready: o.ready, Note: o.note, Shape: o.shape}
}

// buildFaceOffer prepares the front-facing camera (cfg.FaceCamera): a
// second capture of the computer's, video only (the phone's microphone
// stays the session's), published into the face stream; nil when none is
// configured. Like buildOffer, a missing ffmpeg or camera is an offer that
// is not ready, so the connector still runs and the phone's own camera
// stays the face view.
func buildFaceOffer(ctx context.Context, cfg sourceConfig, sink capture.Sink, stats func() serve.Stats, logger *slog.Logger, ui reporter, saved bool) (*offer, error) {
	if cfg.FaceCamera == "" {
		return nil, nil
	}
	opts := capture.Options{Camera: cfg.FaceCamera, Mic: "none", VideoSize: cfg.FaceVideoSize, FPS: cfg.FaceFPS,
		Bitrate: cfg.FaceBitrate, Encoder: cfg.Encoder, FFmpeg: cfg.FFmpeg, Fallback: false}
	src, err := capture.New(ctx, sink, opts, logger)
	if err != nil {
		if capture.IsNoFFmpeg(err) || (errors.Is(err, capture.ErrNoDevice) && saved) {
			return &offer{kind: "capture", label: cfg.FaceCamera, note: err.Error(), save: cfg}, nil
		}
		return nil, fmt.Errorf("front-facing camera: %w", err)
	}
	o := &offer{kind: "capture", label: src.Camera().Name, ready: true, start: src.Start, stop: src.Stop, publishing: src.Publishing, trouble: src.Trouble}
	so := src.Options()
	o.shape = fmt.Sprintf("%s %d fps, %s", so.VideoSize, so.FPS, src.Encoder())
	o.adapt = &capture.Adapter{
		Ceiling: so.Bitrate, FPS: so.FPS, Stats: stats, Target: src, Logger: logger,
		Notify: func(line string) { ui.Notice(noticeInfo, "Front-facing camera: "+line) },
	}
	o.save = cfg
	o.save.FaceCamera = src.Camera().Name
	return o, nil
}

// listDevices is the `devices` subcommand.
func listDevices(ctx context.Context, ffmpegPath string) int {
	devs, err := deviceListing(ctx, ffmpegPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, kind := range []struct {
		k     capture.Kind
		title string
	}{{capture.Video, "Cameras"}, {capture.Audio, "Microphones"}} {
		fmt.Println(kind.title)
		n := 0
		for _, d := range devs {
			if d.Kind != kind.k {
				continue
			}
			fmt.Printf("  %d  %s\n", n, d.Name)
			n++
		}
		if n == 0 {
			fmt.Println("  (none found)")
		}
	}
	fmt.Println("\nChoose with, for example: masseuse-camlink -camera 0 -mic 0")
	fmt.Println("The choice is remembered; later starts need no flags.")
	if runtime.GOOS == "linux" {
		fmt.Println("Microphones are PulseAudio (or PipeWire) sources; pactl must be installed to list them.")
	}
	return 0
}

// cameraGrace is how long the camera stays on after its tunnel ends before
// it is turned off, in case the next tunnel opens a stream in the meantime.
// The service replaces the ticket on every new lease and the connector
// re-dials a tunnel that dropped, and each time the enclave's relay is back
// at the stream within seconds; turning the camera off and on again in
// between would put the capture back to zero just as the relay's DESCRIBE
// arrives, and its startup, several seconds, is the one thing the relay's
// patience does not cover. A session's end turns the camera off at once.
const cameraGrace = 15 * time.Second

// The two cameras the window's switch names (set_camera, ipc.go): the
// camera behind the person (the session's body view) and the front-facing
// camera (their face through OBS).
const (
	viewBody = "body"
	viewFace = "face"
)

// camControl turns the offer on at the first stream a session opens and
// off when the session ends, or when its tunnel has been down for
// cameraGrace with no stream opened since, and prints what is being sent.
// The person can also switch either camera off from the window
// (setEnabled): the capture stops at once, the light with it, and is not
// started again, whatever a session asks, until it is switched on. The
// switch is not remembered: every start has both cameras allowed.
type camControl struct {
	sink *serve.Server
	// offer is the camera served; guarded by mu, replaced by setOffer
	// while the camera is off (a set_source from the window).
	offer *offer
	// face is the front-facing camera, when one is configured: turned on
	// at the enclave's first DESCRIBE of its stream (startFace, through
	// serve.Stream.SetOnDemand) and off with the camera. Guarded by mu.
	face *offer
	log  *slog.Logger
	// ui is where what the camera does is said; nil is the console.
	ui reporter
	// grace is how long off(true) leaves the camera on; 0 means cameraGrace.
	grace time.Duration
	// onOff runs after the camera has gone off, outside mu: a change that
	// waited for the camera to be let go is applied then (manager).
	onOff func()

	mu         sync.Mutex
	on         bool
	cancel     context.CancelFunc
	faceOn     bool
	faceCancel context.CancelFunc
	// cameraOff and faceOff are the window's switches (setEnabled): while
	// set, the camera (the front-facing camera) is not started for a
	// session. Zero is allowed, so a camControl built plainly has both on.
	cameraOff, faceOff bool
	backlog            func() time.Duration // the active tunnel's, nil between tunnels
	// pending turns the camera off when the grace runs out; nil while none
	// is running. pendingGen tells a timer that fired whether it is still
	// the one that counts.
	pending    *time.Timer
	pendingGen uint64
}

func (c *camControl) reporter() reporter {
	if c.ui == nil {
		return stdConsole
	}
	return c.ui
}

// current is the offers as they stand.
func (c *camControl) current() (body, face *offer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.offer, c.face
}

// busy says whether the camera or the front-facing camera is on: an offer
// cannot be replaced while a session reads it.
func (c *camControl) busy() (camera, face bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.on, c.faceOn
}

// setOffer replaces the camera served; errCameraBusy while it is on.
func (c *camControl) setOffer(o *offer) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.on {
		return errCameraBusy
	}
	c.offer = o
	return nil
}

// setFace replaces the front-facing camera (nil for none); errCameraBusy
// while it is on.
func (c *camControl) setFace(o *offer) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.faceOn {
		return errCameraBusy
	}
	c.face = o
	return nil
}

// substitutions are the camera offer's (remembered devices not connected).
func (c *camControl) substitutions() []capture.Substitution {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.offer == nil {
		return nil
	}
	return c.offer.subs
}

// attach gives the stream the active tunnel's backlog (internal/tunnel,
// Tunnel.Backlog), by which it drops video when the connection falls
// behind; nil when the tunnel has ended.
func (c *camControl) attach(backlog func() time.Duration) {
	c.mu.Lock()
	c.backlog = backlog
	c.mu.Unlock()
	c.sink.SetBacklog(backlog)
}

func (c *camControl) currentBacklog() time.Duration {
	c.mu.Lock()
	fn := c.backlog
	c.mu.Unlock()
	if fn == nil {
		return 0
	}
	return fn()
}

// dialLocal answers the enclave's OPEN for the connector's own stream.
func (c *camControl) dialLocal() (net.Conn, error) {
	c.mu.Lock()
	if c.cameraOff {
		// The person switched the camera off in the window: the stream is
		// refused (the tunnel answers "camera unreachable", naming
		// nothing), and the relay's next attempt finds the camera again
		// once it is switched on.
		c.mu.Unlock()
		return nil, errors.New("the camera is switched off in the window")
	}
	if c.offer == nil || !c.offer.ready {
		note := "no camera configured"
		if c.offer != nil {
			note = c.offer.note
		}
		c.mu.Unlock()
		return nil, errors.New(note)
	}
	if c.pending != nil {
		// The next tunnel opened a stream within the grace: the camera
		// stays on, already publishing for it.
		c.pending.Stop()
		c.pending = nil
		c.log.Debug("camera: a stream came within the grace; staying on")
	}
	if !c.on {
		c.on = true
		ctx, cancel := context.WithCancel(context.Background())
		c.cancel = cancel
		c.offer.start()
		c.reporter().CameraOn(c.offer.label)
		go c.report(ctx, c.offer)
		if c.offer.adapt != nil {
			go c.offer.adapt.Run(ctx)
		}
	}
	c.mu.Unlock()
	return c.sink.Dial()
}

// startFace turns the front-facing camera on: the enclave asked for its
// stream (a DESCRIBE with no source publishing yet). It goes off with the
// camera (stopLocked). Nothing happens without one configured and ready,
// or while the person has it switched off in the window.
func (c *camControl) startFace() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.faceOff || c.face == nil || !c.face.ready || c.faceOn {
		return
	}
	c.faceOn = true
	ctx, cancel := context.WithCancel(context.Background())
	c.faceCancel = cancel
	c.face.start()
	c.reporter().FaceOn(c.face.label)
	if c.face.adapt != nil {
		go c.face.adapt.Run(ctx)
	}
}

// off turns the camera off if it is on: at once, or, with later, once the
// grace has passed with no stream opened (dialLocal) in the meantime. A
// grace already running is left to run.
func (c *camControl) off(later bool) {
	c.mu.Lock()
	if !c.on && !c.faceOn {
		c.mu.Unlock()
		return
	}
	if later {
		if c.pending == nil {
			grace := c.grace
			if grace == 0 {
				grace = cameraGrace
			}
			c.pendingGen++
			gen := c.pendingGen
			c.pending = time.AfterFunc(grace, func() { c.graceOver(gen) })
			c.log.Debug("camera: the tunnel ended; staying on for the next one", "grace", grace.String())
		}
		c.mu.Unlock()
		return
	}
	if c.pending != nil {
		c.pending.Stop()
		c.pending = nil
	}
	c.stopLocked()
}

// graceOver is the grace running out: the camera goes off unless a stream
// came in the meantime (dialLocal cleared the timer) or off(false) already
// turned it off.
func (c *camControl) graceOver(gen uint64) {
	c.mu.Lock()
	if c.pending == nil || c.pendingGen != gen || (!c.on && !c.faceOn) {
		c.mu.Unlock()
		return
	}
	c.pending = nil
	c.stopLocked()
}

// setEnabled is the window's switch on the camera (viewBody) or the
// front-facing camera (viewFace). Off stops the capture at once if a
// session has it on, so the light goes out, and dialLocal (startFace)
// refuses to start it again until it is on; on starts nothing by itself:
// the enclave's relay is back at the stream within seconds, and the next
// stream it opens brings the camera on as any does. The standing is said
// first, so the capture going off (CameraOff) already carries it, and the
// switch is the last word either way.
func (c *camControl) setEnabled(view string, enabled bool) error {
	c.mu.Lock()
	switch view {
	case viewBody:
		c.cameraOff = !enabled
		c.reporter().CameraEnabled(enabled)
		if !enabled && c.on {
			// A grace running for the next tunnel has nothing left to keep
			// on, unless the front-facing camera is still on: then it runs
			// on and takes that when it ends, as it would have both.
			if c.pending != nil && !c.faceOn {
				c.pending.Stop()
				c.pending = nil
			}
			c.stopBodyLocked() // releases mu
			return nil
		}
	case viewFace:
		c.faceOff = !enabled
		c.reporter().FaceEnabled(enabled)
		if !enabled && c.faceOn {
			c.stopFaceLocked() // releases mu
			return nil
		}
	default:
		c.mu.Unlock()
		return fmt.Errorf("no camera called %q; the cameras are %q and %q", view, viewBody, viewFace)
	}
	c.mu.Unlock()
	return nil
}

// enabled is the standing of the window's switches: whether the camera
// and the front-facing camera may be started for a session.
func (c *camControl) enabled() (camera, face bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.cameraOff, !c.faceOff
}

// stopLocked turns the camera off, and the front-facing camera with it;
// the caller holds c.mu, which is released before the sources are stopped
// (ffmpeg takes a moment to exit).
func (c *camControl) stopLocked() { c.stopSomeLocked(true, true) }

// stopBodyLocked turns the camera off alone (the window's switch), the
// front-facing camera staying as it is; the caller holds c.mu, released as
// by stopLocked.
func (c *camControl) stopBodyLocked() { c.stopSomeLocked(true, false) }

// stopFaceLocked turns the front-facing camera off alone; as stopBodyLocked.
func (c *camControl) stopFaceLocked() { c.stopSomeLocked(false, true) }

// stopSomeLocked turns off what it is told to of the camera and the
// front-facing camera, each if on; the caller holds c.mu, which is released
// before the sources are stopped (ffmpeg takes a moment to exit). A change
// that waited for the camera to be let go is applied after (onOff),
// whichever went off.
func (c *camControl) stopSomeLocked(stopBody, stopFace bool) {
	var (
		on, faceOn           bool
		cancel, faceCancel   context.CancelFunc
		bodyOffer, faceOffer *offer
	)
	if stopBody && c.on {
		on, cancel, bodyOffer = true, c.cancel, c.offer
		c.on, c.cancel = false, nil
	}
	if stopFace && c.faceOn {
		faceOn, faceCancel, faceOffer = true, c.faceCancel, c.face
		c.faceOn, c.faceCancel = false, nil
	}
	onOff := c.onOff
	c.mu.Unlock()
	if on {
		cancel()
		bodyOffer.stop()
		c.reporter().CameraOff()
	}
	if faceOn {
		faceCancel()
		faceOffer.stop()
		c.reporter().FaceOff()
	}
	if (on || faceOn) && onOff != nil {
		onOff()
	}
}

// report says the send rate every 10 s while the camera is on, and, in
// the same breath and so at most once per 10 s, that the connection is
// congested when video had to be dropped since the last report.
func (c *camControl) report(ctx context.Context, o *offer) {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	last := c.sink.Stats()
	lastAt := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		st := c.sink.Stats()
		now := time.Now()
		secs := now.Sub(lastAt).Seconds()
		if st.Publishing && last.Publishing && st.Readers > 0 && secs > 0 {
			video := float64(st.VideoBytes-last.VideoBytes) * 8 / secs
			audio := float64(st.AudioBytes-last.AudioBytes) * 8 / secs
			congested := st.Congested || st.VideoFramesDropped > last.VideoFramesDropped
			c.reporter().Sending(o.shape, video, audio, congested, c.currentBacklog())
		} else if !st.Publishing {
			reason := ""
			if o.trouble != nil {
				reason = o.trouble()
			}
			c.reporter().NotSending(reason)
		}
		last, lastAt = st, now
	}
}

func rate(bps float64) string {
	switch {
	case bps >= 1e6:
		return fmt.Sprintf("%.1f Mb/s", bps/1e6)
	case bps >= 1e3:
		return fmt.Sprintf("%.0f kb/s", bps/1e3)
	default:
		return fmt.Sprintf("%.0f b/s", bps)
	}
}
