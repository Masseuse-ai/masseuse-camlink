// Package estim links an electrical stimulation device reachable from this
// computer (over Bluetooth Low Energy or a serial link) to the service: it
// holds the device fail-closed, relays the service's bounded commands to
// it and reports its status and telemetry (docs/PROTOCOL.md, section 7).
//
// The package is device-neutral. A Driver speaks one device's protocol; a
// Finder looks for one device family; the Runtime owns the arm window, the
// cancellation latch, the caps, the attached session's settings within
// them and fault handling; the Session speaks the companion protocol with
// the service.
package estim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// Kind names a device family. It is the `kind` of the Descriptor the
// connector reports (docs/PROTOCOL.md, section 7.3) and the value the
// service keys its behaviour on, so the names are fixed here whether or not
// this program carries a driver for them yet. A private build may carry
// drivers for further kinds; the service validates kinds against its own
// table, so Known is a test helper rather than a wire gate.
type Kind string

const (
	// KindMastago is the Mastago transcutaneous electrical nerve
	// stimulation unit over Bluetooth Low Energy: the connector's
	// reference device, the one the documentation is written around.
	KindMastago Kind = "mastago"
	// KindEstim2B is the E-Stim Systems 2B over its serial link.
	KindEstim2B Kind = "estim-2b"
	// KindCoyote is the DG-Lab Coyote, a Bluetooth Low Energy device.
	KindCoyote Kind = "dglabs-coyote"
	// KindTENS is any other transcutaneous electrical nerve stimulation unit
	// the connector cannot name more precisely.
	KindTENS Kind = "tens"
)

// Kinds lists every device family the Descriptor may name, in the order
// above.
var Kinds = []Kind{KindMastago, KindEstim2B, KindCoyote, KindTENS}

// Known reports whether k is one of Kinds.
func (k Kind) Known() bool {
	for _, known := range Kinds {
		if k == known {
			return true
		}
	}
	return false
}

// Capabilities tells the service what a device accepts. Bounds are on the
// device's own scale: the service reads LevelMax as the top of that scale
// (25 on a Mastago unit) and reasons in its units.
type Capabilities struct {
	// LevelMax is the highest output level the connector will set, on the
	// device's own intensity scale.
	LevelMax int `json:"levelMax"`
	// Channels the service may drive; the rest are pinned at zero.
	Channels []string `json:"channels"`
	// Modes are the device's own program numbers the service may select.
	Modes []int `json:"modes"`
	// Tempo says whether the device has a tempo control (percent).
	Tempo bool `json:"tempo"`
	// PowerModes are the power ranges a session may arm the device in
	// (PowerModeNormal, PowerModeHigh); empty for a device with one range.
	PowerModes []string `json:"powerModes,omitempty"`
	// LevelMaxDefault is the level cap that applies until the attached
	// session sets its own; DefaultLevelCap when zero.
	LevelMaxDefault int `json:"levelMaxDefault,omitempty"`
	// Timer says the device has a countdown the connector arms to the arm
	// window, so the device stops by itself if the connector dies.
	Timer bool `json:"timer,omitempty"`
	// LoadDetect says the device reports whether the electrodes are on the
	// skin (Status.LoadDetected) and refuses output when they are not.
	LoadDetect bool `json:"loadDetect,omitempty"`
}

// HasPowerMode says whether the device arms in the range.
func (c Capabilities) HasPowerMode(mode string) bool {
	for _, m := range c.PowerModes {
		if m == mode {
			return true
		}
	}
	return false
}

// Descriptor is what the connector reports about the device it is serving:
// the selected unit, whether or not it is connected right now.
type Descriptor struct {
	Kind  Kind   `json:"kind"`
	Label string `json:"label"`
	// ID is what the system calls the unit (Driver.Port): a Bluetooth
	// peripheral's identifier or a serial port path, the same string a
	// Unit carries and a selection names. Empty before any device.
	ID           string       `json:"id,omitempty"`
	Connected    bool         `json:"connected"`
	Capabilities Capabilities `json:"capabilities"`
	// Held says another program on this computer has the unit open and
	// the connector shares its link (a Bluetooth unit the vendor's app
	// holds): that program's commands and the service's would collide.
	Held bool `json:"held,omitempty"`
}

// Unit is one stimulation unit this computer can see, served or not: what
// a Lister reports and the `device` message lists beside the Descriptor,
// so a person can pick one when there are several.
type Unit struct {
	// ID is what the system calls the unit, as Descriptor.ID.
	ID    string `json:"id"`
	Kind  Kind   `json:"kind"`
	Label string `json:"label"`
	// Held says another program on this computer has the unit open.
	Held bool `json:"held"`
}

