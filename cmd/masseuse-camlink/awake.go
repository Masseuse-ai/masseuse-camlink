package main

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/FemLed/masseuse-camlink/internal/awake"
)

// awakeReason is what the system shows beside the hold (System Settings,
// `pmset -g assertions`, `powercfg /requests`, `systemd-inhibit --list`).
const awakeReason = "Your masseuse uses this computer's camera, microphone and stimulation unit while it runs."

// keepAwake holds the computer awake for as long as the program runs and
// says so on the console, or says why it could not; with allowSleep it
// holds nothing. The returned function lets go (the process ending does
// too). Closing the lid still sleeps the computer, and the screen may
// still go dark: the camera does not need it.
func keepAwake(allowSleep bool, log *slog.Logger) func() {
	if allowSleep {
		fmt.Printf("This computer may go to sleep on its own (-allow-sleep); the camera, the microphone and the unit stop with it.\n")
		return func() {}
	}
	hold, err := awake.Take(appName, awakeReason)
	switch {
	case err == nil:
		fmt.Printf("This computer stays awake while %s runs (the screen may go dark; keep the lid open).\n", appName)
		return func() {
			if err := hold.Release(); err != nil {
				log.Debug("awake: release", "err", err)
			}
		}
	case errors.Is(err, awake.ErrUnsupported):
		return func() {}
	default:
		fmt.Printf("Could not keep this computer from sleeping (%v). Set it not to sleep while %s runs.\n", err, appName)
		log.Warn("awake: could not hold the computer awake", "err", err)
		return func() {}
	}
}
