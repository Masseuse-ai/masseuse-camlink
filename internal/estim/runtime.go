package estim

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Supervision timings.
const (
	// MaxArmWindow bounds one arming; heartbeat acknowledgments renew it.
	MaxArmWindow = 30 * time.Minute
	// HealthInterval is how often the device is read in full while idle.
	HealthInterval = 5 * time.Second
	// TelemetryInterval is the sampling period (2 Hz). A frame costs 12-16
	// serial round trips, so this leaves about half the bus for commands.
	TelemetryInterval = 500 * time.Millisecond
	// RingCapacity is two minutes of frames; with nothing draining, the
	// oldest are dropped.
	RingCapacity = 240
)

// Ring is a bounded FIFO of frames awaiting upload.
type Ring struct {
	mu      sync.Mutex
	cap     int
	frames  []Frame
	dropped int
}

// Offer appends a frame, dropping the oldest when full.
func (r *Ring) Offer(f Frame) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.cap
	if c <= 0 {
		c = RingCapacity
	}
	if len(r.frames) >= c {
		r.frames = r.frames[1:]
		r.dropped++
	}
	r.frames = append(r.frames, f)
}

// Drain takes up to limit frames, oldest first.
func (r *Ring) Drain(limit int) []Frame {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := min(limit, len(r.frames))
	out := append([]Frame(nil), r.frames[:n]...)
	r.frames = r.frames[n:]
	return out
}

// Len is how many frames wait.
func (r *Ring) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.frames)
}

// Dropped is how many frames were lost to a full ring.
func (r *Ring) Dropped() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped
}

// Runtime holds one device fail-closed. Only Arm clears the cancellation
// latch; every release, fault or expiry sets it, and an actuation command
// needs the device armed and the latch clear. Device I/O is serialized: a
// level ramp holds the device for its whole run and telemetry samples that
// find it busy record a gap instead of waiting. The attached session's
// Settings bound what runs (the power range armed in, the level maximum)
// within the device's own caps; they are DefaultSettings until set.
type Runtime struct {
	// Connect finds a device and opens a session with it. The Runtime
	// releases every fresh connection before trusting it.
	Connect func(ctx context.Context) (Driver, error)
	// OnDevice is told when the device connects or is lost, with the
	// descriptor to report. Called without any lock held.
	OnDevice func(ctx context.Context, d Descriptor)
	// ArmWindow is how long one arming lasts without renewal (MaxArmWindow
	// at most; the default).
	ArmWindow time.Duration
	Log       *slog.Logger
	// Now is the clock (time.Now).
	Now func() time.Time

	// dev serializes device I/O and guards device.
	dev    sync.Mutex
	device Driver
	// st guards the fields below.
	st          sync.Mutex
	cancel      atomic.Bool
	armDeadline time.Time
	lastStatus  Status
	hasStatus   bool
	lastError   string
	last        Descriptor
	frames      int
	gaps        int
	readErrors  int
	// settings are the attached session's; DefaultSettings while the zero
	// value (hasSettings false).
	settings    Settings
	hasSettings bool

	// Telemetry is the ring of sampled frames.
	Telemetry Ring
}

func (r *Runtime) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (r *Runtime) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runtime) window() time.Duration {
	if r.ArmWindow <= 0 || r.ArmWindow > MaxArmWindow {
		return MaxArmWindow
	}
	return r.ArmWindow
}

// DisconnectedStatus is what the service sees before a device is found.
func DisconnectedStatus() Status {
	return Status{LevelA: Int(0), LevelB: Int(0), ADCOverride: Bool(false), MAPotOverride: Bool(false)}
}

// -- state ------------------------------------------------------------------

// Connected says whether a device is held.
func (r *Runtime) Connected() bool {
	r.st.Lock()
	defer r.st.Unlock()
	return r.device != nil
}

// Armed says whether actuation commands may run: a device is held, the arm
// window has not run out and the latch is clear.
func (r *Runtime) Armed() bool {
	r.st.Lock()
	defer r.st.Unlock()
	return r.armedLocked()
}