// A Lister is a Finder that can say which units of its family are in reach
// without taking any of them.
type Lister interface {
	List(ctx context.Context) ([]Unit, error)
}

// A Selector is a Finder that can be restricted to one unit after it was
// built: the picker, the remembered choice, the service's `device_select`.
type Selector interface {
	// Select restricts the finder to the unit named: an ID, or a name the
	// family recognizes (the Mastago's advertised name or its suffix).
	// Empty lifts the restriction. A unit of another family never matches,
	// so one selection may be given to every family.
	Select(unit string)
}

// A HeldReporter is a Driver that knows whether another program had its
// unit open when it connected (Descriptor.Held).
type HeldReporter interface {
	Held() bool
}

// RoutineState is the modulation state a pattern-based device reports for
// the loaded program on Channel A: three parallel blocks (pulse width,
// frequency, intensity), each as value, floor, ceiling, signed step, phase
// percent and direction, or all null when the program leaves the block
// pinned. A device whose programs are fixed in firmware (a Mastago unit)
// reports none of it: the embedded pointer stays nil.
type RoutineState struct {
	GateValue        int  `json:"gateValue"`
	ModeRampValue    int  `json:"modeRampValue"`
	RoutineTimer     int  `json:"routineTimer"`
	SequenceProfiled bool `json:"sequenceProfiled"`
	SequenceVaries   bool `json:"sequenceVaries"`
	SequenceGated    bool `json:"sequenceGated"`

	SweepValue     *int    `json:"sweepValue"`
	SweepMin       *int    `json:"sweepMin"`
	SweepMax       *int    `json:"sweepMax"`
	SweepStep      *int    `json:"sweepStep"`
	SweepPercent   *int    `json:"sweepPercent"`
	SweepDirection *string `json:"sweepDirection"`

	FreqValue     *int    `json:"freqValue"`
	FreqMin       *int    `json:"freqMin"`
	FreqMax       *int    `json:"freqMax"`
	FreqStep      *int    `json:"freqStep"`
	FreqPercent   *int    `json:"freqPercent"`
	FreqDirection *string `json:"freqDirection"`

	IntensityValue     *int    `json:"intensityValue"`
	IntensityMin       *int    `json:"intensityMin"`
	IntensityMax       *int    `json:"intensityMax"`
	IntensityStep      *int    `json:"intensityStep"`
	IntensityPercent   *int    `json:"intensityPercent"`
	IntensityDirection *string `json:"intensityDirection"`
}

// Status is a full reading of the device. Safety-relevant fields are
// pointers so that a device that stopped answering reports them as null,
// which the service refuses to act on, rather than as a stale number.
//
// The field names are the wire protocol's and are shared by every kind: a
// single-channel device reports LevelB as 0 and the front-panel override
// flags as false; one without a tempo control leaves the MA fields null;
// one whose programs are fixed in firmware leaves the RoutineState nil.
type Status struct {
	Connected bool `json:"connected"`
	// Port is where the device is attached: a serial port path, or the
	// system's identifier for a Bluetooth peripheral.
	Port *string `json:"port"`
	// Mode is the device's own program number.
	Mode *int `json:"mode"`
	// LevelA and LevelB are the output levels on the device's own scale.
	LevelA *int `json:"levelA"`
	LevelB *int `json:"levelB"`
	// Power is the power range in force (PowerModeNormal, PowerModeHigh,
	// or the device's own name for another).
	Power          *string `json:"power"`
	BatteryPercent *int    `json:"batteryPercent,omitempty"`
	// ADCOverride and MAPotOverride say whether the connector has taken
	// the level and tempo controls away from the front panel.
	ADCOverride *bool `json:"adcOverride"`
	// LevelMA, MAMin, MAMax and MAPercent are the tempo control, for a
	// device that has one.
	LevelMA       *int  `json:"levelMA"`
	MAMin         *int  `json:"maMin"`
	MAMax         *int  `json:"maMax"`
	MAPercent     *int  `json:"maPercent"`
	MAPotOverride *bool `json:"maPotOverride"`
	*RoutineState
	// Outputting says whether the device is delivering its program right
	// now (a paused unit holds its level but delivers nothing), for a
	// device that distinguishes the two.
	Outputting *bool `json:"outputting,omitempty"`
	// LoadDetected says whether the electrodes are on the skin, for a
	// device that can tell (Capabilities.LoadDetect).
	LoadDetected *bool `json:"loadDetected,omitempty"`
	// TimerRemainingS is what is left of the device's own countdown, for a
	// device that has one (Capabilities.Timer).
	TimerRemainingS *int    `json:"timerRemainingS,omitempty"`
	Error           *string `json:"error,omitempty"`
}

