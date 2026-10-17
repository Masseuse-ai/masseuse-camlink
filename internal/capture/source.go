// Package capture publishes the computer's own camera and microphone into
// the connector's stream (internal/serve) using ffmpeg.
//
// ffmpeg is the one program that opens cameras and microphones on macOS
// (avfoundation), Windows (dshow) and Linux (v4l2 and PulseAudio) alike and
// encodes with whatever hardware the computer has. The connector runs it as
// a child while a session is reading, and receives its RTSP output on a
// loopback port nobody else knows the path of; and, while no session has
// the camera, for the desktop window's preview (preview.go), whose small
// pictures go to the window alone. Otherwise the camera light is off. What
// ffmpeg produces is forwarded as it is; nothing is decoded or looked at
// here.
//
// The camera and the microphone are one ffmpeg input, so a microphone that
// is missing, refused or silent could hold back the picture with it. It
// never does: one that is not connected gives way to another or to none
// (Resolve, rechoose), one the system refuses is left out (SetMicRefused),
// and a capture that does not get going with it is tried without it
// (micTrial). Standing says what is in use.
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

	"github.com/Masseuse-ai/masseuse-camlink/internal/serve"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
)

// A Sink is the stream a capture publishes into: the connector's camera
// stream (*serve.Server, the body view) or its face stream (*serve.Stream).
type Sink interface {
	Publish(desc *description.Session) (*serve.Publication, error)
}

// Source is the configured capture, idle until Start.
type Source struct {
	sink Sink
	// conf is the options as configured, a zero field the camera's own;
	// opts the options in force, the camera's mode (shape) and the
	// defaults filled in.
	conf   Options
	opts   Options
	log    *slog.Logger
	goos   string
	ffmpeg string
	// list enumerates the devices, modes reads a camera's modes and
	// encoders reads `ffmpeg -encoders`; Devices, cameraModes and the
	// ffmpeg itself, or what a test puts in their place.
	list     func(ctx context.Context, ffmpeg string) ([]Device, error)
	modes    func(ctx context.Context, goos, ffmpeg string, cam Device) ([]Mode, error)
	encoders func(ctx context.Context, ffmpeg string) (string, error)

	mu sync.Mutex
	// cam, mic and subs are the devices in use; chosen at New and again
	// (rechoose) when an ffmpeg cannot open them. input is how the camera
	// is opened, as its mode says (shape).
	cam        Device
	mic        *Device
	subs       []Substitution
	input      Input
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
	// in is the intake while running; procCancel ends the current ffmpeg,
	// and reshaping (Reshape) or retrying (stalled, without the microphone)
	// says that was asked for rather than a failure.
	in         *intake
	procCancel context.CancelFunc
	reshaping  bool
	retrying   bool
	// micRefused says the system refuses the microphone to this program
	// (SetMicRefused); runRefused is what the capture under way started
	// with.
	micRefused, runRefused bool
	// trial is how the microphone fares in the capture under way.
	trial micTrial
	// micInFFmpeg says the latest ffmpeg has the microphone in it: the
	// one that replaces it under the readers (Reshape) must publish the
	// same tracks, whatever has been learnt since.
	micInFFmpeg bool
	// stopGrace bounds how long Stop waits for ffmpeg to quit on its own.
	stopGrace time.Duration
	// earlyExit is how soon an exit counts as "failed to start" (encoder
	// fallback, growing backoff) rather than a stream that ran and broke.
	earlyExit time.Duration
	// announceGrace is how long a running ffmpeg may go without publishing
	// before trouble says the camera delivered no picture (stalled). An
	// ffmpeg that exits says why; one the system refuses the camera does
	// not: on a Mac the avfoundation input waits for a first frame that
	// never comes, without a word, for as long as it is left running.
	announceGrace time.Duration
}

// micTrial is how the microphone fares in a capture. One with it that
// does not get going (it exits at once, or runs for the announce grace
// without publishing) is started again without it, once: if that one gets
// going, the microphone was the trouble and the picture goes alone until
// the next Start; if not, the trouble is the camera's and the microphone
// goes back in. A microphone that never sends a sample holds back the
// picture with it: on a Mac the avfoundation input carries on past a
// microphone it could not open (one the system refuses) and then waits
// for its first sample for as long as it is left running, and ffmpeg
// publishes nothing until every stream has started.
type micTrial int

