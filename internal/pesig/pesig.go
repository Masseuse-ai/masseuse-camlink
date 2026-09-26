// Package pesig reads the parts of a Windows executable (PE) that
// Authenticode signing touches, and removes a signature, so that a signed
// Windows release file can be compared with an unsigned rebuild (VERIFY.md,
// "The Windows package"). It is the Windows twin of internal/machosig.
//
// Signing a PE file changes three things and nothing else: the attribute
// certificate table (the signature) is appended to the end of the file,
// after zero bytes that pad the file to the eight-byte boundary the table
// must start on; the security entry of the optional header's data
// directory is pointed at it; and the optional header's checksum is
// recomputed. Strip undoes all three: the table and its padding are cut
// off, the entry and the checksum are zeroed. A Go-built executable has
// both at zero to begin with, so Strip of a signed release binary is the
// unsigned build byte for byte; for any other executable (ffmpeg, built
// with mingw-w64) the two sides are compared after Strip has been applied
// to both, since Strip zeroes the checksum whether or not the file was
// signed.
//
// Everything here is pure Go and runs on any operating system: the header
// is read by offset, the way the loader reads it.
package pesig

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// ErrNotPE is returned for a file that is not a Windows executable.
var ErrNotPE = errors.New("pesig: not a PE executable")

const (
	// FooterAlign is the alignment the attribute certificate table must
	// start on; a signing tool pads the file with zeros to reach it.
	FooterAlign = 8

	peOffsetAt         = 0x3c // e_lfanew in the DOS header
	coffHeaderSize     = 20
	optMagicPE32       = 0x10b
	optMagicPE32Plus   = 0x20b
	checksumOffset     = 64  // in the optional header, both formats
	rvaCountOffset32   = 92  // NumberOfRvaAndSizes, PE32
	rvaCountOffset64   = 108 // NumberOfRvaAndSizes, PE32+
	dataDirOffset32    = 96
	dataDirOffset64    = 112
	dirEntrySize       = 8
	securityDirIndex   = 4 // IMAGE_DIRECTORY_ENTRY_SECURITY
	sectionHeaderSize  = 40
	sectionRawSizeAt   = 16 // SizeOfRawData in a section header
	sectionRawOffsetAt = 20 // PointerToRawData
)

// Header is what Strip needs to know about an executable.
type Header struct {
	// PE32Plus says the optional header is the 64-bit form (PE32+).
	PE32Plus bool
	// ChecksumOffset is the file offset of the optional header's CheckSum.
	ChecksumOffset int64
	// SecurityEntryOffset is the file offset of the security entry in the
	// data directory (eight bytes: offset and size); zero when the header
	// declares fewer than five directories, in which case the file cannot
	// be signed.
	SecurityEntryOffset int64
	// SecurityOffset and SecuritySize locate the attribute certificate
	// table: a file offset (not a virtual address, for this one entry) and
	// its length; both zero when the file is unsigned.
	SecurityOffset, SecuritySize uint32
	// ImageEnd is where the image proper ends: the end of the last
	// section's raw data (or of the headers, for an image without one).
	// Anything after it is an overlay: alignment padding, a signature, or
	// data a packer appended (internal/payload).
	ImageEnd int64
}

// Signed says whether the header points at a certificate table.
func (h Header) Signed() bool { return h.SecuritySize != 0 }