// Frame is one telemetry sample: a compact reading at 2 Hz, or a gap
// marker saying why none could be taken. Which fields a frame carries
// depends on the device (see Status).
type Frame struct {
	AtMs       int64   `json:"atMs"`
	MonotonicS float64 `json:"monotonicS"`
	Skipped    bool    `json:"skipped"`
	SkipReason *string `json:"skipReason"`
	Error      *string `json:"error,omitempty"`

	Mode      *int `json:"mode,omitempty"`
	LevelA    *int `json:"levelA,omitempty"`
	LevelMA   *int `json:"levelMA,omitempty"`
	MAPercent *int `json:"maPercent,omitempty"`
	*RoutineState
	Outputting      *bool `json:"outputting,omitempty"`
	LoadDetected    *bool `json:"loadDetected,omitempty"`
	TimerRemainingS *int  `json:"timerRemainingS,omitempty"`
}

// Command is one bounded instruction from the service, in the shape the
// service's controller emits. Numbers are pointers so an absent field is
// told from a zero.
type Command struct {
	Verb    string `json:"verb"`
	Channel string `json:"channel,omitempty"`
	Mode    *int   `json:"mode,omitempty"`
	Level   *int   `json:"level,omitempty"`
	Delta   *int   `json:"delta,omitempty"`
	Percent *int   `json:"percent,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// Result is what a command did.
type Result struct {
	Verb            string `json:"verb"`
	Released        bool   `json:"released,omitempty"`
	Mode            *int   `json:"mode,omitempty"`
	Level           *int   `json:"level,omitempty"`
	PreviousLevel   *int   `json:"previousLevel,omitempty"`
	Percent         *int   `json:"percent,omitempty"`
	PreviousPercent *int   `json:"previousPercent,omitempty"`
	LevelMA         *int   `json:"levelMA,omitempty"`
}

// Caps the connector holds every device to, whatever the service asks.
const (
	// DefaultLevelCap is the highest level a command may set until the
	// attached session sets its own maximum (Settings.LevelMax), for a
	// device whose Capabilities name no LevelMaxDefault of their own.
	DefaultLevelCap = 85
	// LevelScaleMax is the top of the widest intensity scale a device
	// reports (0..99); no setting passes it, and a device's own LevelMax
	// bounds it further.
	LevelScaleMax = 99
	// LevelDeltaCap bounds one adjust_level step.
	LevelDeltaCap = 5
	// TempoPercentCap is the top of the tempo (percent) scale.
	TempoPercentCap = 100
	// TempoDeltaCap bounds one adjust_ma step.
	TempoDeltaCap = 10
)

// Power ranges a session may arm the device in. A device with one range
// (Capabilities.PowerModes empty) is armed the same whichever is named.
const (
	PowerModeNormal = "normal"
	PowerModeHigh   = "high"
)

// Settings are what the attached session may adjust about how the
// connector drives the device (docs/PROTOCOL.md, section 7.3,
// `device_settings`): the power range the device is armed in, and the
// level no command may set past. They are held for the attached session
// alone; a detach restores the defaults.
type Settings struct {
	PowerMode string `json:"powerMode"`
	LevelMax  int    `json:"levelMax"`
}

// DefaultSettings are what apply until a session sets its own, for a
// device whose Capabilities are not known: the high power range while
// armed, the level at most DefaultLevelCap.
func DefaultSettings() Settings {
	return Settings{PowerMode: PowerModeHigh, LevelMax: DefaultLevelCap}
}

// DefaultSettingsFor are the defaults for a device with caps: its own
// LevelMaxDefault when it names one, and the high range only for a device
// that arms in it. The zero Capabilities (no device known yet) give
// DefaultSettings.
func DefaultSettingsFor(caps Capabilities) Settings {
	s := DefaultSettings()
	if caps.LevelMaxDefault > 0 {
		s.LevelMax = caps.LevelMaxDefault
	}
	known := caps.LevelMax > 0 || len(caps.PowerModes) > 0
	if known && !caps.HasPowerMode(PowerModeHigh) {
		s.PowerMode = PowerModeNormal
	}
	return s
}

// Validate refuses a power range the connector does not arm in and a
// level maximum off the device's scale.
func (s Settings) Validate() error {
	if s.PowerMode != PowerModeNormal && s.PowerMode != PowerModeHigh {
		return fmt.Errorf("powerMode must be %q or %q", PowerModeNormal, PowerModeHigh)
	}
	if s.LevelMax < 0 || s.LevelMax > LevelScaleMax {
		return fmt.Errorf("levelMax must be 0..%d", LevelScaleMax)
	}
	return nil
}

// ParseSettings decodes a `device_settings` payload: both fields present,
// the power range one of the two, the level maximum an integer on the
// device's scale. Unknown fields are the service's business.
func ParseSettings(raw json.RawMessage) (Settings, error) {
	var p struct {
		PowerMode *string `json:"powerMode"`
		LevelMax  *int    `json:"levelMax"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return Settings{}, fmt.Errorf("estim: malformed settings: %w", err)
	}
	if p.PowerMode == nil || p.LevelMax == nil {
		return Settings{}, errors.New("estim: settings need powerMode and levelMax")
	}
	s := Settings{PowerMode: *p.PowerMode, LevelMax: *p.LevelMax}
	if err := s.Validate(); err != nil {
		return Settings{}, fmt.Errorf("estim: %w", err)
	}
	return s, nil
}

