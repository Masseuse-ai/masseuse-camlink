// Package estim links an electrical stimulation device plugged into this
// computer to the service: it holds the device fail-closed, relays the
// service's bounded commands to it and reports its status and telemetry
// (docs/PROTOCOL.md, section 7).
//
// The package is device-neutral. A Driver speaks one device's protocol; the
// Runtime owns the arm window, the cancellation latch, the caps, the
// attached session's settings within them and fault handling; the Session
// speaks the companion protocol with the service.
package estim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Kind names a device family. It is the `kind` of the Descriptor the
// connector reports (docs/PROTOCOL.md, section 7.3) and the value the
// service keys its behaviour on, so the names are fixed here whether or not
// this program carries a driver for them yet.
type Kind string

const (
	// KindMK312BT is the ErosTek MK-312BT over its serial link. The one
	// family with a driver in this program.
	KindMK312BT Kind = "mk312bt"
	// KindEstim2B is the E-Stim Systems 2B over its serial link.
	KindEstim2B Kind = "estim-2b"
	// KindCoyote is the DG-Lab Coyote, a Bluetooth Low Energy device.
	KindCoyote Kind = "dglabs-coyote"
	// KindTENS is any other transcutaneous electrical nerve stimulation unit
	// the connector cannot name more precisely.
	KindTENS Kind = "tens"
)

// Kinds lists every device family the Descriptor may name, in the order
// above. A service that receives a kind outside this list should treat the
// descriptor as malformed.
var Kinds = []Kind{KindMK312BT, KindEstim2B, KindCoyote, KindTENS}

// Known reports whether k is one of Kinds.
func (k Kind) Known() bool {
	for _, known := range Kinds {
		if k == known {
			return true
		}
	}
	return false
}

// Capabilities tells the service what a device accepts.
type Capabilities struct {
	// LevelMax is the highest output level the connector will set (0..99
	// scale of the device's front panel).
	LevelMax int `json:"levelMax"`
	// Channels the service may drive; the rest are pinned at zero.
	Channels []string `json:"channels"`
	// Modes are the device's own pattern numbers the service may select.
	Modes []int `json:"modes"`
	// Tempo says whether the device has a tempo control (percent).
	Tempo bool `json:"tempo"`
}

// Descriptor is what the connector reports about the device it is serving.
type Descriptor struct {
	Kind         Kind         `json:"kind"`
	Label        string       `json:"label"`
	Connected    bool         `json:"connected"`
	Capabilities Capabilities `json:"capabilities"`
}

// RoutineState is where the loaded pattern sits in its Channel A
// modulation: three parallel blocks (pulse width, frequency, intensity),
// each reported as value, floor, ceiling, signed step, phase percent and
// direction, or all null when the pattern leaves the block pinned.
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
type Status struct {
	Connected      bool    `json:"connected"`
	Port           *string `json:"port"`
	Mode           *int    `json:"mode"`
	LevelA         *int    `json:"levelA"`
	LevelB         *int    `json:"levelB"`
	Power          *string `json:"power"`
	BatteryPercent *int    `json:"batteryPercent,omitempty"`
	ADCOverride    *bool   `json:"adcOverride"`
	LevelMA        *int    `json:"levelMA"`
	MAMin          *int    `json:"maMin"`
	MAMax          *int    `json:"maMax"`
	MAPercent      *int    `json:"maPercent"`
	MAPotOverride  *bool   `json:"maPotOverride"`
	*RoutineState
	Error *string `json:"error,omitempty"`
}

// Frame is one telemetry sample: a compact routine reading at 2 Hz, or a
// gap marker saying why none could be taken.
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
	// attached session sets its own maximum (Settings.LevelMax).
	DefaultLevelCap = 85
	// LevelScaleMax is the top of the device's own 0..99 scale; no
	// setting passes it.
	LevelScaleMax = 99
	// LevelDeltaCap bounds one adjust_level step.
	LevelDeltaCap = 5
	// TempoPercentCap is the top of the tempo (percent) scale.
	TempoPercentCap = 100
	// TempoDeltaCap bounds one adjust_ma step.
	TempoDeltaCap = 10
)

// Power ranges a session may arm the device in.
const (
	PowerModeNormal = "normal"
	PowerModeHigh   = "high"
)

// Settings are what the attached session may adjust about how the
// connector drives the device (docs/PROTOCOL.md, section 7.3,
// `device_settings`): the power range the device is armed in, and the
// level no command may set past. They are held for the attached session
// alone; a detach restores DefaultSettings.
type Settings struct {
	PowerMode string `json:"powerMode"`
	LevelMax  int    `json:"levelMax"`
}

// DefaultSettings are what apply until a session sets its own: the high
// power range while armed, the level at most DefaultLevelCap.
func DefaultSettings() Settings {
	return Settings{PowerMode: PowerModeHigh, LevelMax: DefaultLevelCap}
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
	// Label names the device for people ("ErosTek MK-312BT").
	Label() string
	// Port is where the device is attached (a serial port path).
	Port() string
	Capabilities() Capabilities
	// Release fails closed: every output to zero, the front panel live, the
	// power range back to normal. It carries on through failures and
	// reports them together.
	Release(ctx context.Context) error
	// Arm prepares the device for an armed window with outputs still at
	// zero, in the power range named (PowerModeNormal or PowerModeHigh).
	Arm(ctx context.Context, powerMode string) error
	Status(ctx context.Context) (Status, error)
	// Telemetry is a compact routine sample, cheaper than Status.
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
