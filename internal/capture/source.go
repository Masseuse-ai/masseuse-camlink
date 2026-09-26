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
	// list enumerates the devices and encoders reads `ffmpeg -encoders`;
	// Devices and the ffmpeg itself, or what a test puts in their place.
	list     func(ctx context.Context, ffmpeg string) ([]Device, error)
	encoders func(ctx context.Context, ffmpeg string) (string, error)

	mu sync.Mutex
	// cam, mic and subs are the devices in use; chosen at New and again
	// (rechoose) when an ffmpeg cannot open them.
	cam        Device
	mic        *Device
	subs       []Substitution
	cancel     context.CancelFunc
	done       chan struct{}
	encoder    string
	publishing bool
	// encoderList is `ffmpeg -encoders` once read (hasEncoder), and
	// noX264 that libx264 is not to be tried again: the build does not
	// have it, whatever the list said.
	encoderList *string
	noX264      bool
	// trouble is why the camera is on but not sending, for the console.
	trouble string
	// bitrate is the video bit rate in force: opts.Bitrate unless Reshape
	// lowered it.
	bitrate string
	// in is the intake while running; procCancel ends the current ffmpeg
	// and reshaping says that was asked for (Reshape) rather than a failure.
	in         *intake
	procCancel context.CancelFunc
	reshaping  bool
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
	cam, mic, subs, err := Resolve(devs, opts)
	if err != nil {
		return nil, err
	}
	s := newSource(sink, opts, logger, runtime.GOOS, ffmpeg, cam, mic)
	s.subs = subs
	return s, nil
}

func newSource(sink *serve.Server, opts Options, logger *slog.Logger, goos, ffmpeg string, cam Device, mic *Device) *Source {
	opts = opts.WithDefaults()
	enc := opts.Encoder
	if enc == EncoderAuto {
		enc = HardwareEncoder(goos)
	}
	return &Source{
		sink: sink, opts: opts, log: logger, goos: goos, ffmpeg: ffmpeg, cam: cam, mic: mic,
		list: Devices, encoders: listEncoders,
		encoder: enc, bitrate: opts.Bitrate, stopGrace: 3 * time.Second, earlyExit: 5 * time.Second,
	}
}

// Label names the devices, the way the phone shows them.
func (s *Source) Label() string {
	s.mu.Lock()
	cam, mic := s.cam, s.mic
	s.mu.Unlock()
	if mic == nil {
		return cam.Name + " (video only)"
	}
	if mic.Name == cam.Name {
		return cam.Name
	}
	return cam.Name + " + " + mic.Name
}

// Camera is the selected camera.
func (s *Source) Camera() Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cam
}

// Substitutions are the devices asked for that were not connected and what
// stands in for them (Options.Fallback); nil when every device was found.
func (s *Source) Substitutions() []Substitution {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subs
}

// Mic is the selected microphone, nil for video only.
func (s *Source) Mic() *Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mic
}

// Trouble is why the camera is on but nothing is being sent, in a sentence
// for the person, or "" while it is sending or has not failed yet.
func (s *Source) Trouble() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.trouble
}

func (s *Source) setTrouble(t string) {
	s.mu.Lock()
	s.trouble = t
	s.mu.Unlock()
}

// Options are the configured shaping options; Options.Bitrate is the
// ceiling, Bitrate the rate in force.
func (s *Source) Options() Options { return s.opts }

// Bitrate is the video bit rate in force: the configured one unless
// Reshape lowered it.
func (s *Source) Bitrate() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bitrate
}

// Encoder is the encoder in use (after any fallback).
func (s *Source) Encoder() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.encoder
}

