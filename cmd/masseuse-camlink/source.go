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

// sourceConfig is the saved choice.
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
}

// sourceFlags are the command-line choices.
type sourceFlags struct {
	camera, mic, videoSize, bitrate, encoder, ffmpeg string
	fps                                              int
	cameraURL, cameraFingerprint                     string
}

func (f sourceFlags) any() bool {
	return f.camera != "" || f.mic != "" || f.videoSize != "" || f.bitrate != "" || f.encoder != "" ||
		f.ffmpeg != "" || f.fps != 0 || f.cameraURL != "" || f.cameraFingerprint != ""
}

// resolveSourceConfig turns flags into the configuration to use: flags when
// any is given (and saved), else the saved one, else the computer's first
// camera and microphone.
func resolveSourceConfig(stateDir string, f sourceFlags) (sourceConfig, error) {
	if f.any() {
		if f.cameraURL != "" {
			if f.camera != "" || f.mic != "" || f.videoSize != "" || f.bitrate != "" || f.encoder != "" || f.ffmpeg != "" || f.fps != 0 {
				return sourceConfig{}, errors.New("-camera-url names a camera on your network; the -camera, -mic and encoding flags are for the computer's own camera and cannot be combined with it")
			}
			return sourceConfig{Kind: "camera", URL: f.cameraURL, Fingerprint: f.cameraFingerprint}, nil
		}
		if f.cameraFingerprint != "" {
			return sourceConfig{}, errors.New("-camera-fingerprint goes with -camera-url")
		}
		return sourceConfig{Kind: "capture", Camera: f.camera, Mic: f.mic, VideoSize: f.videoSize, FPS: f.fps,
			Bitrate: f.bitrate, Encoder: f.encoder, FFmpeg: f.ffmpeg}, nil
	}
	b, err := os.ReadFile(filepath.Join(stateDir, sourceFile))
	if err == nil {
		var cfg sourceConfig
		if json.Unmarshal(b, &cfg) == nil && (cfg.Kind == "capture" || cfg.Kind == "camera") {
			return cfg, nil
		}
	}
	return sourceConfig{Kind: "capture"}, nil
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
		o := &offer{kind: "capture", label: src.Label(), ready: true, start: src.Start, stop: src.Stop, publishing: src.Publishing}
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

// camControl turns the offer on at the first stream a session opens and
// off when the session's tunnel ends, and prints what is being sent.
type camControl struct {
	sink  *serve.Server
	offer *offer
	log   *slog.Logger
	// out is the console; nil means standard output.
	out io.Writer

	mu      sync.Mutex
	on      bool
	cancel  context.CancelFunc
	backlog func() time.Duration // the active tunnel's, nil between tunnels
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

// off turns the camera off if it is on.
func (c *camControl) off() {
	c.mu.Lock()
	on := c.on
	c.on = false
	cancel := c.cancel
	c.cancel = nil
	c.mu.Unlock()
	if !on {
		return
	}
	cancel()
	c.offer.stop()
	c.printf("Camera off.\n")
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
			c.printf("Camera on but not sending yet (waiting for the source).\n")
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
