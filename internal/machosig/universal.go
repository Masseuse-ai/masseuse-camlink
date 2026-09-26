package machosig

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// A universal ("fat") binary, as lipo writes it and as the macOS
// application bundle's executable is, holds one thin Mach-O per
// architecture behind a big-endian header: the magic, the slice count, and
// per slice its CPU type and subtype, file offset, size and alignment. The
// slices are what a rebuild is compared with, one architecture at a time.

const (
	fatMagic     = 0xcafebabe // big endian
	fatHeaderLen = 8
	fatArchLen   = 20

	cpuTypeX86_64 = 0x01000007
	cpuTypeARM64  = 0x0100000c
)

// ErrUniversal is returned by Strip for a universal binary: its slices are
// stripped one by one (StripAll).
var ErrUniversal = errors.New("machosig: universal binary; strip its slices one at a time")

// Slice is one architecture's Mach-O: the whole of a thin binary, or one
// slice of a universal one. Arch uses Go's names (arm64, amd64), which are
// the release archives' (masseuse-camlink_<version>_darwin_<arch>).
type Slice struct {
	Arch string
	Data []byte
}

// IsUniversal reports whether data starts with a fat header.
func IsUniversal(data []byte) bool {
	return len(data) >= fatHeaderLen && binary.BigEndian.Uint32(data) == fatMagic
}

// Slices splits data into its architectures: a thin Mach-O is one slice
// (its architecture read from its own header), a universal binary one per
// entry of its fat header. Each slice must be a 64-bit Mach-O.
func Slices(data []byte) ([]Slice, error) {
	if !IsUniversal(data) {
		if len(data) < headerSize || binary.LittleEndian.Uint32(data) != magic64 {
			return nil, ErrNotMachO
		}
		return []Slice{{Arch: archName(binary.LittleEndian.Uint32(data[4:])), Data: data}}, nil
	}
	be := binary.BigEndian
	n := int(be.Uint32(data[4:]))
	if n == 0 || n > 16 || fatHeaderLen+n*fatArchLen > len(data) {
		return nil, fmt.Errorf("machosig: fat header names %d slices", n)
	}
	slices := make([]Slice, 0, n)
	for i := range n {
		e := data[fatHeaderLen+i*fatArchLen:]
		cpu, off, size := be.Uint32(e), int64(be.Uint32(e[8:])), int64(be.Uint32(e[12:]))
		if off < fatHeaderLen+int64(n)*fatArchLen || size < headerSize || off+size > int64(len(data)) {
			return nil, fmt.Errorf("machosig: slice %d (%s) at %d+%d does not fit the %d-byte file", i, archName(cpu), off, size, len(data))
		}
		slice := data[off : off+size]
		if binary.LittleEndian.Uint32(slice) != magic64 {
			return nil, fmt.Errorf("machosig: slice %d (%s) is not a 64-bit Mach-O", i, archName(cpu))
		}
		if got := binary.LittleEndian.Uint32(slice[4:]); got != cpu {
			return nil, fmt.Errorf("machosig: slice %d is %s in the fat header and %s in its own", i, archName(cpu), archName(got))
		}
		slices = append(slices, Slice{Arch: archName(cpu), Data: slice})
	}
	return slices, nil
}

// StripAll is Strip for every architecture in data: each slice of a
// universal binary, or the one slice of a thin one, with its code signature
// removed.
func StripAll(data []byte) ([]Slice, error) {
	slices, err := Slices(data)
	if err != nil {
		return nil, err
	}
	for i := range slices {
		stripped, err := Strip(slices[i].Data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", slices[i].Arch, err)
		}
		slices[i].Data = stripped
	}
	return slices, nil
}

func archName(cpu uint32) string {
	switch cpu {
	case cpuTypeARM64:
		return "arm64"
	case cpuTypeX86_64:
		return "amd64"
	default:
		return fmt.Sprintf("cpu%#x", cpu)
	}
}
