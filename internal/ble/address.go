package ble

import (
	"fmt"
	"strings"
)

// A peripheral's Bluetooth device address, as the Windows backend names
// peripherals: six octets, upper-case hexadecimal, colon-separated
// ("AA:BB:CC:DD:EE:FF"), the form BlueZ uses on Linux, so an identifier
// remembered on one system reads the same on the other. WinRT hands the
// address over as a 48-bit integer.

// formatAddress writes a 48-bit address as AA:BB:CC:DD:EE:FF.
func formatAddress(addr uint64) string {
	return fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X",
		byte(addr>>40), byte(addr>>32), byte(addr>>24), byte(addr>>16), byte(addr>>8), byte(addr))
}

// parseAddress reads AA:BB:CC:DD:EE:FF (colons or dashes, either case) or
// twelve bare hexadecimal digits.
func parseAddress(s string) (uint64, bool) {
	s = strings.NewReplacer(":", "", "-", "").Replace(strings.TrimSpace(s))
	if len(s) != 12 {
		return 0, false
	}
	var addr uint64
	for _, r := range s {
		var v uint64
		switch {
		case '0' <= r && r <= '9':
			v = uint64(r - '0')
		case 'a' <= r && r <= 'f':
			v = uint64(r-'a') + 10
		case 'A' <= r && r <= 'F':
			v = uint64(r-'A') + 10
		default:
			return 0, false
		}
		addr = addr<<4 | v
	}
	return addr, true
}

// addressFromDeviceID reads the peripheral's address off a Windows device
// identifier such as
// "BluetoothLE#BluetoothLE00:1a:7d:da:71:13-c4:be:84:70:29:3f": the
// adapter's address comes first, the peripheral's after the last dash.
func addressFromDeviceID(id string) (uint64, bool) {
	i := strings.LastIndex(id, "-")
	if i < 0 {
		return 0, false
	}
	return parseAddress(id[i+1:])
}
