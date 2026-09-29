// Package helper runs a stimulation-unit driver as a separate program.
//
// The connector's own tree carries one driver, the Mastago's. Drivers for
// other units are published by masseuse.ai as helper programs
// (`camlink-unit-<name>`, docs/UNITS.md) that the connector finds in its
// units directory and runs as child processes: the connector stays the
// program a person can read from end to end, and a helper is handed the
// unit's link (a serial port, a Bluetooth peripheral) and nothing else. It
// never sees the camera or the microphone, and it speaks to the service
// only through the connector.
//
// The wire, and the guest side a helper program runs (Main, Serve), are
// the unit driver SDK's, github.com/Masseuse-ai/camlink-unit-sdk/helper,
// aliased here; the Host side is this package's: it spawns the helper and
// presents it as an estim.Finder (with Lister and Selector) whose Find
// returns a Driver that forwards every call.
//
// Wire: newline-delimited JSON on the helper's stdin and stdout. A request
// is {"id", "method", "params"}; its answer {"id", "result"} or {"id",
// "error": {"code", "message", "reason"}}. A message without an id is a
// notification: the Host's "cancel" ends a running execute early; the
// helper's stderr is its log, relayed line by line into the connector's.
// Requests may overlap (the connector lists units while a device is open,
// and samples telemetry between commands); each is answered on its own.
// The error codes carry the connector's own errors across: "no_device" is
// estim.ErrNoDevice, "loss" an estim.LossError with its reason (so the
// `device` report says why a unit went, helper or not), "cancelled"
// estim.ErrCancelled, "armed" estim.ErrArmed.
package helper

import sdk "github.com/Masseuse-ai/camlink-unit-sdk/helper"

// Protocol is the version both ends speak; `hello` reports it. Version 2
// added the actuator model (`actuate`, `readings`, and `find`'s
// `actuators` and `sensors`); a helper of version 1 is served as before.
const Protocol = sdk.Protocol

// Prefix is what a helper program's file name starts with in the units
// directory: `camlink-unit-<name>` (`.exe` on Windows).
const Prefix = sdk.Prefix

// Methods.
const (
	MethodHello     = sdk.MethodHello
	MethodDescribe  = sdk.MethodDescribe
	MethodList      = sdk.MethodList
	MethodSelect    = sdk.MethodSelect
	MethodFind      = sdk.MethodFind
	MethodRelease   = sdk.MethodRelease
	MethodArm       = sdk.MethodArm
	MethodRenewArm  = sdk.MethodRenewArm
	MethodStatus    = sdk.MethodStatus
	MethodTelemetry = sdk.MethodTelemetry
	MethodExecute   = sdk.MethodExecute
	MethodActuate   = sdk.MethodActuate
	MethodReadings  = sdk.MethodReadings
	MethodClose     = sdk.MethodClose
	MethodCancel    = sdk.MethodCancel
)

// Error codes.
const (
	CodeNoDevice    = sdk.CodeNoDevice
	CodeLoss        = sdk.CodeLoss
	CodeCancelled   = sdk.CodeCancelled
	CodeArmed       = sdk.CodeArmed
	CodeNoActuator  = sdk.CodeNoActuator
	CodeNoDriver    = sdk.CodeNoDriver
	CodeUnsupported = sdk.CodeUnsupported
	CodeError       = sdk.CodeError
)

// The wire types and the guest side.
type (
	Hello      = sdk.Hello
	Found      = sdk.Found
	Options    = sdk.Options
	CodedError = sdk.CodedError
)

var (
	// Serve answers a Host on in/out from a finder: what a helper program
	// runs. Main wraps it with the flags the Host passes.
	Serve = sdk.Serve
	Main  = sdk.Main
	// ErrExited says the helper process ended while a call was waiting.
	ErrExited = sdk.ErrExited
)

// ExitGrace and ExitCloseGrace are Serve's, for the tests that time them.
const (
	ExitGrace      = sdk.ExitGrace
	ExitCloseGrace = sdk.ExitCloseGrace
)