func (r *Runtime) armedLocked() bool {
	return r.device != nil && !r.armDeadline.IsZero() && r.now().Before(r.armDeadline) && !r.cancel.Load()
}

// ArmedUntil is when the arm window runs out, when armed.
func (r *Runtime) ArmedUntil() (time.Time, bool) {
	r.st.Lock()
	defer r.st.Unlock()
	if !r.armedLocked() {
		return time.Time{}, false
	}
	return r.armDeadline, true
}

// CancelLatched says whether the fail-closed latch is set.
func (r *Runtime) CancelLatched() bool { return r.cancel.Load() }

// Settings are the attached session's settings in force (DefaultSettings
// until set).
func (r *Runtime) Settings() Settings {
	r.st.Lock()
	defer r.st.Unlock()
	return r.settingsLocked()
}

func (r *Runtime) settingsLocked() Settings {
	if !r.hasSettings {
		return DefaultSettings()
	}
	return r.settings
}

// SetSettings takes the attached session's settings. They bound every
// command from here on. While armed, a changed power range or a level
// maximum below the level the device is at cannot be applied in place:
// the device is released (outputs to zero, latch set) and the caller
// arms again, in the new range; released says whether that happened. A
// level maximum at or above the current level, or a change while not
// armed, applies silently. Invalid settings are refused and nothing
// changes.
func (r *Runtime) SetSettings(ctx context.Context, s Settings) (released bool, err error) {
	if err := s.Validate(); err != nil {
		return false, fmt.Errorf("estim: %w", err)
	}
	r.st.Lock()
	prev := r.settingsLocked()
	r.settings = s
	r.hasSettings = true
	armed := r.armedLocked()
	level := 0
	if r.hasStatus && r.lastStatus.LevelA != nil {
		level = *r.lastStatus.LevelA
	}
	r.st.Unlock()
	if prev == s || !armed {
		return false, nil
	}
	if prev.PowerMode == s.PowerMode && level <= s.LevelMax {
		return false, nil
	}
	if _, err := r.Release(ctx, "settings changed"); err != nil {
		return true, err
	}
	return true, nil
}

// ResetSettings restores DefaultSettings: the session they belonged to
// has detached. No device I/O; the detach's release has already run.
func (r *Runtime) ResetSettings() {
	r.st.Lock()
	defer r.st.Unlock()
	r.settings = Settings{}
	r.hasSettings = false
}

// RequestCancel sets the latch: a running ramp stops at its next step and
// no actuation runs until the next Arm.
func (r *Runtime) RequestCancel() { r.cancel.Store(true) }

// LastStatus is the most recent full reading, or what a disconnected or
// faulted device reports.
func (r *Runtime) LastStatus() Status {
	r.st.Lock()
	defer r.st.Unlock()
	if !r.hasStatus {
		return DisconnectedStatus()
	}
	return r.lastStatus
}

// LastError is the last failure, for diagnostics.
func (r *Runtime) LastError() string {
	r.st.Lock()
	defer r.st.Unlock()
	return r.lastError
}

// Descriptor describes the device being served, connected or not (the last
// one seen, with Connected false; the zero Descriptor before any).
func (r *Runtime) Descriptor() Descriptor {
	r.st.Lock()
	defer r.st.Unlock()
	d := r.last
	d.Connected = r.device != nil
	return d
}

// Counters are the telemetry statistics.
func (r *Runtime) Counters() (frames, gaps, readErrors int) {
	r.st.Lock()
	defer r.st.Unlock()
	return r.frames, r.gaps, r.readErrors
}

func (r *Runtime) setStatus(s Status) {
	r.st.Lock()
	defer r.st.Unlock()
	r.lastStatus, r.hasStatus = s, true
}

func (r *Runtime) clearArmLocked() { r.armDeadline = time.Time{} }

// -- connection ----------------------------------------------------------------

