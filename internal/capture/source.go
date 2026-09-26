// Package capture publishes the computer's own camera and microphone into
// the connector's stream (internal/serve) using ffmpeg.
//
// ffmpeg is the one program that opens cameras and microphones on macOS
// (avfoundation), Windows (dshow) and Linux (v4l2 and PulseAudio) alike and
// encodes with whatever hardware the computer has. The connector runs it as
// a child only while a session is reading, so the camera light is off
// otherwise, and receives its RTSP output on a loopback port nobody else
// knows the path of. What ffmpeg produces is forwarded as it is; nothing is
// decoded or looked at here.
package capture

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/serve"
)

// Source is the configured capture, idle until Start.
type Source struct {
	sink   *serve.Server
	opts   Options
	log    *slog.Logger
	goos   string
	ffmpeg string
	cam    Device
	mic    *Device

	mu         sync.Mutex
	cancel     context.CancelFunc
	done       chan struct{}
	encoder    string
	publishing bool
	// stopGrace bounds how long Stop waits for ffmpeg to quit on its own.
	stopGrace time.Duration
	// earlyExit is how soon an exit counts as "failed to start" (encoder
	// fallback, growing backoff) rather than a stream that ran and broke.
	earlyExit time.Duration
}

// New resolves ffmpeg and the devices now, so a wrong selector or a
// missing ffmpeg is reported at startup rather than at the first session,
// and returns the source idle.
func New(ctx context.Context, sink *serve.Server, opts Options, logger *slog.Logger) (*Source, error) {
	if logger == nil {
		logger = slog.Default()
	}
	opts = opts.WithDefaults()
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	ffmpeg, err := FindFFmpeg(opts.FFmpeg)
	if err != nil {
		return nil, fmt.Errorf("%w; %s", err, InstallHint())
	}
	devs, err := Devices(ctx, ffmpeg)
	if err != nil {
		return nil, err
	}
	cam, err := Select(devs, Video, opts.Camera)
	if err != nil {
		return nil, err
	}
	var mic *Device
	if !strings.EqualFold(strings.TrimSpace(opts.Mic), "none") {
		m, err := Select(devs, Audio, opts.Mic)
		if err != nil {
			return nil, err
		}
		mic = &m
	}
	return newSource(sink, opts, logger, runtime.GOOS, ffmpeg, cam, mic), nil
}

func newSource(sink *serve.Server, opts Options, logger *slog.Logger, goos, ffmpeg string, cam Device, mic *Device) *Source {
	opts = opts.WithDefaults()
	enc := opts.Encoder
	if enc == EncoderAuto {
		enc = HardwareEncoder(goos)
	}
	return &Source{
		sink: sink, opts: opts, log: logger, goos: goos, ffmpeg: ffmpeg, cam: cam, mic: mic,
		encoder: enc, stopGrace: 3 * time.Second, earlyExit: 5 * time.Second,
	}
}

// Label names the devices, the way the phone shows them.
func (s *Source) Label() string {
	if s.mic == nil {
		return s.cam.Name + " (video only)"
	}
	if s.mic.Name == s.cam.Name {
		return s.cam.Name
	}
	return s.cam.Name + " + " + s.mic.Name
}

// Camera is the selected camera.
func (s *Source) Camera() Device { return s.cam }

// Mic is the selected microphone, nil for video only.
func (s *Source) Mic() *Device { return s.mic }

// Options are the shaping options in force.
func (s *Source) Options() Options { return s.opts }

// Encoder is the encoder in use (after any fallback).
func (s *Source) Encoder() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.encoder
}

// Running reports whether Start has been called and Stop has not.
func (s *Source) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancel != nil
}

// Publishing reports whether ffmpeg is connected and publishing.
func (s *Source) Publishing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.publishing
}

// Start turns the camera on: ffmpeg is started and kept running (restarted
// with backoff if it exits) until Stop. Calling it while running does
// nothing.
func (s *Source) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.done = make(chan struct{})
	go s.run(ctx, s.done)
}

// Stop turns the camera off and waits for ffmpeg to exit.
func (s *Source) Stop() {
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.cancel, s.done = nil, nil
	s.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

func (s *Source) setPublishing(v bool) {
	s.mu.Lock()
	s.publishing = v
	s.mu.Unlock()
	if v {
		s.log.Info("capture: camera on", "camera", s.cam.Name, "encoder", s.Encoder())
	}
}

func (s *Source) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	in, err := newIntake(s.sink, s.log, s.setPublishing)
	if err != nil {
		s.log.Error("capture: cannot start", "err", err)
		return
	}
	defer in.Close()
	defer s.setPublishing(false)
	backoff := time.Second
	for {
		started := time.Now()
		err := s.runFFmpeg(ctx, in.URL())
		if ctx.Err() != nil {
			s.log.Info("capture: camera off")
			return
		}
		early := time.Since(started) < s.earlyExit
		s.mu.Lock()
		enc := s.encoder
		fallback := early && s.opts.Encoder == EncoderAuto && enc != EncoderX264
		if fallback {
			s.encoder = EncoderX264
		}
		s.mu.Unlock()
		if fallback {
			s.log.Warn("capture: hardware encoder failed to start; using libx264", "encoder", enc, "err", err)
			continue
		}
		if early {
			backoff = min(backoff*2, 15*time.Second)
		} else {
			backoff = time.Second
		}
		s.log.Warn("capture: ffmpeg exited; restarting", "err", err, "in", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			s.log.Info("capture: camera off")
			return
		}
	}
}

// runFFmpeg runs one ffmpeg until it exits or ctx ends; the error carries
// the last lines it wrote.
func (s *Source) runFFmpeg(ctx context.Context, url string) error {
	args := Args(s.goos, s.opts, s.cam, s.mic, s.Encoder(), url)
	cmd := exec.Command(s.ffmpeg, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	cmd.Stdout = io.Discard
	s.log.Debug("capture: starting ffmpeg", "args", strings.Join(args, " "))
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}
	tail := &tailBuffer{n: 12}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64<<10), 64<<10)
		for sc.Scan() {
			line := sc.Text()
			tail.add(line)
			s.log.Debug("ffmpeg", "line", line)
		}
	}()
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	select {
	case err := <-waitErr:
		<-readDone
		return fmt.Errorf("ffmpeg: %v: %s", err, tail.String())
	case <-ctx.Done():
	}
	// Ask ffmpeg to finish cleanly (it reads "q" on stdin on every system),
	// then insist.
	_, _ = io.WriteString(stdin, "q\n")
	_ = stdin.Close()
	select {
	case <-waitErr:
	case <-time.After(s.stopGrace):
		_ = cmd.Process.Kill()
		<-waitErr
	}
	<-readDone
	return ctx.Err()
}

// tailBuffer keeps the last n lines.
type tailBuffer struct {
	mu    sync.Mutex
	n     int
	lines []string
}

func (t *tailBuffer) add(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, line)
	if len(t.lines) > t.n {
		t.lines = t.lines[len(t.lines)-t.n:]
	}
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.lines, " | ")
}

// IsNoFFmpeg reports whether err means ffmpeg is missing.
func IsNoFFmpeg(err error) bool { return errors.Is(err, ErrNoFFmpeg) }
