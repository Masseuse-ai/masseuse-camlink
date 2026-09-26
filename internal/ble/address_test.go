package ble

import "testing"

func TestFormatAddress(t *testing.T) {
	if got := formatAddress(0xC4BE8470293F); got != "C4:BE:84:70:29:3F" {
		t.Errorf("formatAddress = %q", got)
	}
	if got := formatAddress(0); got != "00:00:00:00:00:00" {
		t.Errorf("formatAddress(0) = %q", got)
	}
}

func TestParseAddress(t *testing.T) {
	for _, in := range []string{"C4:BE:84:70:29:3F", "c4:be:84:70:29:3f", "C4-BE-84-70-29-3F", "c4be8470293f", " C4:BE:84:70:29:3F "} {
		got, ok := parseAddress(in)
		if !ok || got != 0xC4BE8470293F {
			t.Errorf("parseAddress(%q) = %x, %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "G-12AB", "MASTOGO G-12AB", "C4:BE:84:70:29", "C4:BE:84:70:29:3F:00", "zz:be:84:70:29:3f", "/dev/cu.usbserial-AB0JQ5W9", "3B5C0E2A-9D1F-4B7E-8A6C-1F2E3D4C5B6A"} {
		if _, ok := parseAddress(in); ok {
			t.Errorf("parseAddress(%q) accepted", in)
		}
	}
}

func TestAddressFromDeviceID(t *testing.T) {
	got, ok := addressFromDeviceID("BluetoothLE#BluetoothLE00:1a:7d:da:71:13-c4:be:84:70:29:3f")
	if !ok || got != 0xC4BE8470293F {
		t.Errorf("addressFromDeviceID = %x, %v", got, ok)
	}
	for _, in := range []string{"", "BluetoothLE#BluetoothLE00:1a:7d:da:71:13", "USB\\VID_0403&PID_6001\\AB0JQ5W9A"} {
		if _, ok := addressFromDeviceID(in); ok {
			t.Errorf("addressFromDeviceID(%q) accepted", in)
		}
	}
}

func TestAddressRoundTrip(t *testing.T) {
	for _, addr := range []uint64{0, 1, 0xFFFFFFFFFFFF, 0xC4BE8470293F, 0x0000A1B2C3D4} {
		got, ok := parseAddress(formatAddress(addr))
		if !ok || got != addr {
			t.Errorf("round trip %x -> %q -> %x, %v", addr, formatAddress(addr), got, ok)
		}
	}
}
