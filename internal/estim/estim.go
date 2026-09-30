// Package estim links a stimulation device reachable from this computer
// (over Bluetooth Low Energy or a serial link) to the service: it holds the
// device fail-closed, relays the service's bounded commands to it and
// reports its status and telemetry (docs/PROTOCOL.md, section 7).
//
// The package is device-neutral. A Driver speaks one device's protocol; a
// Finder looks for one device family; the Runtime owns the arm window, the
// cancellation latch, the caps, the attached session's settings within
// them and fault handling; the Session speaks the companion protocol with
// the service.
//
// The driver interfaces, the wire types and the caps are the unit driver
// SDK's, github.com/Masseuse-ai/camlink-unit-sdk/unit, which a helper
// program (docs/UNITS.md) is written against; they are aliased here so
// this tree reads as it did. The Runtime and the Session are this
// package's own.
package estim

import (
	"github.com/Masseuse-ai/camlink-unit-sdk/unit"
)

// Kind names a device family (unit.Kind). It is the `kind` of the
// Descriptor the connector reports (docs/PROTOCOL.md, section 7.3) and the
// value the service keys its behaviour on. This program's own driver
// reports KindMastago; a unit driver helper reports its family's own
// string, which the service validates against its table. Kinds and Known
// cover the names this tree knows, a test helper rather than a wire gate.
type Kind = unit.Kind

const (
	// KindMastago is the Mastago transcutaneous electrical nerve
	// stimulation unit over Bluetooth Low Energy: the connector's
	// reference device, the one the documentation is written around.
	KindMastago Kind = "mastago"
	// KindTENS is any other transcutaneous electrical nerve stimulation unit
	// the connector cannot name more precisely.
	KindTENS Kind = "tens"
)

// Kinds lists the device families this tree names, in the order above.
var Kinds = []Kind{KindMastago, KindTENS}

// Known reports whether k is one of Kinds.
func Known(k Kind) bool {
	for _, known := range Kinds {
		if k == known {
			return true
		}
	}
	return false
}

// The driver-facing types, from the SDK.
type (
	Capabilities     = unit.Capabilities
	Identity         = unit.Identity
	IdentityReporter = unit.IdentityReporter
	Descriptor       = unit.Descriptor
	LossError        = unit.LossError
	Unit             = unit.Unit
	Lister           = unit.Lister
	Selector         = unit.Selector
	HeldReporter     = unit.HeldReporter
	RoutineState     = unit.RoutineState
	Status           = unit.Status
	Frame            = unit.Frame
	Command          = unit.Command
	Result           = unit.Result
	Settings         = unit.Settings
	Driver           = unit.Driver
	ArmRenewer       = unit.ArmRenewer
	Finder           = unit.Finder
	Finders          = unit.Finders

	// The actuator model, for a device that is more than an intensity
	// channel (the SDK's unit/actuator.go).
	Actuator     = unit.Actuator
	ActuatorKind = unit.ActuatorKind
	Sensor       = unit.Sensor
	SensorKind   = unit.SensorKind
	Reading      = unit.Reading
	Actuate      = unit.Actuate
	Actuating    = unit.Actuating
	Sensing      = unit.Sensing
)

// Why a unit is not connected (Descriptor.Reason).
const (
	ReasonIdleOff          = unit.ReasonIdleOff
	ReasonOutputOff        = unit.ReasonOutputOff
	ReasonBatteryOff       = unit.ReasonBatteryOff
	ReasonButtonOff        = unit.ReasonButtonOff
	ReasonLinkLost         = unit.ReasonLinkLost
	ReasonStoppedAnswering = unit.ReasonStoppedAnswering
	ReasonLetGo            = unit.ReasonLetGo
)

// Caps the connector holds every device to, whatever the service asks.
const (
	DefaultLevelCap = unit.DefaultLevelCap
	LevelScaleMax   = unit.LevelScaleMax
	LevelDeltaCap   = unit.LevelDeltaCap
	TempoPercentCap = unit.TempoPercentCap
	TempoDeltaCap   = unit.TempoDeltaCap
)

// Power ranges a session may arm the device in.
const (
	PowerModeNormal = unit.PowerModeNormal
	PowerModeHigh   = unit.PowerModeHigh
)

// The intensity channels a level command may name (a two-channel device
// lists both in its Capabilities; one listing none drives A alone).
const (
	ChannelA = unit.ChannelA
	ChannelB = unit.ChannelB
)

var (
	// ErrCancelled is returned by a ramp the cancellation latch preempted.
	ErrCancelled = unit.ErrCancelled
	// ErrNoDevice is returned by a scan that found no supported device.
	ErrNoDevice = unit.ErrNoDevice
	// ErrArmed is returned by a selection made while the device is armed: the
	// phone stops it first.
	ErrArmed = unit.ErrArmed

	LabelOf            = unit.LabelOf
	IdentityFromLabel  = unit.IdentityFromLabel
	IdentityOf         = unit.IdentityOf
	LossReason         = unit.LossReason
	DefaultSettings    = unit.DefaultSettings
	DefaultSettingsFor = unit.DefaultSettingsFor
	ParseSettings      = unit.ParseSettings
	LevelMaxFor        = unit.LevelMaxFor
	ParseCommand       = unit.ParseCommand
	CheckCaps          = unit.CheckCaps
	ChannelOf          = unit.ChannelOf
	ActuatorsOf        = unit.ActuatorsOf
	SensorsOf          = unit.SensorsOf
	CheckActuate       = unit.CheckActuate
	Int                = unit.Int
	Bool               = unit.Bool
	String             = unit.String
)