// Reshape changes the video bit rate. While the camera is on, ffmpeg is
// restarted at the new rate without the readers noticing: the stream keeps
// the publication open for the replacement (serve.Publication.ArmHandover)
// and the next ffmpeg joins it, sequence numbers and timestamps carrying
// on. While idle only the rate for the next start changes. It returns
// whether a running encoder was restarted.
func (s *Source) Reshape(bitrate string) bool {
	s.mu.Lock()
	if bitrate == "" || bitrate == s.bitrate {
		s.mu.Unlock()
		return false
	}
	s.bitrate = bitrate
	in, procCancel := s.in, s.procCancel
	if procCancel != nil {
		s.reshaping = true
	}
	s.mu.Unlock()
	if in == nil || procCancel == nil {
		return false
	}
	in.armHandover(serve.DefaultHandoverGrace)
	s.log.Info("capture: restarting the encoder at another bit rate", "bitrate", bitrate)
	procCancel()
	return true
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

// Stop turns the camera off and waits for ffmpeg to exit. The next Start
// is at the configured bit rate again, whatever Reshape lowered it to.
func (s *Source) Stop() {
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.cancel, s.done = nil, nil
	s.bitrate = s.opts.Bitrate
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
	if v {
		s.trouble = ""
	}
	cam, enc := s.cam, s.encoder
	s.mu.Unlock()
	if v {
		s.log.Info("capture: camera on", "camera", cam.Name, "encoder", enc)
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
	s.mu.Lock()
	s.in = in
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.in, s.procCancel, s.reshaping = nil, nil, false
		s.mu.Unlock()
	}()
	backoff := time.Second
	for {
		started := time.Now()
		// Each ffmpeg has its own context so Reshape can end one without
		// turning the camera off.
		pctx, pcancel := context.WithCancel(ctx)
		s.mu.Lock()
		s.procCancel = pcancel
		s.mu.Unlock()
		err := s.runFFmpeg(pctx, in.URL())
		pcancel()
		s.mu.Lock()
		s.procCancel = nil
		reshaped := s.reshaping
		s.reshaping = false
		s.mu.Unlock()
		if ctx.Err() != nil {
			s.log.Info("capture: camera off")
			return
		}
		if reshaped {
			// Asked for: start the next one now, at the new rate.
			continue
		}
		if time.Since(started) >= s.earlyExit {
			// It ran and broke: start over soon.
			backoff = time.Second
			s.log.Warn("capture: ffmpeg exited; restarting", "err", err, "in", backoff)
			if !s.pause(ctx, backoff) {
				return
			}
			continue
		}
		// It did not get going. What ffmpeg wrote says why, and the
		// remedy differs: a camera or microphone it could not open is
		// chosen again (the list may have been renumbered, or the device
		// gone), an encoder that would not start gives way to libx264
		// when this ffmpeg has it, and an option it does not know means
		// the encoder in use is not in this build at all.
		switch kind := classifyExit(err); kind {
		case exitInput:
			s.setTrouble("the camera or microphone could not be opened; choosing the devices again")
			s.log.Warn("capture: ffmpeg could not open the camera or microphone; choosing the devices again", "err", err)
			s.rechoose(ctx)
		case exitOption:
			if s.revertEncoder(err) {
				continue
			}
			s.setTrouble("ffmpeg does not know an option it was given")
		default:
			if s.fallBack(ctx, err) {
				continue
			}
			if kind == exitEncoder {
				s.setTrouble("the video encoder failed to start")
			} else {
				s.setTrouble("ffmpeg stopped as soon as it started")
			}
		}
		s.log.Warn("capture: ffmpeg exited; restarting", "err", err, "in", backoff)
		if !s.pause(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, 15*time.Second)
	}
}

// pause waits for d or until ctx ends, reporting whether to go on.
func (s *Source) pause(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		s.log.Info("capture: camera off")
		return false
	}
}

// rechoose lists the devices again and picks by the configured selectors,
// for an ffmpeg that could not open its input. On macOS the list is
// renumbered when a device comes or goes (a phone through Continuity
// Camera, most often); a device addressed by name (Device.Input) rides
// that out, one addressed by index does not, and a device may simply be
// gone, in which case the default or the fallback stands in as at New.
// When no choice can be made the devices stay as they are, for the retry.
func (s *Source) rechoose(ctx context.Context) {
	devs, err := s.list(ctx, s.ffmpeg)
	if err != nil {
		s.log.Warn("capture: cannot list the devices", "err", err)
		return
	}
	cam, mic, subs, err := Resolve(devs, s.opts)
	if err != nil {
		s.log.Warn("capture: the devices are not all connected", "err", err)
		s.setTrouble("the camera or microphone is not connected: " + err.Error())
		return
	}
	s.mu.Lock()
	changed := !sameDevice(cam, s.cam) || (mic == nil) != (s.mic == nil) || (mic != nil && !sameDevice(*mic, *s.mic))
	s.cam, s.mic, s.subs = cam, mic, subs
	s.mu.Unlock()
	if changed {
		micName := "none"
		if mic != nil {
			micName = mic.Name
		}
		s.log.Info("capture: devices chosen again", "camera", cam.Name, "cameraInput", cam.input(), "mic", micName)
	}
}

func sameDevice(a, b Device) bool { return a.Kind == b.Kind && a.ID == b.ID && a.Name == b.Name }

