package main

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"

	"github.com/FemLed/masseuse-camlink/internal/payload"
	"github.com/FemLed/masseuse-camlink/internal/pesig"
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
// every build but the Windows package, is untouched by this.

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

// unpackPayloadFrom is unpackPayload for the executable at exe.
func unpackPayloadFrom(exe, stateDir string, log *slog.Logger) (string, error) {
	r, err := payload.OpenFile(exe)
	switch {
	case errors.Is(err, payload.ErrNone), errors.Is(err, pesig.ErrNotPE), errors.Is(err, os.ErrNotExist):
		return "", nil
	case err != nil:
		return "", err
	}
	defer r.Close()
	binDir := filepath.Join(stateDir, "bin")
	dir := filepath.Join(binDir, r.Info.ID())
	if err := r.Info.Check(dir); err == nil {
		prunePayloads(binDir, r.Info.ID(), log)
		return dir, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Info("payload: the unpacked files are not whole; unpacking again", "dir", dir, "why", err)
	}
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		return "", err
	}
	// Unpacked beside its final place and renamed in, so a start that is
	// interrupted, or a second instance at the same moment, never leaves
	// a half-written directory under the final name.
	tmp, err := os.MkdirTemp(binDir, ".unpack-*")
	if err != nil {
		return "", err
	}
	if err := r.Extract(tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return "", err
	}
	_ = os.RemoveAll(dir)
	if err := os.Rename(tmp, dir); err != nil {
		// Another instance got there first, or the old directory would
		// not go: what is there is used if it is whole.
		_ = os.RemoveAll(tmp)
		if cerr := r.Info.Check(dir); cerr != nil {
			return "", err
		}
	}
	prunePayloads(binDir, r.Info.ID(), log)
	log.Info("payload: unpacked", "dir", dir, "version", r.Info.Manifest.Version, "files", len(r.Info.Manifest.Files))
	return dir, nil
}

// prunePayloads removes what else is under bin\: the unpacked payloads of
// earlier versions and the leftovers of interrupted unpacks. A file still
// in use (Windows holds a running program's file) is left for the next
// start.
func prunePayloads(binDir, keep string, log *slog.Logger) {
	entries, err := os.ReadDir(binDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Name() == keep {
			continue
		}
		if err := os.RemoveAll(filepath.Join(binDir, e.Name())); err != nil {
			log.Debug("payload: an earlier unpack could not be removed yet", "dir", e.Name(), "err", err)
		}
	}
}