// Open connects to a device if none is held. Every fresh connection starts
// released: outputs zero, knobs live, normal power. Arming later raises the
// power range.
func (r *Runtime) Open(ctx context.Context) error {
	r.dev.Lock()
	defer r.dev.Unlock()
	if r.device != nil {
		return nil
	}
	if r.Connect == nil {
		return errors.New("estim: no way to connect a device")
	}
	d, err := r.Connect(ctx)
	if err != nil {
		return err
	}
	status, err := func() (Status, error) {
		if err := d.Release(ctx); err != nil {
			return Status{}, err
		}
		return d.Status(ctx)
	}()
	if err != nil {
		if cerr := d.Close(ctx, true); cerr != nil {
			r.log().Debug("estim: closing a device that failed to open", "err", cerr)
		}
		return err
	}
	desc := Descriptor{Kind: d.Kind(), Label: d.Label(), Connected: true, Capabilities: d.Capabilities()}
	r.st.Lock()
	r.device = d
	r.lastStatus, r.hasStatus = status, true
	r.lastError = ""
	r.last = desc
	r.st.Unlock()
	r.log().Info("estim: device ready", "device", d.Label(), "port", d.Port())
	r.notify(ctx, desc)
	return nil
}

func (r *Runtime) notify(ctx context.Context, d Descriptor) {
	if r.OnDevice != nil {
		r.OnDevice(ctx, d)
	}
}

// fault forgets a device that stopped answering; the caller holds dev.
func (r *Runtime) fault(err error) Driver {
	r.cancel.Store(true)
	r.st.Lock()
	defer r.st.Unlock()
	r.clearArmLocked()
	failed := r.device
	r.device = nil
	r.lastError = truncate(err.Error(), 300)
	// Every safety-relevant field unknown, so the service's completeness
	// check refuses to trust it.
	r.lastStatus = Status{Connected: false, Port: r.lastStatus.Port, Mode: r.lastStatus.Mode, Power: r.lastStatus.Power, Error: String(r.lastError)}
	r.hasStatus = true
	return failed
}

// closeFailed closes a device being given up on. Never fails.
func (r *Runtime) closeFailed(ctx context.Context, d Driver, restore bool) {
	if d == nil {
		return
	}
	if err := d.Close(ctx, restore); err != nil {
		r.log().Debug("estim: closing a failed device", "err", err)
	}
}

// HealthCheck reads the device in full; on failure the device is released
// as far as it can be, closed and forgotten. A device that is busy with a
// command is taken as healthy. Reports whether a device is held afterwards.
func (r *Runtime) HealthCheck(ctx context.Context) bool {
	if !r.dev.TryLock() {
		return true
	}
	defer r.dev.Unlock()
	if r.device == nil {
		return false
	}
	status, err := r.device.Status(ctx)
	if err == nil {
		r.setStatus(status)
		return true
	}
	r.log().Warn("estim: device stopped answering; released and disconnected", "err", err)
	failed := r.fault(err)
	if rerr := failed.Release(ctx); rerr != nil {
		r.log().Debug("estim: release of a failed device", "err", rerr)
	}
	r.closeFailed(ctx, failed, false)
	r.notify(ctx, r.Descriptor())
	return false
}

// Close releases the device and ends its session.
func (r *Runtime) Close(ctx context.Context) error {
	r.cancel.Store(true)
	r.dev.Lock()
	defer r.dev.Unlock()
	if r.device == nil {
		return nil
	}
	err := r.device.Close(ctx, true)
	r.st.Lock()
	r.device = nil
	r.clearArmLocked()
	s := r.lastStatus
	s.Connected = false
	s.LevelA, s.LevelB = Int(0), Int(0)
	s.ADCOverride, s.MAPotOverride = Bool(false), Bool(false)
	r.lastStatus = s
	r.st.Unlock()
	r.notify(ctx, r.Descriptor())
	return err
}

// -- arming ----------------------------------------------------------------------

