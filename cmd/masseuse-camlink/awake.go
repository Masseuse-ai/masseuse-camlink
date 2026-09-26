package main

import (
	"errors"
	"log/slog"

	"github.com/FemLed/masseuse-camlink/internal/awake"
)

// awakeReason is what the system shows beside the hold (System Settings,
// `pmset -g assertions`, `powercfg /requests`, `systemd-inhibit --list`).
const awakeReason = "Your masseuse uses this computer's camera, microphone and stimulation unit while it runs."

// keepAwake holds the computer awake for as long as the program runs and
// says so (reporter.Awake), or says why it could not; with allowSleep it
// holds nothing. The returned function lets go (the process ending does
// too). Closing the lid still sleeps the computer, and the screen may
// still go dark: the camera does not need it.
func keepAwake(allowSleep bool, log *slog.Logger, ui reporter) func() {
	if allowSleep {
		ui.Awake(awakeAllowed, "")
		return func() {}
	}
	hold, err := awake.Take(appName, awakeReason)
	switch {
	case err == nil:
		ui.Awake(awakeHeld, "")
		return func() {
			if err := hold.Release(); err != nil {
				log.Debug("awake: release", "err", err)
			}
		}
	case errors.Is(err, awake.ErrUnsupported):
		ui.Awake(awakeUnsupported, "")
		return func() {}
	default:
		ui.Awake(awakeFailed, err.Error())
		log.Warn("awake: could not hold the computer awake", "err", err)
		return func() {}
	}
}