// LevelMaxFor is the level no command may set past under settings on a
// device with caps: the lowest of the session's maximum, the device's
// own and the scale's top.
func LevelMaxFor(caps Capabilities, settings Settings) int {
	return max(0, min(caps.LevelMax, settings.LevelMax, LevelScaleMax))
}

// A Driver speaks one device's protocol over an open link. Methods are
// called one at a time; the Runtime serializes them.
type Driver interface {
	Kind() Kind
	// Label names the device for people ("Mastago TENS G-12AB").
	Label() string
	// Port is where the device is attached: a serial port path, or the
	// system's identifier for a Bluetooth peripheral.
	Port() string
	Capabilities() Capabilities
	// Release fails closed: every output to zero and stopped, the device's
	// own controls live, the power range back to normal. It carries on
	// through failures and reports them together.
	Release(ctx context.Context) error
	// Arm prepares the device for an armed window with outputs still at
	// zero, in the power range named (PowerModeNormal or PowerModeHigh); a
	// device with one range ignores the name. A device with a countdown
	// arms it to the window, so it stops by itself if the connector dies.
	Arm(ctx context.Context, powerMode string) error
	Status(ctx context.Context) (Status, error)
	// Telemetry is a compact sample, cheaper than Status.
	Telemetry(ctx context.Context) (Frame, error)
	// Execute runs one bounded actuation command (not status or release,
	// which the Runtime handles). The Runtime has already checked the caps
	// and passes the level no step may pass (LevelMaxFor); cancelled,
	// polled between steps, preempts a ramp.
	Execute(ctx context.Context, cmd Command, levelMax int, cancelled func() bool) (Result, error)
	// Close releases the device when restore is set and leaves it ready for
	// a fresh connection. It never leaves a stale session behind.
	Close(ctx context.Context, restore bool) error
}

// An ArmRenewer is a Driver whose device has its own countdown
// (Capabilities.Timer). The Runtime calls RenewArm while armed, each time
// the arm window is extended, so the countdown follows the window without
// the outputs being touched.
type ArmRenewer interface {
	RenewArm(ctx context.Context, until time.Time) error
}

// A Finder looks for one device family on this computer. Finders are
// registered by the program in the order they are tried.
type Finder interface {
	// Find returns a driver for a device found and opened, or an error
	// wrapping ErrNoDevice when there is none. A device present but
	// unusable is reported as itself.
	Find(ctx context.Context) (Driver, error)
	// Describe writes what the finder can see to out, for `estim probe`:
	// the candidates it would try and why it would skip any.
	Describe(ctx context.Context, out io.Writer) error
}

// Finders tries several finders in order and is itself a Finder. Find
// returns the first device found; when none is, an error other than
// ErrNoDevice (a device present but unusable) is preferred to ErrNoDevice,
// so the reason a device is not served is the one reported.
type Finders []Finder

