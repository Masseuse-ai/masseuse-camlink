package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/FemLed/masseuse-camlink/internal/payload"
)

// connectorName is the connector's file beside the shell.
const connectorName = "masseuse-camlink"

// locateConnector finds the connector to run and what an update replaces
// (the connector's -install-root; docs/DESKTOP.md section 3):
//
//   - MASSEUSE_CAMLINK_BIN names the connector outright (a working tree;
//     `wails3 dev` with `task connector:build`), with no install root, so
//     the connector's updater sees a build from a working tree and stays
//     off.
//   - In the macOS bundle the connector is Contents/MacOS/masseuse-camlink
//     beside the shell, and the root is the .app.
//   - On Windows the shell carries the connector, ffmpeg and the unit
//     driver helpers as its payload (packaging/windows): unpacked under the
//     state directory, bin\<id>\, the connector runs from there and finds
//     the rest beside itself; the root is the shell's own .exe.
//   - Otherwise (the Linux desktop archive) the connector lies beside the
//     shell and the root is their directory.
func locateConnector(stateDir string, log *slog.Logger) (bin, root string, err error) {
	if v := os.Getenv("MASSEUSE_CAMLINK_BIN"); v != "" {
		if _, err := os.Stat(v); err != nil {
			return "", "", fmt.Errorf("MASSEUSE_CAMLINK_BIN: %w", err)
		}
		return v, "", nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return locateBeside(exe, stateDir, runtime.GOOS, log)
}

// locateBeside is locateConnector for the shell at exe on goos.
func locateBeside(exe, stateDir, goos string, log *slog.Logger) (bin, root string, err error) {
	dir := filepath.Dir(exe)
	name := connectorName
	if goos == "windows" {
		name += ".exe"
		unpacked, err := payload.UnpackUnder(exe, stateDir, log)
		if err != nil {
			return "", "", fmt.Errorf("the program's own files (the connector, ffmpeg and the unit drivers) could not be unpacked under %s: %w", stateDir, err)
		}
		if unpacked != "" {
			bin := filepath.Join(unpacked, name)
			if _, err := os.Stat(bin); err != nil {
				return "", "", fmt.Errorf("the package carries no connector: %w", err)
			}
			return bin, exe, nil
		}
	}
	bin = filepath.Join(dir, name)
	if _, err := os.Stat(bin); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", fmt.Errorf("the connector is missing: %s is not there (set MASSEUSE_CAMLINK_BIN to run one from elsewhere)", bin)
		}
		return "", "", err
	}
	if goos == "darwin" && filepath.Base(dir) == "MacOS" && filepath.Base(filepath.Dir(dir)) == "Contents" &&
		strings.HasSuffix(filepath.Dir(filepath.Dir(dir)), ".app") {
		return bin, filepath.Dir(filepath.Dir(dir)), nil
	}
	return bin, dir, nil
}
