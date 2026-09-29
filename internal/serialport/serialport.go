// Package serialport is the connector's name for the serial port package it
// shares with unit driver helpers: github.com/Masseuse-ai/camlink-unit-sdk/serialport.
// The types and functions are that package's, aliased here so this tree
// and the trees built on it read as they did; new code imports the SDK.
package serialport

import sdk "github.com/Masseuse-ai/camlink-unit-sdk/serialport"

type (
	Config    = sdk.Config
	Port      = sdk.Port
	Candidate = sdk.Candidate
)

// FTDIVendor is the USB vendor id of the FTDI adapters most units use.
const FTDIVendor = sdk.FTDIVendor

var (
	// Open opens the port at path: 8N1, DTR and RTS asserted.
	Open = sdk.Open
	// Candidates lists the serial ports that may lead to a device.
	Candidates = sdk.Candidates
	// SetLatency lowers an FTDI adapter's latency timer where the system
	// lets a program do so.
	SetLatency = sdk.SetLatency

	ErrNoPort = sdk.ErrNoPort
)
