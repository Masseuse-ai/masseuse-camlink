// Package estim links an electrical stimulation device plugged into this
// computer to the service: it holds the device fail-closed, relays the
// service's bounded commands to it and reports its status and telemetry
// (docs/PROTOCOL.md, section 7).
//
// The package is device-neutral. A Driver speaks one device's protocol; the
// Runtime owns the arm window, the cancellation latch, the caps and fault
// handling; the Session speaks the companion protocol with the service.
package estim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Kind names a supported device family.
type Kind string

// KindMK312BT is the ErosTek MK-312BT over its serial link.
const KindMK312BT Kind = "mk312bt"

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
	// LevelCap is the highest level a command may set.
	LevelCap = 85
	// LevelDeltaCap bounds one adjust_level step.
	LevelDeltaCap = 5
	// TempoPercentCap is the top of the tempo (percent) scale.
	TempoPercentCap = 100
	// TempoDeltaCap bounds one adjust_ma step.
	TempoDeltaCap = 10
)

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
	// zero (for the MK-312BT: the high power range).
	Arm(ctx context.Context) error
	Status(ctx context.Context) (Status, error)
	// Telemetry is a compact routine sample, cheaper than Status.
	Telemetry(ctx context.Context) (Frame, error)
	// Execute runs one bounded actuation command (not status or release,
	// which the Runtime handles). The Runtime has already checked the caps;
	// cancelled, polled between steps, preempts a ramp.
	Execute(ctx context.Context, cmd Command, cancelled func() bool) (Result, error)
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

// CheckCaps enforces the connector's caps on an actuation command.
func CheckCaps(cmd Command, caps Capabilities) error {
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
		if cmd.Level == nil || *cmd.Level < 0 || *cmd.Level > min(LevelCap, caps.LevelMax) {
			return fmt.Errorf("level must be 0..%d", min(LevelCap, caps.LevelMax))
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
