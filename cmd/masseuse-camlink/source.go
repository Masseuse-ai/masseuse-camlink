package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
func buildOffer(ctx context.Context, cfg sourceConfig, stateDir string, sink *serve.Server, logger *slog.Logger, saved bool) (*offer, error) {
	switch cfg.Kind {
	case "camera":
		src, err := camera.New(ctx, sink, camera.Options{
			URL: cfg.URL, Fingerprint: cfg.Fingerprint, StateDir: stateDir, Logger: logger,
			OnTrust: func(host, fp string) {
				fmt.Printf("Trusting camera %s with certificate %s (saved; pass -camera-fingerprint to pin one yourself).\n", host, fp)
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
			fmt.Printf("%s is not connected; using %s.\n", sub.Wanted, sub.Using.Name)
		}
		o := &offer{kind: "capture", label: src.Label(), ready: true, start: src.Start, stop: src.Stop, publishing: src.Publishing, trouble: src.Trouble}
		so := src.Options()
		o.shape = fmt.Sprintf("%s %d fps, %s", so.VideoSize, so.FPS, src.Encoder())
		o.adapt = &capture.Adapter{
			Ceiling: so.Bitrate, FPS: so.FPS, Stats: sink.Stats, Target: src, Logger: logger,
			Notify: func(line string) { fmt.Println(line) },
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

// describe prints the offer at startup.
func (o *offer) describe() {
	if !o.ready {
		fmt.Printf("Camera: %s is not available: %s\n", o.label, o.note)
		return
	}
	fmt.Printf("Camera: %s (%s). It is on only while a session reads it.\n", o.label, o.shape)
}

// buildFaceOffer prepares the front-facing camera (cfg.FaceCamera): a
// second capture of the computer's, video only (the phone's microphone
// stays the session's), published into the face stream; nil when none is
// configured. Like buildOffer, a missing ffmpeg or camera is an offer that
// is not ready, so the connector still runs and the phone's own camera
// stays the face view.
func buildFaceOffer(ctx context.Context, cfg sourceConfig, sink capture.Sink, stats func() serve.Stats, logger *slog.Logger, saved bool) (*offer, error) {
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
		Notify: func(line string) { fmt.Println("Front-facing camera: " + line) },
	}
	o.save = cfg
	o.save.FaceCamera = src.Camera().Name
	return o, nil
}

// describeFace prints the front-facing camera at startup.
func (o *offer) describeFace() {
	if !o.ready {
		fmt.Printf("Front-facing camera: %s is not available: %s. Your phone's own camera stays your face.\n", o.label, o.note)
		return
	}
	fmt.Printf("Front-facing camera: %s (%s). It is on only while a session shows it as your face; until then your phone's own camera is.\n", o.label, o.shape)
}

// listDevices is the `devices` subcommand.
func listDevices(ctx context.Context, ffmpegPath string) int {
	ffmpeg, err := capture.FindFFmpeg(ffmpegPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v; %s\n", err, capture.InstallHint())
		return 1
	}
	devs, err := capture.Devices(ctx, ffmpeg)
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

// camControl turns the offer on at the first stream a session opens and
// off when the session ends, or when its tunnel has been down for
// cameraGrace with no stream opened since, and prints what is being sent.
type camControl struct {
	sink  *serve.Server
	offer *offer
	// face is the front-facing camera, when one is configured: turned on
	// at the enclave's first DESCRIBE of its stream (startFace, through
	// serve.Stream.SetOnDemand) and off with the camera.
	face *offer
	log  *slog.Logger
	// out is the console; nil means standard output.
	out io.Writer
	// grace is how long off(true) leaves the camera on; 0 means cameraGrace.
	grace time.Duration

	mu         sync.Mutex
	on         bool
	cancel     context.CancelFunc
	faceOn     bool
	faceCancel context.CancelFunc
	backlog    func() time.Duration // the active tunnel's, nil between tunnels
	// pending turns the camera off when the grace runs out; nil while none
	// is running. pendingGen tells a timer that fired whether it is still
	// the one that counts.
	pending    *time.Timer
	pendingGen uint64
}

func (c *camControl) printf(format string, args ...any) {
	w := c.out
	if w == nil {
		w = os.Stdout
	}
	fmt.Fprintf(w, format, args...)
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
	if c.offer == nil || !c.offer.ready {
		note := "no camera configured"
		if c.offer != nil {
			note = c.offer.note
		}
		return nil, errors.New(note)
	}
	c.mu.Lock()
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
		c.printf("Camera on: %s.\n", c.offer.label)
		go c.report(ctx)
		if c.offer.adapt != nil {
			go c.offer.adapt.Run(ctx)
		}
	}
	c.mu.Unlock()
	return c.sink.Dial()
}

// startFace turns the front-facing camera on: the enclave asked for its
// stream (a DESCRIBE with no source publishing yet). It goes off with the
// camera (stopLocked). Nothing happens without one configured and ready.
func (c *camControl) startFace() {
	if c.face == nil || !c.face.ready {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.faceOn {
		return
	}
	c.faceOn = true
	ctx, cancel := context.WithCancel(context.Background())
	c.faceCancel = cancel
	c.face.start()
	c.printf("Front-facing camera on: %s.\n", c.face.label)
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
	if c.pending == nil || c.pendingGen != gen || !c.on {
		c.mu.Unlock()
		return
	}
	c.pending = nil
	c.stopLocked()
}

// stopLocked turns the camera off, and the front-facing camera with it;
// the caller holds c.mu, which is released before the sources are stopped
// (ffmpeg takes a moment to exit).
func (c *camControl) stopLocked() {
	on, cancel := c.on, c.cancel
	c.on, c.cancel = false, nil
	faceOn, faceCancel := c.faceOn, c.faceCancel
	c.faceOn, c.faceCancel = false, nil
	c.mu.Unlock()
	if on {
		cancel()
		c.offer.stop()
		c.printf("Camera off.\n")
	}
	if faceOn {
		faceCancel()
		c.face.stop()
		c.printf("Front-facing camera off.\n")
	}
}

// report prints the send rate every 10 s while the camera is on, and, in
// the same breath and so at most once per 10 s, that the connection is
// congested when video had to be dropped since the last report.
func (c *camControl) report(ctx context.Context) {
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
			c.printf("Sending %s: video %s, audio %s\n", c.offer.shape, rate(video), rate(audio))
			if st.Congested || st.VideoFramesDropped > last.VideoFramesDropped {
				c.printf("Connection congested: dropping video to keep up (backlog %.1f s).\n", c.currentBacklog().Seconds())
			}
		} else if !st.Publishing {
			if c.offer.trouble != nil && c.offer.trouble() != "" {
				c.printf("Camera on but not sending yet: %s.\n", c.offer.trouble())
			} else {
				c.printf("Camera on but not sending yet (waiting for the source).\n")
			}
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
