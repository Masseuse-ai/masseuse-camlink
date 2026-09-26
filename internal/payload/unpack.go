package payload

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/FemLed/masseuse-camlink/internal/pesig"
)

// UnpackUnder unpacks the payload the executable at exe carries into
// stateDir/bin/<id>/ and returns that directory; "" with no error when exe
// carries none (every build but the Windows package), is not a PE image,
// or is not there. Every file is checked against the manifest: a complete
// unpack from an earlier start is used as it is, and whatever else lies
// under bin/ (earlier versions, interrupted unpacks) is removed. The
// connector does this for its own file (cmd/masseuse-camlink), the desktop
// window for its own, which carries the connector too (docs/DESKTOP.md).
func UnpackUnder(exe, stateDir string, log *slog.Logger) (string, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	r, err := OpenFile(exe)
	switch {
	case errors.Is(err, ErrNone), errors.Is(err, pesig.ErrNotPE), errors.Is(err, os.ErrNotExist):
		return "", nil
	case err != nil:
		return "", err
	}
	defer r.Close()
	binDir := filepath.Join(stateDir, "bin")
	dir := filepath.Join(binDir, r.Info.ID())
	if err := r.Info.Check(dir); err == nil {
		prune(binDir, r.Info.ID(), log)
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
	prune(binDir, r.Info.ID(), log)
	log.Info("payload: unpacked", "dir", dir, "version", r.Info.Manifest.Version, "files", len(r.Info.Manifest.Files))
	return dir, nil
}

// prune removes what else is under bin/: the unpacked payloads of earlier
// versions and the leftovers of interrupted unpacks. A file still in use
// (Windows holds a running program's file) is left for the next start.
func prune(binDir, keep string, log *slog.Logger) {
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
