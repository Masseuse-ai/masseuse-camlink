package main

import (
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/FemLed/masseuse-camlink/internal/estim"
	"github.com/FemLed/masseuse-camlink/internal/estim/mastago"
)

// The stimulation device families this build can serve. Each family is
// registered with the command-line flags it needs: the Mastago TENS unit
// over Bluetooth, the connector's reference device, from this file; any
// other from a file of its own, appended to families in an init function,
// so that a build without the family drops the one file. The Runtime tries
// the families' finders in the order registered while it holds no device,
// and `estim probe` describes what each can see.

// A deviceFamily is one family's entry in the registry.
type deviceFamily struct {
	// name is what messages call the family.
	name string
	// finder builds the family's Finder for this run from the parsed flags,
	// or nil when the flags switch the family off.
	finder func(cfg finderConfig) estim.Finder
}

// finderConfig is what a family gets to build its finder.
type finderConfig struct {
	stateDir string
	log      *slog.Logger
}

// The Bluetooth family's flag: which unit, or none.
var estimBLE = flag.String("estim-ble", envOr("MASSEUSE_CAMLINK_ESTIM_BLE", ""),
	`the Bluetooth stimulation unit to serve, by the name it advertises ("G-12AB") or the system's identifier for it, when there are several; "off" to leave Bluetooth alone (default: the first unit found)`)

// The selection across families: which one unit, remembered.
var estimUnit = flag.String("estim-unit", envOr("MASSEUSE_CAMLINK_ESTIM_UNIT", ""),
	`the stimulation unit to serve when several are in reach, by the name it advertises ("G-12AB"), the system's identifier for it, or its serial port; remembered for the next start, as is a unit picked from the list this program prints or from the phone; "any" forgets the choice (default: the remembered unit, else the first found)`)

// estimFile remembers the unit selected (the flag, the console picker or
// the phone's device_select), so the next start serves it again.
const estimFile = "estim.json"

// estimConfig is the saved selection.
type estimConfig struct {
	// Unit is what the selection names: an estim.Unit's ID, or a name the
	// unit's family recognizes. Empty is no selection: the first found.
	Unit string `json:"unit"`
}

// resolveEstimSelection is the unit to serve: the flag when given (and
// saved; "any" clears the saved choice), else the saved one, else none.
func resolveEstimSelection(stateDir string, flagValue string) (string, error) {
	if v := strings.TrimSpace(flagValue); v != "" {
		if strings.EqualFold(v, "any") {
			v = ""
		}
		return v, saveEstimSelection(stateDir, v)
	}
	b, err := os.ReadFile(filepath.Join(stateDir, estimFile))
	if err != nil {
		return "", nil
	}
	var cfg estimConfig
	if json.Unmarshal(b, &cfg) != nil {
		return "", nil
	}
	return strings.TrimSpace(cfg.Unit), nil
}

// saveEstimSelection remembers the selection; empty forgets it.
func saveEstimSelection(stateDir string, unit string) error {
	path := filepath.Join(stateDir, estimFile)
	if unit == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	b, err := json.MarshalIndent(estimConfig{Unit: unit}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// families is the registry, in the order tried.
var families = []deviceFamily{{
	name: "Mastago TENS (Bluetooth)",
	finder: func(cfg finderConfig) estim.Finder {
		if familyOff(*estimBLE) {
			return nil
		}
		return mastago.NewFinder(*estimBLE, cfg.log)
	},
}}

// familyOff says whether a family's flag switches it off.
func familyOff(flagValue string) bool {
	return strings.EqualFold(strings.TrimSpace(flagValue), "off")
}

// familyPins are the families' own pin flags, for familyPinned; a family
// registered from its own file appends its flag in an init function.
var familyPins = []*string{estimBLE}

// familyPinned says whether any family's own flag names a unit (not empty,
// not "off"): that pin is this run's, and no remembered selection
// overrides it.
func familyPinned() bool {
	for _, p := range familyPins {
		if v := strings.TrimSpace(*p); v != "" && !familyOff(v) {
			return true
		}
	}
	return false
}

// deviceFinders builds the finders of every family the flags leave on, in
// registry order.
func deviceFinders(cfg finderConfig) estim.Finders {
	var fs estim.Finders
	for _, fam := range families {
		if f := fam.finder(cfg); f != nil {
			fs = append(fs, f)
		}
	}
	return fs
}

// familyNames lists the registered families, for messages.
func familyNames() []string {
	names := make([]string, 0, len(families))
	for _, fam := range families {
		names = append(names, fam.name)
	}
	return names
}