const (
	micIn     micTrial = iota // in the capture, or none is chosen
	micTrying                 // left out of this attempt, to see whether the camera alone gets going
	micOut                    // left out: the camera alone got going
	micBack                   // in again: the camera alone did not get going either
)

// MicOut is why the microphone chosen is left out of the capture.
type MicOut string

const (
	// MicRefused: the system refuses it to this program (SetMicRefused).
	MicRefused MicOut = "refused"
	// MicFailed: a capture with it did not get going and one without it
	// did; it is tried again at the next Start.
	MicFailed MicOut = "failed"
)

// Standing is what the capture uses, and how that differs from the choice.
type Standing struct {
	// Label names the devices, the way the phone shows them (Label).
	Label  string
	Camera Device
	// Mic is the microphone chosen or standing in, nil for none; MicOut,
	// when set, is why it is left out of the capture.
	Mic    *Device
	MicOut MicOut
	// Substitutions are the devices asked for that are not connected and
	// what stands in for them (Substitutions).
	Substitutions []Substitution
}

// New resolves ffmpeg, the devices and the camera's mode now, so a wrong
// selector or a missing ffmpeg is reported at startup rather than at the
// first session, and returns the source idle.
func New(ctx context.Context, sink Sink, opts Options, logger *slog.Logger) (*Source, error) {
	if logger == nil {
		logger = slog.Default()
	}
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
	s.shape(ctx)
	return s, nil
}

func newSource(sink Sink, conf Options, logger *slog.Logger, goos, ffmpeg string, cam Device, mic *Device) *Source {
	opts := conf.WithDefaults()
	enc := opts.Encoder
	if enc == EncoderAuto {
		enc = HardwareEncoder(goos)
	}
	return &Source{
		sink: sink, conf: conf, opts: opts, log: logger, goos: goos, ffmpeg: ffmpeg, cam: cam, mic: mic,
		list: Devices, modes: cameraModes, encoders: listEncoders,
		encoder: enc, bitrate: opts.Bitrate, stopGrace: 3 * time.Second, earlyExit: 5 * time.Second,
		announceGrace: 10 * time.Second,
	}
}

// shape reads the camera's modes and settles the picture: the mode chosen
// (choose) gives the size and rate not configured and how the camera is
// opened, and the bit rate ceiling, unless configured, is the picture's.
// A camera whose modes cannot be read, or none of whose modes will do, is
// opened as configured, at Defaults for what is not. The rate in force
// goes back to the ceiling when the ceiling changes.
func (s *Source) shape(ctx context.Context) {
	s.mu.Lock()
	cam, conf := s.cam, s.conf
	s.mu.Unlock()
	var choice *Choice
	if s.modes != nil {
		modes, err := s.modes(ctx, s.goos, s.ffmpeg, cam)
		switch {
		case err != nil:
			s.log.Warn("capture: cannot read the camera's modes; opening it as configured", "camera", cam.Name, "err", err)
		case len(modes) == 0:
			s.log.Debug("capture: the camera lists no modes; opening it as configured", "camera", cam.Name)
		default:
			if c, ok := choose(s.goos, modes, conf); ok {
				choice = &c
				s.log.Info("capture: camera mode", "camera", cam.Name, "mode", c.String(), "offers", describeModes(modes))
			} else {
				s.log.Warn("capture: no mode of the camera's will do; opening it as configured", "camera", cam.Name, "offers", describeModes(modes))
			}
		}
	}
	opts := conf
	var in Input
	if choice != nil {
		if opts.VideoSize == "" {
			opts.VideoSize = choice.Size()
		}
		if opts.FPS == 0 {
			opts.FPS = choice.FPS
		}
		in = choice.Input
	}
	opts = opts.WithDefaults()
	s.mu.Lock()
	if opts.Bitrate != s.opts.Bitrate {
		s.bitrate = opts.Bitrate
	}
	s.opts, s.input = opts, in
	s.mu.Unlock()
}

// Label names the devices, the way the phone shows them.
func (s *Source) Label() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.labelLocked()
}

func (s *Source) labelLocked() string {
	if s.mic == nil {
		return s.cam.Name + " (video only)"
	}
	if s.mic.Name == s.cam.Name {
		return s.cam.Name
	}
	return s.cam.Name + " + " + s.mic.Name
}