// fallBack moves an automatically chosen hardware encoder that would not
// start to libx264, when this ffmpeg has it: the one shipped with the
// connector is built without GPL parts and so without libx264, and a
// fallback to an encoder that is not there fails at once on its own
// options, forever. It reports whether the encoder was changed.
func (s *Source) fallBack(ctx context.Context, cause error) bool {
	s.mu.Lock()
	enc, auto, no := s.encoder, s.opts.Encoder == EncoderAuto, s.noX264
	s.mu.Unlock()
	if !auto || enc == EncoderX264 || no {
		return false
	}
	if !s.hasEncoder(ctx, EncoderX264) {
		s.log.Warn("capture: the encoder failed to start, and this ffmpeg has no libx264 to fall back to; retrying it", "encoder", enc, "err", cause)
		return false
	}
	s.mu.Lock()
	s.encoder = EncoderX264
	s.mu.Unlock()
	s.log.Warn("capture: hardware encoder failed to start; using libx264", "encoder", enc, "err", cause)
	return true
}

// revertEncoder undoes a fallback to libx264 that ffmpeg did not know the
// options of (it does not have the encoder), going back to the hardware
// encoder and not trying libx264 again. It reports whether it did.
func (s *Source) revertEncoder(cause error) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	hw := HardwareEncoder(s.goos)
	if s.opts.Encoder != EncoderAuto || s.encoder != EncoderX264 || hw == EncoderX264 {
		return false
	}
	s.encoder, s.noX264 = hw, true
	s.log.Warn("capture: this ffmpeg does not have libx264 after all; back to the hardware encoder", "encoder", hw, "err", cause)
	return true
}

// hasEncoder reports whether this ffmpeg build has the named encoder,
// reading `ffmpeg -encoders` the first time it is asked. An ffmpeg that
// cannot be asked is taken not to have it, and asked again next time.
func (s *Source) hasEncoder(ctx context.Context, name string) bool {
	s.mu.Lock()
	list := s.encoderList
	s.mu.Unlock()
	if list == nil {
		out, err := s.encoders(ctx, s.ffmpeg)
		if err != nil {
			s.log.Warn("capture: cannot list ffmpeg's encoders", "err", err)
			return false
		}
		list = &out
		s.mu.Lock()
		s.encoderList = list
		s.mu.Unlock()
	}
	return listsEncoder(*list, name)
}

// listEncoders runs `ffmpeg -encoders`.
func listEncoders(ctx context.Context, ffmpeg string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-encoders").Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// listsEncoder reads `ffmpeg -encoders` for name: after the legend, one
// line per encoder, its six capability flags then its name
// (" V....D libx264              libx264 H.264 / AVC ...").
func listsEncoder(out, name string) bool {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && len(f[0]) == 6 && f[1] == name {
			return true
		}
	}
	return false
}

// exitKind is what an ffmpeg that exited at once said was wrong.
type exitKind int

const (
	exitUnknown exitKind = iota
	// exitInput: the camera or microphone could not be opened.
	exitInput
	// exitEncoder: the encoder could not be opened.
	exitEncoder
	// exitOption: an option ffmpeg does not know, which for the options
	// the connector passes means the encoder they belong to is not in
	// this build.
	exitOption
)

// classifyExit reads the lines an ffmpeg that exited at once wrote (the
// error from runFFmpeg carries the last of them).
func classifyExit(err error) exitKind {
	if err == nil {
		return exitUnknown
	}
	msg := strings.ToLower(err.Error())
	has := func(parts ...string) bool {
		for _, p := range parts {
			if strings.Contains(msg, p) {
				return true
			}
		}
		return false
	}
	switch {
	case has("unrecognized option", "unknown encoder", "option not found", "codec not found"):
		return exitOption
	case has("error while opening encoder", "error opening encoder", "cannot create compression session"):
		return exitEncoder
	case has("error opening input", "device index", "no such device", "could not find video device",
		"could not find audio device", "device not found", "cannot open video device", "cannot open audio device"):
		return exitInput
	case has("encoder", "videotoolbox", "mediafoundation", "h264_mf"):
		return exitEncoder
	}
	return exitUnknown
}

// runFFmpeg runs one ffmpeg until it exits or ctx ends; the error carries
// the last lines it wrote.
func (s *Source) runFFmpeg(ctx context.Context, url string) error {
	s.mu.Lock()
	opts, enc := s.opts, s.encoder
	opts.Bitrate = s.bitrate
	cam, mic := s.cam, s.mic
	s.mu.Unlock()
	args := Args(s.goos, opts, cam, mic, enc, url)
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