// Arm opens a bounded window: outputs zeroed, the power range set to the
// session's (Settings.PowerMode), the latch cleared. Only a live session
// attaching (or being renewed) calls it.
func (r *Runtime) Arm(ctx context.Context) error {
	r.dev.Lock()
	defer r.dev.Unlock()
	if r.device == nil {
		return errors.New("estim: device is not connected")
	}
	powerMode := r.Settings().PowerMode
	// Arming is the one act that clears the fail-closed latch.
	r.cancel.Store(false)
	err := func() error {
		if err := r.device.Release(ctx); err != nil {
			return err
		}
		if err := r.device.Arm(ctx, powerMode); err != nil {
			return err
		}
		s, err := r.device.Status(ctx)
		if err != nil {
			return err
		}
		r.setStatus(s)
		return nil
	}()
	r.st.Lock()
	defer r.st.Unlock()
	if err != nil {
		r.cancel.Store(true)
		r.clearArmLocked()
		return err
	}
	r.armDeadline = r.now().Add(r.window())
	r.log().Info("estim: armed", "window", r.window().String(), "power", powerMode)
	return nil
}

// ExtendArm pushes the arm expiry forward without touching the device; a
// renewal must not zero the outputs the way Arm does. False when there was
// nothing to renew.
func (r *Runtime) ExtendArm() bool {
	r.st.Lock()
	defer r.st.Unlock()
	if !r.armedLocked() {
		return false
	}
	r.armDeadline = r.now().Add(r.window())
	return true
}

// Release fails closed: outputs to zero, knobs live, normal power, the arm
// revoked and the latch set. The status afterwards is returned even when
// the release itself failed.
func (r *Runtime) Release(ctx context.Context, reason string) (Status, error) {
	r.cancel.Store(true)
	r.dev.Lock()
	defer r.dev.Unlock()
	r.st.Lock()
	r.clearArmLocked()
	dev := r.device
	r.st.Unlock()
	if dev == nil {
		return r.LastStatus(), nil
	}
	err := dev.Release(ctx)
	if err == nil {
		var s Status
		if s, err = dev.Status(ctx); err == nil {
			r.setStatus(s)
		}
	}
	if err != nil {
		failed := r.fault(err)
		r.closeFailed(ctx, failed, false)
		r.notify(ctx, r.Descriptor())
		return r.LastStatus(), err
	}
	r.log().Info("estim: released", "reason", reason)
	return r.LastStatus(), nil
}

// ReleaseIfArmExpired releases when the arm window has run out; true when
// it did.
func (r *Runtime) ReleaseIfArmExpired(ctx context.Context) (bool, error) {
	r.st.Lock()
	expired := !r.armDeadline.IsZero() && !r.now().Before(r.armDeadline)
	r.st.Unlock()
	if !expired {
		return false, nil
	}
	_, err := r.Release(ctx, "arm window expired")
	return true, err
}

// -- telemetry -------------------------------------------------------------------

// Sample takes one telemetry frame, or a gap marker when the device cannot
// be read. It never waits for a running command, and read failures are
// counted rather than fail-closing: HealthCheck owns fault detection, and a
// sampler that disarmed on one flaky read would end sessions for nothing.
func (r *Runtime) Sample(ctx context.Context, started time.Time) Frame {
	now := r.now()
	base := Frame{AtMs: now.UnixMilli(), MonotonicS: now.Sub(started).Seconds()}
	gap := func(reason string, err error) Frame {
		r.st.Lock()
		r.gaps++
		r.st.Unlock()
		base.Skipped = true
		base.SkipReason = String(reason)
		if err != nil {
			base.Error = String(truncate(err.Error(), 200))
		}
		return base
	}
	if !r.Connected() {
		return gap("disconnected", nil)
	}
	if !r.dev.TryLock() {
		return gap("busy", nil)
	}
	defer r.dev.Unlock()
	if r.device == nil {
		return gap("disconnected", nil)
	}
	f, err := r.device.Telemetry(ctx)
	if err != nil {
		r.st.Lock()
		r.readErrors++
		r.st.Unlock()
		return gap("read_error", err)
	}
	r.st.Lock()
	r.frames++
	r.st.Unlock()
	f.AtMs, f.MonotonicS, f.Skipped, f.SkipReason = base.AtMs, base.MonotonicS, false, nil
	return f
}