// Standing is the capture's devices as they stand: what is used, what
// stands in for what, and whether the microphone is left out and why (the
// system's refusal as the capture under way started with it, or between
// captures as the next will; a microphone that failed the last capture,
// until the next Start).
func (s *Source) Standing() Standing {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Standing{Label: s.labelLocked(), Camera: s.cam, Substitutions: append([]Substitution(nil), s.subs...)}
	if s.mic != nil {
		m := *s.mic
		st.Mic = &m
		running := s.cancel != nil
		switch {
		case running && s.runRefused, !running && s.micRefused:
			st.MicOut = MicRefused
		case s.trial == micOut:
			st.MicOut = MicFailed
		}
	}
	return st
}

// SetMicRefused says whether the system refuses the microphone to this
// program. While it does, a capture that starts leaves the microphone out
// and sends the picture alone, rather than waiting on a microphone that
// never delivers (micTrial). A capture under way keeps what it started
// with; the next follows the standing.
func (s *Source) SetMicRefused(refused bool) {
	s.mu.Lock()
	s.micRefused = refused
	s.mu.Unlock()
}

// leaveOutMicLocked says whether the next ffmpeg goes without the
// microphone chosen; the caller holds s.mu.
func (s *Source) leaveOutMicLocked() bool {
	return s.runRefused || s.trial == micTrying || s.trial == micOut
}

// tryAloneLocked starts the microphone's trial (micTrial) when the capture
// that did not get going had one in it and none has been tried this run:
// the next ffmpeg goes without it. It reports whether it did; the caller
// holds s.mu.
func (s *Source) tryAloneLocked() bool {
	if s.mic == nil || s.trial != micIn || s.leaveOutMicLocked() {
		return false
	}
	s.trial = micTrying
	return true
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

// Options are the shaping options in force: the configured ones, with the
// camera's mode and the defaults for what was not configured.
// Options.Bitrate is the ceiling, Bitrate the rate in force.
func (s *Source) Options() Options {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opts
}

// Input is how the camera is opened beyond its size, as its mode says.
func (s *Source) Input() Input {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.input
}

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
// nothing. A start is a fresh attempt: what went wrong the last time the
// camera was on is not said of this one, a microphone that failed then is
// tried again, and devices that stood in for others give way to them if
// they are back (run).
func (s *Source) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return
	}
	s.trouble = ""
	s.trial, s.runRefused = micIn, s.micRefused
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
	alone := false
	if v {
		s.trouble = ""
		if s.trial == micTrying {
			s.trial, alone = micOut, true
		}
	}
	cam, mic, enc := s.cam, s.mic, s.encoder
	s.mu.Unlock()
	if v {
		s.log.Info("capture: camera on", "camera", cam.Name, "encoder", enc)
	}
	if alone && mic != nil {
		s.log.Warn("capture: the camera alone got going; the microphone stays out until the camera next comes on", "mic", mic.Name)
	}
}

