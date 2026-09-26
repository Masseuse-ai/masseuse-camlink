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
// The two ends live here. The Host side is the connector's: it spawns the
// helper and presents it as an estim.Finder (with Lister and Selector) whose
// Find returns a Driver that forwards every call. The guest side, Serve, is
// what a helper program runs: it answers the Host's requests from a real
// Finder and Driver. Both ends are in the connector's module, so a helper
// built from a tree that carries this package (the private one does) needs
// no exported API.
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

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/FemLed/masseuse-camlink/internal/estim"
)

// Protocol is the version both ends speak; `hello` reports it.
const Protocol = 1

// Prefix is what a helper program's file name starts with in the units
// directory: `camlink-unit-<name>` (`.exe` on Windows).
const Prefix = "camlink-unit-"

// Methods.
const (
	MethodHello     = "hello"
	MethodDescribe  = "describe"
	MethodList      = "list"
	MethodSelect    = "select"
	MethodFind      = "find"
	MethodRelease   = "release"
	MethodArm       = "arm"
	MethodRenewArm  = "renewArm"
	MethodStatus    = "status"
	MethodTelemetry = "telemetry"
	MethodExecute   = "execute"
	MethodClose     = "close"
	// MethodCancel is a notification from the Host: the execute with the
	// id given should stop as soon as it can (estim.Driver.Execute's
	// cancelled).
	MethodCancel = "cancel"
)

// Error codes.
const (
	CodeNoDevice    = "no_device"
	CodeLoss        = "loss"
	CodeCancelled   = "cancelled"
	CodeArmed       = "armed"
	CodeNoDriver    = "no_driver"
	CodeUnsupported = "unsupported"
	CodeError       = "error"
)

// envelope is one line either way. A request has Method (and ID unless it
// is a notification); an answer has ID and Result or Error.
type envelope struct {
	ID     *uint64         `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *wireError      `json:"error,omitempty"`
}

type wireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Reason  string `json:"reason,omitempty"`
}

// Hello is what a helper says about itself.
type Hello struct {
	Protocol int `json:"protocol"`
	// Name is the family's name, what messages call it: the file name's
	// suffix, as a rule ("example" for camlink-unit-example).
	Name string `json:"name"`
	// Kinds the helper's driver may report (estim.Kind).
	Kinds []estim.Kind `json:"kinds"`
}

// Found is `find`'s answer: the device opened, as the Host needs to present
// it before any other call.
type Found struct {
	Kind         estim.Kind         `json:"kind"`
	Label        string             `json:"label"`
	Port         string             `json:"port"`
	Capabilities estim.Capabilities `json:"capabilities"`
	// Held: another program on this computer has the unit open (estim.HeldReporter).
	Held bool `json:"held"`
	// RenewsArm: the driver has a countdown to renew (estim.ArmRenewer).
	RenewsArm bool `json:"renewsArm"`
}

type describeResult struct {
	Text string `json:"text"`
}

type listResult struct {
	Units []estim.Unit `json:"units"`
}

type selectParams struct {
	Unit string `json:"unit"`
}

type armParams struct {
	PowerMode string `json:"powerMode"`
}

type renewArmParams struct {
	// Until is RFC 3339 with nanoseconds.
	Until string `json:"until"`
}

type statusResult struct {
	Status estim.Status `json:"status"`
}

type telemetryResult struct {
	Frame estim.Frame `json:"frame"`
}

type executeParams struct {
	Command  estim.Command `json:"command"`
	LevelMax int           `json:"levelMax"`
}

type executeResult struct {
	Result estim.Result `json:"result"`
}

type closeParams struct {
	Restore bool `json:"restore"`
}

type cancelParams struct {
	ID uint64 `json:"id"`
}

// ErrExited says the helper process ended (or never started) while a call
// was waiting on it. Driver calls report it as a loss (the unit stopped
// answering, as far as the connector can tell).
var ErrExited = errors.New("helper: the helper program ended")

// toWire turns a Go error into its wire form, keeping the connector's own
// errors recognizable on the other side.
func toWire(err error) *wireError {
	var loss *estim.LossError
	switch {
	case errors.As(err, &loss):
		return &wireError{Code: CodeLoss, Message: err.Error(), Reason: loss.Reason}
	case errors.Is(err, estim.ErrNoDevice):
		return &wireError{Code: CodeNoDevice, Message: err.Error()}
	case errors.Is(err, estim.ErrCancelled):
		return &wireError{Code: CodeCancelled, Message: err.Error()}
	case errors.Is(err, estim.ErrArmed):
		return &wireError{Code: CodeArmed, Message: err.Error()}
	}
	return &wireError{Code: CodeError, Message: err.Error()}
}

// fromWire is toWire's inverse.
func fromWire(e *wireError) error {
	if e == nil {
		return nil
	}
	switch e.Code {
	case CodeLoss:
		return &estim.LossError{Reason: e.Reason, Err: errors.New(e.Message)}
	case CodeNoDevice:
		// The message is most often ErrNoDevice's own text with a detail
		// after it; wrapping keeps errors.Is without saying it twice.
		if detail, ok := strings.CutPrefix(e.Message, estim.ErrNoDevice.Error()); ok {
			detail = strings.TrimPrefix(detail, ": ")
			if detail == "" {
				return estim.ErrNoDevice
			}
			return fmt.Errorf("%w: %s", estim.ErrNoDevice, detail)
		}
		return fmt.Errorf("%w: %s", estim.ErrNoDevice, e.Message)
	case CodeCancelled:
		return estim.ErrCancelled
	case CodeArmed:
		return estim.ErrArmed
	}
	return fmt.Errorf("helper: %s: %s", e.Code, e.Message)
}
