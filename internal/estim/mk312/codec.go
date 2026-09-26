// Package mk312 speaks the ErosTek MK-312BT's serial protocol: a 19200 baud
// link on which the host reads and writes single bytes of the device's
// memory, XOR-scrambled with a session key agreed at connection time. The
// package implements estim.Driver for it with a fixed Channel A boundary.
//
// Pattern numbers are the device's own (the byte at 0x407B); the connector
// never names them.
package mk312

import (
	"errors"
	"fmt"
	"math"
)

// Memory addresses of the device state the connector reads and writes.
const (
	AddressR15         = 0x400F
	AddressLevelA      = 0x4064
	AddressLevelB      = 0x4065
	AddressCommand     = 0x4070
	AddressPowerParam  = 0x4078
	AddressCurrentMode = 0x407B
	AddressMAMin       = 0x4086
	AddressMAMax       = 0x4087
	AddressPowerLevel  = 0x41F4
	AddressPulseWidth  = 0x41FE
	AddressBattery     = 0x4203
	AddressLevelMA     = 0x420D
	AddressKey         = 0x4213

	// Live state of the loaded pattern.
	AddressRoutineTimerLow  = 0x4088
	AddressRoutineTimerHigh = 0x4089
	AddressGateValueA       = 0x4090
	AddressModeRampValueA   = 0x409C

	// Channel A carries three parallel modulation blocks, nine bytes apart,
	// each laid out value, min, max, rate, step, ..., select at +7.
	AddressIntensityValueA = 0x40A5
	AddressIntensityMinA   = 0x40A6
	AddressIntensityMaxA   = 0x40A7
	AddressIntensityStepA  = 0x40A9
	AddressFreqValueA      = 0x40AE
	AddressFreqMinA        = 0x40AF
	AddressFreqMaxA        = 0x40B0
	AddressFreqStepA       = 0x40B2
	AddressSweepValueA     = 0x40B7
	AddressSweepMinA       = 0x40B8
	AddressSweepMaxA       = 0x40B9
	AddressSweepStepA      = 0x40BB
)

// Register 15 bits and the commands written to AddressCommand.
const (
	R15ADCDisable   = 1 << 0
	R15MAPotDisable = 1 << 3

	CommandExitMenu = 0x04
	CommandSetPower = 0x06
	CommandNewMode  = 0x12
)

// Power ranges as read at AddressPowerLevel, and the parameter written at
// AddressPowerParam before CommandSetPower to select each.
const (
	PowerLow    = 0x01
	PowerNormal = 0x02
	PowerHigh   = 0x03
)

var powerParam = map[int]byte{PowerLow: 0x6B, PowerNormal: 0x6C, PowerHigh: 0x6D}

// PowerOf is the range named as the service's protocol spells it
// (estim.PowerModeNormal, estim.PowerModeHigh); false for any other name,
// the low range included: the connector never arms in it.
func PowerOf(name string) (int, bool) {
	switch name {
	case "normal":
		return PowerNormal, true
	case "high":
		return PowerHigh, true
	}
	return 0, false
}

// PowerName is the range's name as the service's protocol spells it.
func PowerName(v int) string {
	switch v {
	case PowerLow:
		return "low"
	case PowerNormal:
		return "normal"
	case PowerHigh:
		return "high"
	}
	return fmt.Sprintf("unknown-%02x", v)
}

// Wire bytes.
const (
	BeaconByte  = 0x07 // sent continuously while the device waits for a host
	CmdRead     = 0x3C
	CmdWrite    = 0x4D
	CmdSetKey   = 0x2F
	AckWrite    = 0x06
	ReplyRead   = 0x22
	ReplySetKey = 0x21
	BoxKeyXOR   = 0x55
)

// NoKey is the session key before the key exchange: bytes go in the clear.
const NoKey = -1

// Pattern numbers: the byte at AddressCurrentMode. The device knows
// 0x76..0x8E; the service may select the eleven below, which need no audio
// input and do not split the channels.
const (
	ModeFirst = 0x76
	ModeLast  = 0x8E
)

// AllowedModes are the pattern numbers a command may select.
var AllowedModes = []int{0x76, 0x77, 0x78, 0x79, 0x7A, 0x7B, 0x80, 0x81, 0x82, 0x83, 0x84}

// KnownMode says whether m is a pattern number the device has.
func KnownMode(m int) bool { return m >= ModeFirst && m <= ModeLast }

// ModeAllowed says whether the service may select m.
func ModeAllowed(m int) bool {
	for _, a := range AllowedModes {
		if a == m {
			return true
		}
	}
	return false
}

// Patterns in which the tempo control has no effect.
var tempoInert = map[int]bool{0x80: true, 0x81: true}

// Scales.
const (
	LCDMax        = 99
	RAMMax        = 255
	MAPercentCap  = 100
	SweepPercent  = 100
	PulseWidthMin = 70
	PulseWidthMax = 250
)

// ProtocolError is a malformed or unexpected reply.
type ProtocolError struct{ msg string }

func (e *ProtocolError) Error() string { return "mk312: " + e.msg }

func protocolErr(format string, args ...any) error {
	return &ProtocolError{msg: fmt.Sprintf(format, args...)}
}