// Find tries each finder in order.
func (fs Finders) Find(ctx context.Context) (Driver, error) {
	if len(fs) == 0 {
		return nil, fmt.Errorf("%w: every device family is switched off", ErrNoDevice)
	}
	var firstErr error
	for _, f := range fs {
		d, err := f.Find(ctx)
		if err == nil {
			return d, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if firstErr == nil || (errors.Is(firstErr, ErrNoDevice) && !errors.Is(err, ErrNoDevice)) {
			firstErr = err
		}
	}
	return nil, firstErr
}

// Describe runs each finder's Describe, a blank line between them, and
// reports the first failure after running them all.
func (fs Finders) Describe(ctx context.Context, out io.Writer) error {
	var firstErr error
	for i, f := range fs {
		if i > 0 {
			fmt.Fprintln(out)
		}
		if err := f.Describe(ctx, out); err != nil && firstErr == nil {
			firstErr = err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return firstErr
}

// List is Lister.List over every finder that can list: the units of all
// families together, in the finders' order, and the first failure after
// listing them all, so a family whose bus is unavailable does not hide
// another's units.
func (fs Finders) List(ctx context.Context) ([]Unit, error) {
	var units []Unit
	var firstErr error
	for _, f := range fs {
		l, ok := f.(Lister)
		if !ok {
			continue
		}
		us, err := l.List(ctx)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		units = append(units, us...)
		if ctx.Err() != nil {
			return units, ctx.Err()
		}
	}
	return units, firstErr
}

// Select is Selector.Select over every finder that can be restricted: the
// one selection is given to every family, and only the family the unit
// belongs to finds anything under it.
func (fs Finders) Select(unit string) {
	for _, f := range fs {
		if s, ok := f.(Selector); ok {
			s.Select(unit)
		}
	}
}

// Close closes every finder that holds something (an io.Closer) and
// reports the first failure.
func (fs Finders) Close() error {
	var firstErr error
	for _, f := range fs {
		if c, ok := f.(io.Closer); ok {
			if err := c.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// ErrCancelled is returned by a ramp the cancellation latch preempted.
var ErrCancelled = errors.New("estim: cancelled by a release")

// ErrNoDevice is returned by a scan that found no supported device.
var ErrNoDevice = errors.New("estim: no supported device found")

// ParseCommand decodes a command and rejects the malformed: unknown verbs,
// non-integer numbers, a channel other than A.
func ParseCommand(raw json.RawMessage) (Command, error) {
	// Unknown fields are the service's business: a newer controller must not
	// break an older connector, so only the known ones are read.
	var cmd Command
	if err := json.Unmarshal(raw, &cmd); err != nil {
		return Command{}, fmt.Errorf("estim: malformed command: %w", err)
	}
	switch cmd.Verb {
	case "status", "release", "set_mode", "set_level", "adjust_level", "set_ma", "adjust_ma":
	default:
		return Command{}, fmt.Errorf("estim: unsupported command: %q", cmd.Verb)
	}
	if cmd.Channel != "" && cmd.Channel != "a" {
		return Command{}, fmt.Errorf("estim: only channel A is controlled")
	}
	return cmd, nil
}

// CheckCaps enforces the connector's caps on an actuation command: the
// device's, and the session's settings within them.
func CheckCaps(cmd Command, caps Capabilities, settings Settings) error {
	switch cmd.Verb {
	case "set_mode":
		if cmd.Mode == nil {
			return errors.New("set_mode needs a mode")
		}
		for _, m := range caps.Modes {
			if m == *cmd.Mode {
				return nil
			}
		}
		return fmt.Errorf("mode is not allowed: %d", *cmd.Mode)
	case "set_level":
		levelMax := LevelMaxFor(caps, settings)
		if cmd.Level == nil || *cmd.Level < 0 || *cmd.Level > levelMax {
			return fmt.Errorf("level must be 0..%d", levelMax)
		}
	case "adjust_level":
		if cmd.Delta == nil || *cmd.Delta == 0 || *cmd.Delta < -LevelDeltaCap || *cmd.Delta > LevelDeltaCap {
			return fmt.Errorf("delta must be a non-zero integer from -%d to %d", LevelDeltaCap, LevelDeltaCap)
		}
	case "set_ma":
		if !caps.Tempo {
			return errors.New("device has no tempo control")
		}
		if cmd.Percent == nil || *cmd.Percent < 0 || *cmd.Percent > TempoPercentCap {
			return fmt.Errorf("percent must be 0..%d", TempoPercentCap)
		}
	case "adjust_ma":
		if !caps.Tempo {
			return errors.New("device has no tempo control")
		}
		if cmd.Delta == nil || *cmd.Delta == 0 || *cmd.Delta < -TempoDeltaCap || *cmd.Delta > TempoDeltaCap {
			return fmt.Errorf("delta must be a non-zero integer from -%d to %d", TempoDeltaCap, TempoDeltaCap)
		}
	}
	return nil
}

// Int returns a pointer to v, for the nullable fields above.
func Int(v int) *int { return &v }

// Bool returns a pointer to v.
func Bool(v bool) *bool { return &v }

// String returns a pointer to v.
func String(v string) *string { return &v }
