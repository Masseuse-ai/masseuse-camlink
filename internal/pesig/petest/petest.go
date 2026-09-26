// Package petest builds small Windows executables for tests: an image with
// one section holding whatever bytes the test wants, and the shape a
// signing tool leaves behind (a certificate table appended after
// alignment padding, the security entry and the checksum set), so that
// internal/pesig, internal/payload, the packer and the updater can be
// tested on any operating system without a compiler for Windows.
package petest

import (
	"encoding/binary"
)

const (
	peOffset       = 0x80
	sectionOffset  = 0x200
	optSizePE32    = 224
	optSizePE32P   = 240
	checksumOffset = 64
)

// Option shapes an Image.
type Option func(*options)

type options struct {
	pe32     bool
	checksum uint32
}

// PE32 makes a 32-bit image (the default is PE32+, what a windows/amd64
// build is).
func PE32() Option { return func(o *options) { o.pe32 = true } }

// Checksum sets the optional header's CheckSum, which a linker may write
// and a signing tool always does.
func Checksum(v uint32) Option { return func(o *options) { o.checksum = v } }

// Image is an executable with one section, .text, holding data. It parses
// under internal/pesig and debug/pe alike; it does not run.
func Image(data []byte, opts ...Option) []byte {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	optSize := optSizePE32P
	machine := uint16(0x8664)
	if o.pe32 {
		optSize = optSizePE32
		machine = 0x14c
	}
	img := make([]byte, sectionOffset+len(data))
	copy(img, "MZ")
	binary.LittleEndian.PutUint32(img[0x3c:], peOffset)
	copy(img[peOffset:], "PE\x00\x00")
	coff := img[peOffset+4:]
	binary.LittleEndian.PutUint16(coff[0:], machine)
	binary.LittleEndian.PutUint16(coff[2:], 1) // one section
	binary.LittleEndian.PutUint16(coff[16:], uint16(optSize))
	binary.LittleEndian.PutUint16(coff[18:], 0x22) // executable, large address aware
	opt := img[peOffset+4+20:]
	if o.pe32 {
		binary.LittleEndian.PutUint16(opt[0:], 0x10b)
		binary.LittleEndian.PutUint32(opt[92:], 16) // NumberOfRvaAndSizes
	} else {
		binary.LittleEndian.PutUint16(opt[0:], 0x20b)
		binary.LittleEndian.PutUint32(opt[108:], 16)
	}
	binary.LittleEndian.PutUint32(opt[32:], 0x1000) // SectionAlignment
	binary.LittleEndian.PutUint32(opt[36:], 0x200)  // FileAlignment
	binary.LittleEndian.PutUint32(opt[60:], sectionOffset)
	binary.LittleEndian.PutUint32(opt[checksumOffset:], o.checksum)
	binary.LittleEndian.PutUint16(opt[68:], 3) // console subsystem
	sec := img[peOffset+4+20+optSize:]
	copy(sec, ".text")
	binary.LittleEndian.PutUint32(sec[8:], uint32(len(data)))  // VirtualSize
	binary.LittleEndian.PutUint32(sec[12:], 0x1000)            // VirtualAddress
	binary.LittleEndian.PutUint32(sec[16:], uint32(len(data))) // SizeOfRawData
	binary.LittleEndian.PutUint32(sec[20:], sectionOffset)     // PointerToRawData
	binary.LittleEndian.PutUint32(sec[36:], 0x60000020)        // code, execute, read
	copy(img[sectionOffset:], data)
	return img
}

// Sign does to img what a signing tool does: pads the file with zeros to
// an eight-byte boundary, appends a certificate table of certLen bytes
// (its WIN_CERTIFICATE header filled, the rest the bytes 0xAB), points the
// security directory entry at it, and writes checksum into the optional
// header. The bytes before the padding are left as they are, so img may
// carry an overlay (a payload) already.
func Sign(img []byte, certLen int, checksum uint32) []byte {
	if certLen < 8 {
		certLen = 8
	}
	out := make([]byte, len(img))
	copy(out, img)
	for len(out)%8 != 0 {
		out = append(out, 0)
	}
	start := len(out)
	cert := make([]byte, certLen)
	binary.LittleEndian.PutUint32(cert[0:], uint32(certLen))
	binary.LittleEndian.PutUint16(cert[4:], 0x0200) // WIN_CERT_REVISION_2_0
	binary.LittleEndian.PutUint16(cert[6:], 0x0002) // WIN_CERT_TYPE_PKCS_SIGNED_DATA
	for i := 8; i < certLen; i++ {
		cert[i] = 0xAB
	}
	out = append(out, cert...)
	peOff := int64(binary.LittleEndian.Uint32(out[0x3c:]))
	optOff := peOff + 4 + 20
	opt := out[optOff:]
	entry := 112 + 4*8 // PE32+
	if binary.LittleEndian.Uint16(opt[0:]) == 0x10b {
		entry = 96 + 4*8
	}
	binary.LittleEndian.PutUint32(opt[entry:], uint32(start))
	binary.LittleEndian.PutUint32(opt[entry+4:], uint32(certLen))
	binary.LittleEndian.PutUint32(opt[checksumOffset:], checksum)
	return out
}