// -- commands --------------------------------------------------------------------

// ErrNotArmed is returned for an actuation while the device is not armed.
var ErrNotArmed = errors.New("estim: device is not armed")

// ErrLatched is returned for an actuation while the latch is set.
var ErrLatched = errors.New("estim: cancellation is latched; a session attach arms again")

// Execute runs one command and returns what it did and the status after.
func (r *Runtime) Execute(ctx context.Context, cmd Command) (Result, Status, error) {
	r.dev.Lock()
	defer r.dev.Unlock()
	if r.device == nil {
		return Result{}, r.LastStatus(), errors.New("estim: device is not connected")
	}
	dev := r.device
	switch cmd.Verb {
	case "status":
		s, err := dev.Status(ctx)
		if err != nil {
			return Result{}, r.LastStatus(), err
		}
		r.setStatus(s)
		return Result{Verb: "status"}, s, nil
	case "release":
		r.st.Lock()
		r.clearArmLocked()
		r.st.Unlock()
		if err := dev.Release(ctx); err != nil {
			return Result{}, r.LastStatus(), err
		}
		s, err := dev.Status(ctx)
		if err != nil {
			return Result{}, r.LastStatus(), err
		}
		r.setStatus(s)
		return Result{Verb: "release", Released: true}, s, nil
	}
	if !r.Armed() {
		return Result{}, r.LastStatus(), ErrNotArmed
	}
	if r.cancel.Load() {
		return Result{}, r.LastStatus(), ErrLatched
	}
	caps, settings := dev.Capabilities(), r.Settings()
	if err := CheckCaps(cmd, caps, settings); err != nil {
		return Result{}, r.LastStatus(), fmt.Errorf("estim: %w", err)
	}
	res, err := dev.Execute(ctx, cmd, LevelMaxFor(caps, settings), r.cancel.Load)
	if err != nil {
		return res, r.LastStatus(), err
	}
	s, err := dev.Status(ctx)
	if err != nil {
		return res, r.LastStatus(), err
	}
	r.setStatus(s)
	return res, s, nil
}

// -- supervision -------------------------------------------------------------------

// Run connects, then supervises until ctx ends: a full reading every
// HealthInterval while a device is held, a reconnection attempt while none
// is, the arm expiry, and telemetry sampling into the ring. On return the
// device is released and closed.
func (r *Runtime) Run(ctx context.Context) {
	started := r.now()
	if err := r.Open(ctx); err != nil && ctx.Err() == nil {
		r.logUnavailable(err)
	}
	health := time.NewTicker(HealthInterval)
	defer health.Stop()
	sample := time.NewTicker(TelemetryInterval)
	defer sample.Stop()
	for {
		select {
		case <-ctx.Done():
			cctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			if err := r.Close(cctx); err != nil {
				r.log().Warn("estim: release at exit incomplete", "err", err)
			}
			cancel()
			return
		case <-sample.C:
			if !r.Connected() {
				continue
			}
			r.Telemetry.Offer(r.Sample(ctx, started))
		case <-health.C:
			if r.Connected() {
				r.HealthCheck(ctx)
			}
			if !r.Connected() {
				if err := r.Open(ctx); err != nil && ctx.Err() == nil {
					r.logUnavailable(err)
				}
			}
			if _, err := r.ReleaseIfArmExpired(ctx); err != nil && ctx.Err() == nil {
				r.log().Warn("estim: release at arm expiry failed", "err", err)
			}
		}
	}
}

// logUnavailable logs why no device is held, once per distinct reason, so
// an absent device does not fill the log every five seconds.
func (r *Runtime) logUnavailable(err error) {
	msg := err.Error()
	r.st.Lock()
	same := r.lastError == truncate(msg, 300)
	r.lastError = truncate(msg, 300)
	r.st.Unlock()
	if same {
		return
	}
	if errors.Is(err, ErrNoDevice) {
		r.log().Debug("estim: no device", "err", err)
		return
	}
	r.log().Warn("estim: device unavailable", "err", err)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
