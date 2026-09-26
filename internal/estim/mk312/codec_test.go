package mk312_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/FemLed/masseuse-camlink/internal/estim/mk312"
)

// The vectors the reference implementation checks itself against.
func TestCodecVectors(t *testing.T) {
	if got := mk312.EncodeRead(0x407B, mk312.NoKey); !bytes.Equal(got, []byte{0x3C, 0x40, 0x7B, 0xF7}) {
		t.Errorf("read packet = % x", got)
	}
	if got := mk312.EncodeWrite(0x4064, 0x20, mk312.NoKey); !bytes.Equal(got, []byte{0x4D, 0x40, 0x64, 0x20, 0x11}) {
		t.Errorf("write packet = % x", got)
	}
	if got := mk312.EncodeSetKey(0, mk312.NoKey); !bytes.Equal(got, []byte{0x2F, 0x00, 0x2F}) {
		t.Errorf("set key packet = % x", got)
	}
	// Every host byte, checksum included, is scrambled with the key.
	if got := mk312.EncodeRead(0x407B, 0x17); !bytes.Equal(got, []byte{0x3C ^ 0x17, 0x40 ^ 0x17, 0x7B ^ 0x17, 0xF7 ^ 0x17}) {
		t.Errorf("keyed read packet = % x", got)
	}
	if mk312.LCDToRAM(99) != 255 || mk312.RAMToLCD(255) != 99 || mk312.RAMToLCD(int(mk312.LCDToRAM(85))) != 85 {
		t.Error("level scale conversions")
	}
	for _, c := range []struct {
		percent, lo, hi, raw int
	}{{0, 1, 64, 1}, {100, 1, 64, 64}, {50, 1, 64, 33}} {
		raw, err := mk312.MAPercentToRaw(c.percent, c.lo, c.hi)
		if err != nil || raw != c.raw {
			t.Errorf("MAPercentToRaw(%d,%d,%d) = %d, %v", c.percent, c.lo, c.hi, raw, err)
		}
	}
	if _, err := mk312.MAPercentToRaw(50, 64, 64); !mk312.IsProtocolError(err) {
		t.Errorf("degenerate range: %v", err)
	}
	if p, ok := mk312.MARawToPercent(1, 1, 64); !ok || p != 0 {
		t.Error("ma raw min")
	}
	if p, ok := mk312.MARawToPercent(64, 1, 64); !ok || p != 100 {
		t.Error("ma raw max")
	}
	if _, ok := mk312.MARawToPercent(10, 64, 64); ok {
		t.Error("ma degenerate range should not convert")
	}
	for _, c := range []struct {
		v, lo, hi, want int
		ok              bool
	}{{50, 50, 200, 0, true}, {200, 50, 200, 100, true}, {125, 50, 200, 50, true}, {10, 50, 200, 0, true}, {68, 9, 128, 50, true}, {200, 255, 255, 0, false}} {
		p, ok := mk312.PhasePercent(c.v, c.lo, c.hi)
		if ok != c.ok || p != c.want {
			t.Errorf("PhasePercent(%d,%d,%d) = %d, %v", c.v, c.lo, c.hi, p, ok)
		}
	}
	if mk312.SignedByte(2) != 2 || mk312.SignedByte(254) != -2 {
		t.Error("signed byte")
	}
	if mk312.SweepDirection(2) != "rising" || mk312.SweepDirection(-2) != "falling" || mk312.SweepDirection(0) != "steady" {
		t.Error("sweep direction")
	}
	if mk312.BatteryPercent(255) != 100 || mk312.BatteryPercent(0) != 0 {
		t.Error("battery percent")
	}
	if mk312.PowerName(mk312.PowerHigh) != "high" || mk312.PowerName(9) != "unknown-09" {
		t.Error("power names")
	}
}

func TestDecodeReply(t *testing.T) {
	v, err := mk312.DecodeReply([]byte{0x22, 0x76, 0x98}, mk312.NoKey, mk312.ReplyRead)
	if err != nil || v != 0x76 {
		t.Fatalf("plaintext read reply: %d, %v", v, err)
	}
	if _, err := mk312.DecodeReply([]byte{0x22, 0x76, 0x99}, mk312.NoKey, mk312.ReplyRead); !mk312.IsProtocolError(err) {
		t.Fatalf("bad checksum: %v", err)
	}
	if _, err := mk312.DecodeReply([]byte{0x21, 0x76, 0x97}, mk312.NoKey, mk312.ReplyRead); !mk312.IsProtocolError(err) {
		t.Fatalf("wrong opcode: %v", err)
	}
	if _, err := mk312.DecodeReply([]byte{0x07}, mk312.NoKey, mk312.ReplyRead); !mk312.IsProtocolError(err) {
		t.Fatalf("short reply: %v", err)
	}
	if mk312.IsProtocolError(errors.New("other")) || mk312.IsProtocolError(mk312.ErrSilent) {
		t.Fatal("IsProtocolError is too broad")
	}
}

func TestModes(t *testing.T) {
	if len(mk312.AllowedModes) != 11 {
		t.Fatalf("allowed patterns = %d", len(mk312.AllowedModes))
	}
	for _, m := range mk312.AllowedModes {
		if !mk312.KnownMode(m) || !mk312.ModeAllowed(m) {
			t.Errorf("pattern 0x%02x", m)
		}
	}
	// Audio, split, phase and user patterns exist but are never selected.
	for _, m := range []int{0x7C, 0x7D, 0x7E, 0x7F, 0x85, 0x86, 0x87, 0x88, 0x8E} {
		if !mk312.KnownMode(m) || mk312.ModeAllowed(m) {
			t.Errorf("pattern 0x%02x should be known and not allowed", m)
		}
	}
	if mk312.KnownMode(0x75) || mk312.KnownMode(0x8F) || mk312.ModeAllowed(0) {
		t.Error("pattern range")
	}
}

// FuzzDecodeReply: a reply of any bytes under any key never panics, and a
// reply the codec produced itself always decodes to what was encoded.
func FuzzDecodeReply(f *testing.F) {
	f.Add([]byte{0x22, 0x10, 0x32}, 0, byte(0x22))
	f.Add([]byte{0x22 ^ 0x55, 0x10 ^ 0x55, 0x32 ^ 0x55}, 0x55, byte(0x22))
	f.Add([]byte{0x07}, -1, byte(0x06))
	f.Add([]byte{}, 3, byte(0x21))
	f.Fuzz(func(t *testing.T, raw []byte, key int, opcode byte) {
		key = clampKey(key)
		if _, err := mk312.DecodeReply(raw, key, opcode); err != nil && !mk312.IsProtocolError(err) {
			t.Fatalf("not a protocol error: %v", err)
		}
		if len(raw) < 2 {
			return
		}
		plain := []byte{opcode, raw[1]}
		plain = append(plain, mk312.Checksum(plain))
		wire := make([]byte, 3)
		for i, b := range plain {
			wire[i] = b
			if key != mk312.NoKey {
				wire[i] = b ^ byte(key)
			}
		}
		v, err := mk312.DecodeReply(wire, key, opcode)
		if err != nil || v != raw[1] {
			t.Fatalf("round trip under key %d: %d %v", key, v, err)
		}
	})
}

// clampKey folds a fuzzed int onto the codec's key domain: none, or a byte.
func clampKey(key int) int {
	if key < 0 {
		return mk312.NoKey
	}
	return key & 0xFF
}