// degraded says the devices in use are not all the ones chosen: something
// stands in for a device that is not connected, or no microphone was for a
// choice that wants one.
func (s *Source) degraded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs) > 0 || (s.mic == nil && !strings.EqualFold(strings.TrimSpace(s.conf.Mic), "none"))
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
		s.in, s.procCancel, s.reshaping, s.retrying = nil, nil, false, false
		s.mu.Unlock()
	}()
	// What stood in last time gives way to the devices chosen if they are
	// back: one listing, only while something stands in.
	if s.degraded() {
		s.rechoose(ctx)
	}
	backoff := time.Second
	// handover says the next ffmpeg replaces one under the readers.
	handover := false
	for {
		started := time.Now()
		// Each ffmpeg has its own context so Reshape can end one without
		// turning the camera off.
		pctx, pcancel := context.WithCancel(ctx)
		s.mu.Lock()
		s.procCancel = pcancel
		s.mu.Unlock()
		// An ffmpeg that runs on without publishing is said so of after
		// the announce grace (stalled); one that exits is read below.
		stall := time.AfterFunc(s.announceGrace, func() { s.stalled(pctx) })
		err := s.runFFmpeg(pctx, in.URL(), handover)
		stall.Stop()
		pcancel()
		s.mu.Lock()
		s.procCancel = nil
		reshaped, retried := s.reshaping, s.retrying
		s.reshaping, s.retrying = false, false
		s.mu.Unlock()
		if ctx.Err() != nil {
			s.log.Info("capture: camera off")
			return
		}
		if reshaped || retried {
			// Asked for: start the next one now, at the new rate or
			// without the microphone.
			handover = reshaped && !retried
			continue
		}
		handover = false
		// The camera alone, tried without the microphone, ended without
		// publishing (which would have settled the trial): the trouble is
		// the camera's, and the microphone goes back in.
		s.mu.Lock()
		backIn := s.trial == micTrying
		if backIn {
			s.trial = micBack
		}
		s.mu.Unlock()
		if backIn {
			s.log.Warn("capture: the camera alone did not get going either; the microphone goes back in", "err", err)
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
		// the encoder in use is not in this build at all. A picture this
		// ffmpeg cannot decode is said so: no encoder would help. When
		// choosing again changes nothing, or the reason is not known, the
		// microphone may be what stops it: the camera is tried alone
		// (micTrial).
		switch kind := classifyExit(err); kind {
		case exitInput:
			s.setTrouble("the camera or microphone could not be opened; choosing the devices again")
			s.log.Warn("capture: ffmpeg could not open the camera or microphone; choosing the devices again", "err", err)
			if changed, ok := s.rechoose(ctx); ok && !changed && s.tryAlone(err) {
				continue
			}
		case exitOption:
			if s.revertEncoder(err) {
				continue
			}
			s.setTrouble("ffmpeg does not know an option it was given")
		case exitDecoder:
			format := undecodable(err)
			s.setTrouble("the camera sends its picture as " + format + ", which this ffmpeg cannot decode")
			s.log.Warn("capture: the camera sends a format this ffmpeg cannot decode", "format", format, "err", err)
		default:
			// Words that name nothing: the camera alone first, so a
			// microphone that stops it costs the hardware encoder nothing;
			// if the camera alone does not start either, the encoder next.
			if kind == exitUnknown && s.tryAlone(err) {
				continue
			}
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

// stalled is the announce grace running out on the ffmpeg whose context is
// ctx: it has been running since then and has not published. With the
// microphone in it, and none tried this run, it is ended and the camera
// tried alone (micTrial): a microphone that sends nothing holds back the
// picture too. Otherwise trouble says the camera delivered no picture, in
// the words of the system it runs on (stallTrouble), and one line is
// logged; nothing is restarted: a camera the system refuses would be
// refused again, a camera that is merely slow to start would be set back
// to zero, and the enclave's relay asks for the stream again on its own.
// An ffmpeg that has published, or ended, in the meantime is left alone.
func (s *Source) stalled(ctx context.Context) {
	s.mu.Lock()
	quiet := !s.publishing && ctx.Err() == nil
	cam, grace := s.cam, s.announceGrace
	var alone context.CancelFunc
	var mic string
	if quiet {
		if s.tryAloneLocked() {
			alone, mic, s.retrying = s.procCancel, s.mic.Name, true
			s.trouble = "the camera and microphone sent nothing in " + describeDuration(grace) + "; trying the camera alone"
		} else {
			if s.trial == micTrying {
				// The camera alone sends nothing either: the trouble is
				// the camera's, even if its picture comes later.
				s.trial = micBack
			}
			s.trouble = stallTrouble(s.goos, grace)
		}
	}
	s.mu.Unlock()
	switch {
	case alone != nil:
		s.log.Warn("capture: ffmpeg is running but has not started publishing; trying the camera without the microphone", "camera", cam.Name, "mic", mic, "after", grace.String())
		alone()
	case quiet:
		s.log.Warn("capture: ffmpeg is running but has not started publishing; the camera delivered no picture", "camera", cam.Name, "after", grace.String())
	}
}

// tryAlone starts the microphone's trial after a capture with it that
// exited without getting going (tryAloneLocked), and says so; it reports
// whether the next ffmpeg is to start at once, without the microphone.
func (s *Source) tryAlone(cause error) bool {
	s.mu.Lock()
	ok := s.tryAloneLocked()
	var mic string
	if ok {
		mic = s.mic.Name
		s.trouble = "the camera and microphone did not start together; trying the camera alone"
	}
	s.mu.Unlock()
	if ok {
		s.log.Warn("capture: ffmpeg did not get going with the microphone; trying the camera alone", "mic", mic, "err", cause)
	}
	return ok
}

// stallTrouble is what to say when ffmpeg has run for grace without
// publishing, and the microphone was not what held it back (micTrial): the
// camera delivered no picture. On a Mac that is what a camera the system
// refuses looks like, and the refusal is silent when the application may
// not ask (a hardened-runtime application without the camera entitlement,
// as v0.16.0 and v0.17.0 were), so the words point at Privacy & Security;
// on Windows, at the camera privacy switch for desktop programs. Without a
// trailing stop: the reporters add their own.
func stallTrouble(goos string, grace time.Duration) string {
	after := "the camera delivered no picture in " + describeDuration(grace)
	switch goos {
	case "darwin":
		return after + "; macOS may have refused it: System Settings › Privacy & Security › Camera must list Masseuse and allow it"
	case "windows":
		return after + "; Windows may have refused it: Settings › Privacy & security › Camera, Let desktop apps access your camera, must be on"
	default:
		return after + "; the camera may be refused to this program, or held by another"
	}
}

// describeDuration is d for a person: whole seconds as "10 s", anything
// else as Go prints it.
func describeDuration(d time.Duration) string {
	if d >= time.Second && d%time.Second == 0 {
		return fmt.Sprintf("%d s", int(d/time.Second))
	}
	return d.String()
}

// rechoose lists the devices again and picks by the configured selectors:
// for an ffmpeg that could not open its input, and at a start while
// something stands in (degraded). On macOS the list is renumbered when a
// device comes or goes (a phone through Continuity Camera, most often); a
// device addressed by name (Device.Input) rides that out, one addressed by
// index does not, and a device may simply be gone, in which case the
// default or the fallback stands in as at New, and a microphone that
// cannot be chosen leaves the picture to go alone: a session under way
// never waits on one. A device that is back takes its place again. When
// no camera can be chosen the devices stay as they are, for the retry.
// Another camera brings its own modes, read again (shape). It reports
// whether the devices changed, and whether a choice could be made.
func (s *Source) rechoose(ctx context.Context) (changed, ok bool) {
	devs, err := s.list(ctx, s.ffmpeg)
	if err != nil {
		s.log.Warn("capture: cannot list the devices", "err", err)
		return false, false
	}
	cam, mic, subs, err := resolve(devs, s.conf, true)
	if err != nil {
		s.log.Warn("capture: the camera is not connected", "err", err)
		s.setTrouble("the camera is not connected: " + err.Error())
		return false, false
	}
	s.mu.Lock()
	camChanged := !sameDevice(cam, s.cam)
	changed = camChanged || (mic == nil) != (s.mic == nil) || (mic != nil && !sameDevice(*mic, *s.mic))
	s.cam, s.mic, s.subs = cam, mic, subs
	s.mu.Unlock()
	if changed {
		micName := "none"
		if mic != nil {
			micName = mic.Name
		}
		s.log.Info("capture: devices chosen again", "camera", cam.Name, "cameraInput", cam.input(), "mic", micName)
	}
	if camChanged {
		s.shape(ctx)
	}
	return changed, true
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
	// exitDecoder: the camera's picture is in a format this ffmpeg has no
	// decoder for.
	exitDecoder
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
	case has("decoding requested, but no decoder found"):
		return exitDecoder
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

// undecodable is the format an exitDecoder's ffmpeg named ("Decoding
// requested, but no decoder found for: mjpeg"), or "a format it does not
// name".
func undecodable(err error) string {
	const marker = "no decoder found for: "
	msg := err.Error()
	i := strings.Index(strings.ToLower(msg), marker)
	if i < 0 {
		return "a format it does not name"
	}
	name := msg[i+len(marker):]
	if j := strings.IndexAny(name, " |\n"); j >= 0 {
		name = name[:j]
	}
	if name == "" {
		return "a format it does not name"
	}
	return name
}

// runFFmpeg runs one ffmpeg until it exits or ctx ends; the error carries
// the last lines it wrote. With handover it replaces the one before under
// the readers (Reshape), with the same tracks.
func (s *Source) runFFmpeg(ctx context.Context, url string, handover bool) error {
	s.mu.Lock()
	opts, enc, in := s.opts, s.encoder, s.input
	opts.ceiling, opts.Bitrate = opts.Bitrate, s.bitrate
	cam, mic := s.cam, s.mic
	if (handover && !s.micInFFmpeg) || (!handover && s.leaveOutMicLocked()) {
		mic = nil
	}
	s.micInFFmpeg = mic != nil
	s.mu.Unlock()
	args := Args(s.goos, opts, in, cam, mic, enc, url)
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
