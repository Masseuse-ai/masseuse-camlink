// Package machosig removes the code signature from a Mach-O executable so
// that a signed macOS release binary can be compared with an unsigned
// rebuild (VERIFY.md, "The macOS binaries").
//
// A Developer ID signature is a per-build input: nobody else holds the key,
// so a rebuilt darwin binary can never match a release byte for byte. The Go
// project verifies its own macOS distributions the way this package
// enables: the binaries must match exactly once their signatures are
// removed. The approach follows StripDarwinSig in
// golang.org/x/build/cmd/gorebuild (Copyright 2023 The Go Authors,
// BSD-3-Clause): drop the LC_CODE_SIGNATURE load command, shrink the
// __LINKEDIT segment and cut the signature off the end of the file.
//
// One step is added to gorebuild's. A signing tool that finds a signature
// already in place (the Go linker writes an ad-hoc one into every
// darwin/arm64 binary) may zero it where it lies and append its own after,
// leaving a run of zeros between the last link-edit table and the new
// signature. Strip therefore cuts __LINKEDIT back to the end of its last
// table when everything past that point is zero. Stripping the signature
// the Go linker wrote, stripping a Developer ID signature written over it,
// and passing an unsigned darwin/amd64 binary through all yield the same
// bytes.
package machosig

import (
	"bytes"
	"debug/macho"
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	magic64          = 0xfeedfacf // 64-bit Mach-O, little endian
	headerSize       = 32
	codeSignatureLen = 16
	segment64Len     = 72
)

// Load commands whose payload lives in __LINKEDIT.
const (
	lcSymtab             = 0x2
	lcDysymtab           = 0xb
	lcSegment64          = 0x19
	lcCodeSignature      = 0x1d
	lcSegmentSplitInfo   = 0x1e
	lcDyldInfo           = 0x22
	lcDyldInfoOnly       = 0x80000022
	lcFunctionStarts     = 0x26
	lcDataInCode         = 0x29
	lcDylibCodeSignDRs   = 0x2b
	lcLinkerOptimization = 0x2e
	lcDyldExportsTrie    = 0x80000033
	lcDyldChainedFixups  = 0x80000034
	lcAtomInfo           = 0x36
)

// ErrNotMachO is returned for input that is not a 64-bit little-endian
// Mach-O file.
var ErrNotMachO = errors.New("machosig: not a 64-bit Mach-O executable")

