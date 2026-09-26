package main

import (
	"flag"
	"log/slog"
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
