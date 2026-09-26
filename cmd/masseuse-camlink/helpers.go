package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/estim"
	"github.com/FemLed/masseuse-camlink/internal/estim/helper"
)

// Unit driver helpers (internal/estim/helper, docs/UNITS.md): drivers for
// stimulation units the connector's own tree does not carry, published by
// masseuse.ai as programs named camlink-unit-<name> in the units directory.
// Each one found becomes a device family, tried after the Mastago, and
// `estim probe` describes what it can see. The connector runs a helper as
// a child process and hands it nothing but its own stdio and the state
// directory: the camera and the microphone never reach it.

// The helpers flag: a directory, or "none".
var estimHelpers = flag.String("estim-helpers", envOr("MASSEUSE_CAMLINK_HELPERS", ""),
	`the directory of unit driver helpers (camlink-unit-*) to serve stimulation units from; "none" runs without any (default: Contents/Helpers/units in the application bundle, else units/ next to the program)`)

// helpersDir is the directory to look in: the flag's, or the default place
// for this program (bundledHelpersDir), or "" when helpers are off.
func helpersDir(flagValue string, exe string) string {
	v := strings.TrimSpace(flagValue)
	if strings.EqualFold(v, "none") {
		return ""
	}
	if v != "" {
		return v
	}
	return bundledHelpersDir(exe, runtime.GOOS)
}

// bundledHelpersDir is where the release puts the helpers: in a macOS
// application bundle (exe in Contents/MacOS) Contents/Helpers/units;
// anywhere, units/ next to the executable (the way ffmpeg is found,
// internal/capture).
func bundledHelpersDir(exe, goos string) string {
	dir := filepath.Dir(exe)
	if goos == "darwin" && filepath.Base(dir) == "MacOS" && filepath.Base(filepath.Dir(dir)) == "Contents" {
		return filepath.Join(filepath.Dir(dir), "Helpers", "units")
	}
	return filepath.Join(dir, "units")
}

// helperPrograms lists the helper programs in dir: files named
// camlink-unit-<name> (with .exe on Windows), executable, in name order.
// A missing directory is no helpers.
func helperPrograms(dir string, goos string) []string {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var programs []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), helper.Prefix) {
			continue
		}
		if goos == "windows" {
			if !strings.EqualFold(filepath.Ext(e.Name()), ".exe") {
				continue
			}
		} else {
			info, err := e.Info()
			if err != nil || info.Mode()&0o111 == 0 {
				continue
			}
		}
		programs = append(programs, filepath.Join(dir, e.Name()))
	}
	sort.Strings(programs)
	return programs
}

// helperName is a program's family name from its file name:
// camlink-unit-example is "example".
func helperName(path string) string {
	name := strings.TrimPrefix(filepath.Base(path), helper.Prefix)
	return strings.TrimSuffix(name, filepath.Ext(name))
}

// helperFamilies is one device family per helper program in dir. Each is
// greeted now (hello, with a short timeout) so that a helper that does not
// run on this system is logged and left out rather than tried at every
// scan; the family's name on the console is the helper's own.
func helperFamilies(ctx context.Context, dir string, stateDir string, log *slog.Logger) []deviceFamily {
	var fams []deviceFamily
	for _, path := range helperPrograms(dir, runtime.GOOS) {
		h := helper.New(path, filepath.Join(stateDir, "units", helperName(path)), log)
		hctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		hello, err := h.Hello(hctx)
		cancel()
		if err != nil {
			log.Warn("estim: a unit driver helper could not be started; left out", "path", path, "err", err)
			_ = h.Close()
			continue
		}
		log.Info("estim: unit driver helper", "name", hello.Name, "kinds", hello.Kinds, "path", path)
		host := h
		fams = append(fams, deviceFamily{
			name:   fmt.Sprintf("%s (helper)", hello.Name),
			finder: func(finderConfig) estim.Finder { return host },
		})
	}
	return fams
}

// registerHelpers adds the helpers in the units directory to the family
// registry (once) and returns the console's line about them.
func registerHelpers(ctx context.Context, stateDir string, log *slog.Logger) string {
	exe, _ := os.Executable()
	dir := helpersDir(*estimHelpers, exe)
	found := helperFamilies(ctx, dir, stateDir, log)
	families = append(families, found...)
	return helpersLine(dir, found)
}

// helpersLine is the console's word on the drivers this run has.
func helpersLine(dir string, found []deviceFamily) string {
	names := make([]string, 0, len(found))
	for _, f := range found {
		names = append(names, strings.TrimSuffix(f.name, " (helper)"))
	}
	switch {
	case dir == "":
		return "Unit drivers: Mastago (built in); helpers off (-estim-helpers none)."
	case len(found) == 0:
		return fmt.Sprintf("Unit drivers: Mastago (built in); no helpers in %s.", dir)
	default:
		return fmt.Sprintf("Unit drivers: Mastago (built in) + %d helper(s): %s.", len(found), strings.Join(names, ", "))
	}
}