// IsProtocolError says whether err is a ProtocolError (the device answered,
// wrongly), as opposed to silence or a transport failure.
func IsProtocolError(err error) bool {
	var pe *ProtocolError
	return errors.As(err, &pe)
}

// ErrSilent is returned when not a byte came back: the device is off or the
// link plug is out. Deliberately not a ProtocolError: a reply that fails to
// decode says the session key is wrong; no reply says nothing about it.
var ErrSilent = errors.New("mk312: device did not answer; is it on and the link plug seated?")

// ErrHandshake is returned when the device does not complete the key
// exchange; ErrNoBeacon and ErrNoKeyExchange say at which step.
var (
	ErrHandshake     = errors.New("mk312: handshake failed")
	ErrNoBeacon      = fmt.Errorf("%w: the device did not reply 0x07; power it off for about ten seconds and retry", ErrHandshake)
	ErrNoKeyExchange = fmt.Errorf("%w: the device answers but never completes the key exchange; reseat the link plug, disconnect Bluetooth, then power-cycle the device", ErrHandshake)
)

// Checksum is the sum of the bytes modulo 256, computed before scrambling.
func Checksum(data []byte) byte {
	var sum int
	for _, b := range data {
		sum += int(b)
	}
	return byte(sum % 256)
}

func xorBytes(data []byte, key int) []byte {
	out := make([]byte, len(data))
	for i, b := range data {
		if key == NoKey {
			out[i] = b
		} else {
			out[i] = b ^ byte(key)
		}
	}
	return out
}

// EncodeSetKey is the key exchange request with the host's key (always 0
// here) under the session key (none, at the handshake).
func EncodeSetKey(hostKey byte, key int) []byte {
	p := []byte{CmdSetKey, hostKey}
	p = append(p, Checksum(p))
	return xorBytes(p, key)
}

// EncodeRead is a one-byte read of addr.
func EncodeRead(addr uint16, key int) []byte {
	p := []byte{CmdRead, byte(addr >> 8), byte(addr)}
	p = append(p, Checksum(p))
	return xorBytes(p, key)
}

// EncodeWrite is a one-byte write of value at addr.
func EncodeWrite(addr uint16, value byte, key int) []byte {
	p := []byte{CmdWrite, byte(addr >> 8), byte(addr), value}
	p = append(p, Checksum(p))
	return xorBytes(p, key)
}

// DecodeReply reads a three-byte reply (opcode, value, checksum) under key
// and checks its opcode and checksum.
func DecodeReply(raw []byte, key int, opcode byte) (byte, error) {
	if len(raw) != 3 {
		return 0, protocolErr("reply length %d, expected 3", len(raw))
	}
	plain := xorBytes(raw, key)
	if want := Checksum(plain[:2]); want != plain[2] {
		return 0, protocolErr("reply checksum mismatch: 0x%02x != 0x%02x", want, plain[2])
	}
	if plain[0] != opcode {
		return 0, protocolErr("reply opcode mismatch: 0x%02x != 0x%02x", plain[0], opcode)
	}
	return plain[1], nil
}

// LCDToRAM converts a front-panel level (0..99) to the memory byte.
func LCDToRAM(level int) byte {
	return byte(math.Round(float64(clamp(level, 0, LCDMax)) * RAMMax / LCDMax))
}

// RAMToLCD converts a memory byte to the front-panel level (0..99).
func RAMToLCD(raw int) int {
	return int(math.Round(float64(clamp(raw, 0, RAMMax)) * LCDMax / RAMMax))
}

// MAPercentToRaw maps a tempo percent onto the loaded pattern's range.
func MAPercentToRaw(percent, lo, hi int) (int, error) {
	if hi <= lo {
		return 0, protocolErr("degenerate tempo range [%d,%d]", lo, hi)
	}
	p := clamp(percent, 0, MAPercentCap)
	return lo + int(math.Round(float64(p)*float64(hi-lo)/MAPercentCap)), nil
}

// MARawToPercent maps a tempo byte back onto percent; false for a
// degenerate range.
func MARawToPercent(raw, lo, hi int) (int, bool) {
	if hi <= lo {
		return 0, false
	}
	p := int(math.Round(float64(raw-lo) * MAPercentCap / float64(hi-lo)))
	return clamp(p, 0, MAPercentCap), true
}

// PhasePercent is where a sweeping value sits between its floor and
// ceiling; false for a degenerate range, which is how a pattern that does
// not drive the block presents itself.
func PhasePercent(value, lo, hi int) (int, bool) {
	if hi <= lo {
		return 0, false
	}
	p := int(math.Round(float64(value-lo) * SweepPercent / float64(hi-lo)))
	return clamp(p, 0, SweepPercent), true
}

// SignedByte reinterprets a memory byte as the two's-complement delta the
// device stores.
func SignedByte(v byte) int {
	if v > 127 {
		return int(v) - 256
	}
	return int(v)
}

// SweepDirection is which way a modulation block travels, from its step.
func SweepDirection(step int) string {
	switch {
	case step > 0:
		return "rising"
	case step < 0:
		return "falling"
	}
	return "steady"
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// BatteryPercent converts the battery byte.
func BatteryPercent(raw byte) int {
	return int(math.Round(float64(raw) / 2.55))
}
