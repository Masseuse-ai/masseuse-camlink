// Package awake keeps the computer from going to sleep on its own while
// the connector runs.
//
// A laptop serving as the rear camera sits untouched at the foot of the
// bed while the phone runs the session, and the enclave takes minutes to
// boot: on battery, macOS and Windows put the computer to sleep a few
// minutes after the last touch, which stops the camera and the microphone
// (ffmpeg waits for a first frame that never comes) and drops the
// Bluetooth link to the stimulation unit. A hold prevents that idle sleep
// for as long as it is held; the screen may still go dark, which the
// camera does not need. Closing the lid still sleeps the computer on
// every system, and nothing here changes that.
//
// Each system has its own way, none needing cgo or a child process: an
// IOKit power assertion on macOS (through purego, as internal/ble drives
// CoreBluetooth), a power request on Windows (kernel32 through
// golang.org/x/sys/windows), a logind inhibitor on Linux (org.freedesktop.login1
// over D-Bus, as internal/ble drives BlueZ). All three belong to the
// process, so an exit of any kind lets go of them without cleanup.
package awake

import "errors"

// ErrUnsupported is returned by Take on a system with no way to hold the
// computer awake; the connector runs on regardless.
var ErrUnsupported = errors.New("awake: not supported on this system")

// Hold keeps the computer from going to sleep on its own (its screen may)
// until Release.
type Hold struct {
	release func() error
}

// Take asks the system to keep the computer awake. name is what the
// system shows for the hold (`pmset -g assertions`, `powercfg /requests`,
// `systemd-inhibit --list`); reason is one sentence saying why. The hold
// lasts until Release or the end of the process.
func Take(name, reason string) (*Hold, error) {
	release, err := take(name, reason)
	if err != nil {
		return nil, err
	}
	return &Hold{release: release}, nil
}

// Release lets the computer sleep on its own again. Releasing twice is
// harmless.
func (h *Hold) Release() error {
	if h == nil || h.release == nil {
		return nil
	}
	release := h.release
	h.release = nil
	return release()
}