// Parse reads the header of the executable in r, size bytes long.
func Parse(r io.ReaderAt, size int64) (Header, error) {
	var h Header
	head := make([]byte, 64)
	if size < int64(len(head)) {
		return h, ErrNotPE
	}
	if _, err := r.ReadAt(head, 0); err != nil {
		return h, fmt.Errorf("pesig: %w", err)
	}
	if head[0] != 'M' || head[1] != 'Z' {
		return h, ErrNotPE
	}
	peOff := int64(binary.LittleEndian.Uint32(head[peOffsetAt:]))
	// Signature, COFF header and the optional header's magic.
	fixed := make([]byte, 4+coffHeaderSize+2)
	if peOff < int64(len(head)) || peOff+int64(len(fixed)) > size {
		return h, ErrNotPE
	}
	if _, err := r.ReadAt(fixed, peOff); err != nil {
		return h, fmt.Errorf("pesig: %w", err)
	}
	if string(fixed[:4]) != "PE\x00\x00" {
		return h, ErrNotPE
	}
	coff := fixed[4:]
	sections := int64(binary.LittleEndian.Uint16(coff[2:]))
	optSize := int64(binary.LittleEndian.Uint16(coff[16:]))
	optOff := peOff + 4 + coffHeaderSize
	switch binary.LittleEndian.Uint16(fixed[4+coffHeaderSize:]) {
	case optMagicPE32:
	case optMagicPE32Plus:
		h.PE32Plus = true
	default:
		return h, ErrNotPE
	}
	if optOff+optSize > size {
		return h, fmt.Errorf("pesig: the optional header runs past the end of the file")
	}
	opt := make([]byte, optSize)
	if _, err := r.ReadAt(opt, optOff); err != nil {
		return h, fmt.Errorf("pesig: %w", err)
	}
	if optSize < checksumOffset+4 {
		return h, fmt.Errorf("pesig: the optional header is too short (%d bytes)", optSize)
	}
	h.ChecksumOffset = optOff + checksumOffset
	rvaCountAt, dirAt := int64(rvaCountOffset32), int64(dataDirOffset32)
	if h.PE32Plus {
		rvaCountAt, dirAt = rvaCountOffset64, dataDirOffset64
	}
	if optSize >= rvaCountAt+4 {
		dirs := int64(binary.LittleEndian.Uint32(opt[rvaCountAt:]))
		entryAt := dirAt + securityDirIndex*dirEntrySize
		if dirs > securityDirIndex && optSize >= entryAt+dirEntrySize {
			h.SecurityEntryOffset = optOff + entryAt
			h.SecurityOffset = binary.LittleEndian.Uint32(opt[entryAt:])
			h.SecuritySize = binary.LittleEndian.Uint32(opt[entryAt+4:])
		}
	}
	// The section table follows the optional header; the image ends where
	// the furthest section's raw data does.
	tableOff := optOff + optSize
	tableLen := sections * sectionHeaderSize
	if tableOff+tableLen > size {
		return h, fmt.Errorf("pesig: the section table runs past the end of the file")
	}
	table := make([]byte, tableLen)
	if _, err := r.ReadAt(table, tableOff); err != nil {
		return h, fmt.Errorf("pesig: %w", err)
	}
	h.ImageEnd = tableOff + tableLen
	for i := int64(0); i < sections; i++ {
		sec := table[i*sectionHeaderSize:]
		rawSize := int64(binary.LittleEndian.Uint32(sec[sectionRawSizeAt:]))
		rawOff := int64(binary.LittleEndian.Uint32(sec[sectionRawOffsetAt:]))
		if rawSize == 0 {
			continue
		}
		if end := rawOff + rawSize; end > h.ImageEnd {
			h.ImageEnd = end
		}
	}
	if h.ImageEnd > size {
		return h, fmt.Errorf("pesig: a section runs past the end of the file")
	}
	if h.Signed() {
		start, end := int64(h.SecurityOffset), int64(h.SecurityOffset)+int64(h.SecuritySize)
		if start < h.ImageEnd || end > size {
			return h, fmt.Errorf("pesig: the certificate table (%d+%d) lies outside the file's overlay (%d..%d)", h.SecurityOffset, h.SecuritySize, h.ImageEnd, size)
		}
	}
	return h, nil
}

// ParseBytes is Parse on a file held in memory.
func ParseBytes(data []byte) (Header, error) {
	return Parse(bytesReaderAt(data), int64(len(data)))
}

// Strip returns data without its Authenticode signature, normalized for
// comparison: the certificate table, which must end the file (where every
// signing tool puts it), is cut off along with the zero bytes that padded
// the file to its alignment; the security directory entry and the checksum
// are zeroed. An unsigned file passes through with the same two fields
// zeroed, so Strip applied to both sides of a comparison cancels a
// checksum a linker may have written. Bytes an executable carries after
// its image other than that padding (an appended payload) are kept; a
// certificate table anywhere but at the end is an error.
func Strip(data []byte) ([]byte, error) {
	h, err := ParseBytes(data)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(data))
	copy(out, data)
	if h.Signed() {
		end := int64(h.SecurityOffset) + int64(h.SecuritySize)
		if end != int64(len(out)) {
			return nil, fmt.Errorf("pesig: the certificate table ends at %d, the file at %d; only a signature at the end of the file is removed", end, len(out))
		}
		out = out[:h.SecurityOffset]
		// The signing tool's alignment padding: exactly the zero bytes that
		// take the image's end to the next boundary, and nothing else past
		// the image. Anything else there is somebody's overlay
		// (internal/payload knows its own footer and drops the padding
		// after it itself).
		pad := (FooterAlign - h.ImageEnd%FooterAlign) % FooterAlign
		if tail := out[h.ImageEnd:]; pad > 0 && int64(len(tail)) == pad && allZero(tail) {
			out = out[:h.ImageEnd]
		}
	}
	if h.SecurityEntryOffset != 0 {
		clear(out[h.SecurityEntryOffset : h.SecurityEntryOffset+dirEntrySize])
	}
	clear(out[h.ChecksumOffset : h.ChecksumOffset+4])
	return out, nil
}

// End is where the signable part of the file ends: the start of the
// certificate table when the file is signed, else the file's end. It is
// where a payload appended before signing would end (internal/payload).
func (h Header) End(size int64) int64 {
	if h.Signed() {
		return int64(h.SecurityOffset)
	}
	return size
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// bytesReaderAt is an io.ReaderAt over a slice (bytes.Reader also is; this
// keeps the import list to what the loader's view needs).
type bytesReaderAt []byte

func (b bytesReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(b)) {
		return 0, io.EOF
	}
	n := copy(p, b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
