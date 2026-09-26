package main

import (
	"log/slog"
	"os"
	"runtime"

	"github.com/FemLed/masseuse-camlink/internal/payload"
)

// The Windows package carries ffmpeg.exe, the unit driver helpers and the
// notices inside the one executable people download (internal/payload;
// packaging/windows), so Masseuse.exe runs from wherever it lies with
// nothing beside it. Before anything looks for those files, the payload is
// unpacked under the state directory, bin\<id>\, every file checked
// against the payload's manifest; the id names the contents, so a new
// version unpacks beside the old one's directory, which is then removed,
// and a complete unpack from an earlier start is used as it is. ffmpeg and
// the helpers are then looked for there before anywhere else
// (capture.BundledDir, helpersDir). A program without a payload, which is
// every build but the Windows package, is untouched by this. Run by the
// desktop window, the window has unpacked its own payload, the connector
// among its files, and this program finds ffmpeg and the helpers beside
// itself.

// payloadDir is where this run's payload is unpacked; "" without one.
var payloadDir string

// unpackPayload unpacks the running program's payload, if it carries one,
// and returns the directory; "" when there is none. An error is a payload
// that could not be unpacked (damaged, or no room), which the caller says
// and carries on from: the program still runs, without a camera and
// without helpers, as a bare build would.
func unpackPayload(stateDir string, log *slog.Logger) (string, error) {
	if runtime.GOOS != "windows" {
		return "", nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", nil
	}
	return unpackPayloadFrom(exe, stateDir, log)
}

// unpackPayloadFrom is unpackPayload for the executable at exe
// (payload.UnpackUnder).
func unpackPayloadFrom(exe, stateDir string, log *slog.Logger) (string, error) {
	return payload.UnpackUnder(exe, stateDir, log)
}
