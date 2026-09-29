// Package ble is the connector's name for the Bluetooth Low Energy central
// it shares with unit driver helpers: github.com/Masseuse-ai/camlink-unit-sdk/ble,
// a pure Go central on CoreBluetooth, BlueZ and the Windows Runtime. The
// types and functions are that package's, aliased here so this tree and
// the trees built on it read as they did; new code imports the SDK.
package ble

import sdk "github.com/Masseuse-ai/camlink-unit-sdk/ble"

type (
	UUID          = sdk.UUID
	Advertisement = sdk.Advertisement
	Central       = sdk.Central
	Conn          = sdk.Conn
	Broadcast     = sdk.Broadcast
	Advertiser    = sdk.Advertiser
)

// DefaultMTU is the payload one write carries when the link negotiated
// nothing larger.
const DefaultMTU = sdk.DefaultMTU

var (
	// Open is a Central over this computer's Bluetooth.
	Open = sdk.Open
	// OpenAdvertiser transmits advertisements (Linux and Windows).
	OpenAdvertiser = sdk.OpenAdvertiser

	ErrUnsupported      = sdk.ErrUnsupported
	ErrUnavailable      = sdk.ErrUnavailable
	ErrDisconnected     = sdk.ErrDisconnected
	ErrNoCharacteristic = sdk.ErrNoCharacteristic
	ErrNotFound         = sdk.ErrNotFound
	ErrNoBroadcast      = sdk.ErrNoBroadcast
)