// Strip returns a copy of data with its code signature removed and its
// __LINKEDIT segment cut back to the end of the last table it holds. Input
// with no signature and nothing but tables in __LINKEDIT (the unsigned
// binaries the Go linker writes for darwin/amd64) is returned unchanged, so
// signed and unsigned binaries can be passed through the same way before
// comparing them.
func Strip(data []byte) ([]byte, error) {
	if len(data) < headerSize || binary.LittleEndian.Uint32(data) != magic64 {
		return nil, ErrNotMachO
	}
	f, err := macho.NewFile(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("machosig: %w", err)
	}
	defer f.Close()
	if len(f.Loads) == 0 {
		return nil, errors.New("machosig: no load commands")
	}

	le := binary.LittleEndian
	out := append([]byte(nil), data...)
	end := int64(len(out))

	ncmds := int(le.Uint32(data[16:]))
	sizeofcmds := int64(le.Uint32(data[20:]))
	if ncmds != len(f.Loads) {
		return nil, errors.New("machosig: load command count mismatch")
	}

	// Walk the load commands: find the __LINKEDIT segment, the end of the
	// last table that lives in it, and the signature command, which is
	// always the last load command.
	var (
		off         = int64(headerSize)
		linkeditOff = int64(-1)
		sigCmdOff   = int64(-1)
		sigOff      int64
		sigSize     int64
		tableEnd    int64
	)
	tables := func(ends ...int64) {
		for _, e := range ends {
			tableEnd = max(tableEnd, e)
		}
	}
	for i := 0; i < ncmds; i++ {
		if off+8 > end {
			return nil, errors.New("machosig: load commands overrun the file")
		}
		cmd, size := le.Uint32(data[off:]), int64(le.Uint32(data[off+4:]))
		if size < 8 || off+size > end {
			return nil, errors.New("machosig: malformed load command")
		}
		c := data[off : off+size]
		u32 := func(at int) int64 { return int64(le.Uint32(c[at:])) }
		switch cmd {
		case lcSegment64:
			if size >= segment64Len && string(bytes.TrimRight(c[8:24], "\x00")) == "__LINKEDIT" {
				linkeditOff = off
			}
		case lcSymtab:
			if size >= 24 {
				tables(u32(8)+u32(12)*16, u32(16)+u32(20)) // nlist_64 entries, string table
			}
		case lcDysymtab:
			if size >= 80 {
				tables(u32(32)+u32(36)*8, u32(40)+u32(44)*56, u32(48)+u32(52)*4,
					u32(56)+u32(60)*4, u32(64)+u32(68)*8, u32(72)+u32(76)*8)
			}
		case lcDyldInfo, lcDyldInfoOnly:
			if size >= 48 {
				tables(u32(8)+u32(12), u32(16)+u32(20), u32(24)+u32(28), u32(32)+u32(36), u32(40)+u32(44))
			}
		case lcSegmentSplitInfo, lcFunctionStarts, lcDataInCode, lcDylibCodeSignDRs,
			lcLinkerOptimization, lcDyldExportsTrie, lcDyldChainedFixups, lcAtomInfo:
			if size >= 16 {
				tables(u32(8) + u32(12))
			}
		case lcCodeSignature:
			if size != codeSignatureLen || i != ncmds-1 {
				return nil, errors.New("machosig: LC_CODE_SIGNATURE is not the last load command")
			}
			sigCmdOff, sigOff, sigSize = off, u32(8), u32(12)
		}
		off += size
	}
	if off != headerSize+sizeofcmds {
		return nil, errors.New("machosig: sizeofcmds does not match the load commands")
	}
	if linkeditOff < 0 {
		return nil, errors.New("machosig: no __LINKEDIT segment")
	}
	linkeditFileOff := int64(le.Uint64(data[linkeditOff+40:]))
	linkeditFileSize := int64(le.Uint64(data[linkeditOff+48:]))
	linkeditEnd := linkeditFileOff + linkeditFileSize
	if linkeditFileOff < 0 || linkeditFileSize < 0 || linkeditEnd != end {
		return nil, fmt.Errorf("machosig: __LINKEDIT (%d+%d) does not end the %d-byte file", linkeditFileOff, linkeditFileSize, end)
	}

	// 1. The signature: the tail of the file, referenced only by the
	// command being removed.
	if sigCmdOff >= 0 {
		if sigOff < linkeditFileOff || sigOff+sigSize != end {
			return nil, fmt.Errorf("machosig: signature at %d+%d does not end the %d-byte file", sigOff, sigSize, end)
		}
		// The linker leaves the space after the load commands zeroed, so an
		// unsigned build has zeros where the command was.
		le.PutUint32(out[16:], uint32(ncmds-1))
		le.PutUint32(out[20:], uint32(sizeofcmds-codeSignatureLen))
		copy(out[sigCmdOff:sigCmdOff+codeSignatureLen], make([]byte, codeSignatureLen))
		end = sigOff
	}

	// 2. Zero filler between the last table and the signature (or the end).
	if tableEnd < linkeditFileOff {
		tableEnd = linkeditFileOff
	}
	if tableEnd > end {
		return nil, errors.New("machosig: a link-edit table overlaps the signature")
	}
	if isZero(out[tableEnd:end]) {
		end = tableEnd
	}

	// __LINKEDIT sizes. The Go linker sets vmsize equal to filesize; Apple's
	// signer rounds vmsize up to a page, so it is recomputed from filesize
	// rather than reduced.
	newSize := uint64(end - linkeditFileOff)
	le.PutUint64(out[linkeditOff+32:], newSize) // vmsize
	le.PutUint64(out[linkeditOff+48:], newSize) // filesize
	return out[:end], nil
}

func isZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}
