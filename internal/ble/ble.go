// Package ble is a minimal Bluetooth Low Energy central: enough to find a
// peripheral, connect to it, write to a characteristic and receive its
// notifications. It is pure Go so the connector keeps cross-compiling with
// CGO_ENABLED=0: on macOS it drives CoreBluetooth through the Objective-C
// runtime (purego), on Linux it speaks to BlueZ over D-Bus, and on Windows
// it drives the Windows Runtime's Windows.Devices.Bluetooth through its
// COM vtables (winrt-go).
//
// Everything above the Central and Conn interfaces is tested against fakes;
// the backends themselves are exercised by `estim probe` on a real system.
package ble

import (
	"context"
	"errors"
	"strings"
)

// A UUID names a service or characteristic. Comparisons go through Equal so
// the 16-bit short form ("fff0") and the full Bluetooth base form
// ("0000fff0-0000-1000-8000-00805f9b34fb") name the same thing.
type UUID string

const baseSuffix = "-0000-1000-8000-00805f9b34fb"

// Canonical is the lower-case 128-bit form.
func (u UUID) Canonical() string {
	s := strings.ToLower(strings.TrimSpace(string(u)))
	switch len(s) {
	case 4:
		return "0000" + s + baseSuffix
	case 8:
		return s + baseSuffix
	}
	return s
}

// Short is the 16-bit form when the UUID is on the Bluetooth base, the
// canonical form otherwise.
func (u UUID) Short() string {
	c := u.Canonical()
	if len(c) == 36 && strings.HasPrefix(c, "0000") && strings.HasSuffix(c, baseSuffix) {
		return c[4:8]
	}
	return c
}

// Equal says whether two UUIDs name the same thing in any form.
func (u UUID) Equal(o UUID) bool { return u.Canonical() == o.Canonical() }

// An Advertisement is what a scan learned about a peripheral, or what the
// system knows about one it already holds a connection to.
type Advertisement struct {
	// ID is the system's identifier for the peripheral: a CoreBluetooth
	// UUID on macOS (per computer), the device address on Linux and
	// Windows (AA:BB:CC:DD:EE:FF).
	ID string
	// Name is the advertised local name, or the system's cached name.
	Name string
	// Services are the advertised service UUIDs, when any.
	Services []UUID
	// RSSI is the signal strength in dBm, 0 when unknown.
	RSSI int
	// Connected says the system already holds a connection to it (the
	// ConnectedWithService path); such a peripheral does not advertise.
	Connected bool
}

// HasService says whether the advertisement names the service.
func (a Advertisement) HasService(u UUID) bool {
	for _, s := range a.Services {
		if s.Equal(u) {
			return true
		}
	}
	return false
}

// A Central finds peripherals and connects to them.
type Central interface {
	// Scan reports the first advertisement match accepts, or ctx's error.
	Scan(ctx context.Context, match func(Advertisement) bool) (Advertisement, error)
	// ConnectedWithService lists the peripherals the system already holds
	// a connection to (another program's) that offer the service. Where
	// the system cannot tell, the list is empty.
	ConnectedWithService(ctx context.Context, service UUID) ([]Advertisement, error)
	// Connect opens a connection to the peripheral with ID and discovers
	// its services and characteristics.
	Connect(ctx context.Context, id string) (Conn, error)
	// Close stops any scan and forgets the central; connections opened
	// through it stay usable until closed themselves.
	Close() error
}

// A Conn is one connected peripheral.
type Conn interface {
	ID() string
	Name() string
	// Write sends data to the characteristic, waiting for the peripheral's
	// acknowledgment when withResponse is set.
	Write(ctx context.Context, service, char UUID, data []byte, withResponse bool) error
	// Subscribe turns the characteristic's notifications on and delivers
	// each value to fn, in order, from one goroutine.
	Subscribe(ctx context.Context, service, char UUID, fn func([]byte)) error
	// Read reads the characteristic's value.
	Read(ctx context.Context, service, char UUID) ([]byte, error)
	// Disconnected is closed when the link drops, for whatever reason.
	Disconnected() <-chan struct{}
	// Close disconnects.
	Close() error
}

// ErrUnsupported says this system has no Bluetooth backend.
var ErrUnsupported = errors.New("ble: Bluetooth is not supported on this system")

// ErrUnavailable says the system has Bluetooth but it cannot be used now:
// switched off, or this program was not allowed to use it.
var ErrUnavailable = errors.New("ble: Bluetooth is not available")

// ErrDisconnected says the peripheral dropped the link.
var ErrDisconnected = errors.New("ble: disconnected")

// ErrNoCharacteristic says the peripheral has no such characteristic.
var ErrNoCharacteristic = errors.New("ble: no such characteristic")

// ErrNotFound says no peripheral with that ID is known to the system.
var ErrNotFound = errors.New("ble: peripheral not found")
