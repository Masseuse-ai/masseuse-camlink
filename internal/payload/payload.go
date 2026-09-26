// Package payload is the Windows package's appended payload: the files the
// connector needs beside it on Windows (ffmpeg.exe, the unit driver
// helpers, the notices) carried inside the one executable people download,
// so that Masseuse.exe runs from wherever it lies, opened from inside
// the zip preview of Explorer, from Downloads or from a USB stick alike,
// with nothing to extract and nothing to lose (packaging/windows).
//
// The payload is appended after the executable's image and before its
// Authenticode signature, in a form the loader ignores and a signature
// covers:
//
//	[PE image, byte for byte the published connector]
//	[file 1][file 2]...[file n]        the files, contiguous, in manifest order
//	[zero padding, fewer than 8 bytes]  so the whole ends on an 8-byte boundary
//	[manifest]                          JSON: version, and each file's name, size and SHA-256
//	[footer, 32 bytes]                  magic, payload length, manifest length, reserved
//	[Authenticode signature]            added afterwards by the signing tool, if signed
//
// The footer is found from the end of the signable part of the file (the
// start of the certificate table when signed, the end of the file when
// not; internal/pesig), so the same reader serves a signed and an unsigned
// package. Everything before the payload is the published windows_amd64
// connector byte for byte: cmd/pestrip removes the signature and the
// payload and proves it (VERIFY.md, "The Windows package").
//
// The connector unpacks the payload under its state directory the first
// time it runs (cmd/masseuse-camlink), checking every file against the
// manifest's hash, and finds ffmpeg and the helpers there. A payload is
// read only from the program's own executable, whose integrity is what the
// signature and the release's checksums establish; the hashes inside are
// against a damaged unpack, not a hostile one.
package payload

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/FemLed/masseuse-camlink/internal/pesig"
)

const (
	// Magic opens the footer; the digit is the format.
	Magic = "MSAIPKG1"
	// FooterSize is the footer's length: Magic, three uint64s.
	FooterSize = 32
	// Format is the manifest format this package reads and writes.
	Format = 1
	// MaxManifest bounds the manifest; MaxFiles the file count; MaxTotal
	// what the files may add up to.
	MaxManifest = 1 << 20
	MaxFiles    = 256
	MaxTotal    = 512 << 20
	// MaxName bounds a file name.
	MaxName = 200
)

// ErrNone says the executable carries no payload.
var ErrNone = errors.New("payload: the executable carries no payload")

// File is one file of the payload, as the manifest names it.
type File struct {
	// Name is the file's path inside the unpacked payload, with forward
	// slashes: ffmpeg.exe, units/camlink-unit-mk312.exe, licenses/....
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest describes the payload.
type Manifest struct {
	Format int `json:"format"`
	// Version is the release the package is from ("0.0.0" for a local build).
	Version string `json:"version"`
	Files   []File `json:"files"`
}

// Entry is a file to pack.
type Entry struct {
	Name string
	Data []byte
}

// Append returns exe with the payload of entries appended. exe must be an
// unsigned executable without an overlay: the connector as published. The
// result's length is a multiple of pesig.FooterAlign, so a signing tool
// appends its certificate table right after the footer with no padding.
func Append(exe []byte, version string, entries []Entry) ([]byte, error) {
	h, err := pesig.ParseBytes(exe)
	if err != nil {
		return nil, err
	}
	if h.Signed() {
		return nil, errors.New("payload: the executable is signed; a payload goes on before the signature")
	}
	if h.ImageEnd != int64(len(exe)) {
		if _, err := LocateBytes(exe); err == nil {
			return nil, errors.New("payload: the executable already carries a payload")
		}
		return nil, fmt.Errorf("payload: the executable carries %d bytes past its image", int64(len(exe))-h.ImageEnd)
	}
	if len(entries) == 0 {
		return nil, errors.New("payload: nothing to pack")
	}
	if len(entries) > MaxFiles {
		return nil, fmt.Errorf("payload: %d files, more than %d", len(entries), MaxFiles)
	}
	m := Manifest{Format: Format, Version: version}
	seen := map[string]bool{}
	var total int64
	for _, e := range entries {
		if err := validName(e.Name); err != nil {
			return nil, err
		}
		if seen[e.Name] {
			return nil, fmt.Errorf("payload: %s is packed twice", e.Name)
		}
		seen[e.Name] = true
		sum := sha256.Sum256(e.Data)
		m.Files = append(m.Files, File{Name: e.Name, Size: int64(len(e.Data)), SHA256: hex.EncodeToString(sum[:])})
		total += int64(len(e.Data))
	}
	if total > MaxTotal {
		return nil, fmt.Errorf("payload: %d bytes of files, more than %d", total, MaxTotal)
	}
	manifest, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	if len(manifest) > MaxManifest {
		return nil, fmt.Errorf("payload: the manifest is %d bytes, more than %d", len(manifest), MaxManifest)
	}
	out := make([]byte, 0, len(exe)+int(total)+len(manifest)+FooterSize+pesig.FooterAlign)
	out = append(out, exe...)
	for _, e := range entries {
		out = append(out, e.Data...)
	}
	pad := (pesig.FooterAlign - (len(out)+len(manifest)+FooterSize)%pesig.FooterAlign) % pesig.FooterAlign
	out = append(out, make([]byte, pad)...)
	out = append(out, manifest...)
	footer := make([]byte, FooterSize)
	copy(footer, Magic)
	binary.LittleEndian.PutUint64(footer[8:], uint64(len(out)-len(exe)))
	binary.LittleEndian.PutUint64(footer[16:], uint64(len(manifest)))
	out = append(out, footer...)
	return out, nil
}

// validName says whether a manifest name is a plain relative path with
// forward slashes: no volume, no leading slash, no "." or ".." segment, no
// backslash, colon or control character.
func validName(name string) error {
	switch {
	case name == "", len(name) > MaxName:
		return fmt.Errorf("payload: file name %q is empty or too long", name)
	case strings.HasPrefix(name, "/"), strings.ContainsAny(name, "\\:*?\"<>|"), path.Clean(name) != name:
		return fmt.Errorf("payload: file name %q is not a plain relative path", name)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("payload: file name %q is not a plain relative path", name)
		}
	}
	if !utf8.ValidString(name) || strings.ContainsRune(name, utf8.RuneError) {
		return fmt.Errorf("payload: file name %q is not valid UTF-8", name)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("payload: file name %q has a control character", name)
		}
	}
	return nil
}

// Info is a located payload.
type Info struct {
	Manifest Manifest
	// Start is the file offset of the first payload byte; End is the
	// offset just past the footer (where a signature would begin).
	Start, End int64

	manifest []byte
	offsets  []int64
}

// Locate finds the payload in the executable r, size bytes long. ErrNone
// when there is none; pesig.ErrNotPE when r is not a Windows executable.
func Locate(r io.ReaderAt, size int64) (*Info, error) {
	h, err := pesig.Parse(r, size)
	if err != nil {
		return nil, err
	}
	end := h.End(size)
	// The footer ends the signable part of the file, or lies up to seven
	// zero bytes before it (a signing tool that padded a file this package
	// had already aligned; not signtool's doing, but cheap to allow).
	for k := int64(0); k < pesig.FooterAlign; k++ {
		off := end - FooterSize - k
		if off < h.ImageEnd {
			break
		}
		footer := make([]byte, FooterSize+k)
		if _, err := r.ReadAt(footer, off); err != nil {
			return nil, fmt.Errorf("payload: %w", err)
		}
		if string(footer[:8]) != Magic {
			continue
		}
		if !allZero(footer[FooterSize:]) {
			return nil, errors.New("payload: bytes between the footer and the signature")
		}
		return locate(r, h, off, footer[:FooterSize])
	}
	return nil, ErrNone
}

func locate(r io.ReaderAt, h pesig.Header, footerOff int64, footer []byte) (*Info, error) {
	payloadLen := binary.LittleEndian.Uint64(footer[8:])
	manifestLen := binary.LittleEndian.Uint64(footer[16:])
	if binary.LittleEndian.Uint64(footer[24:]) != 0 {
		return nil, errors.New("payload: the footer's reserved field is not zero")
	}
	if manifestLen == 0 || manifestLen > MaxManifest || payloadLen < manifestLen || payloadLen > uint64(footerOff) {
		return nil, errors.New("payload: the footer's lengths are out of range")
	}
	start := footerOff - int64(payloadLen)
	if start < h.ImageEnd {
		return nil, errors.New("payload: the payload would start inside the image")
	}
	manifest := make([]byte, manifestLen)
	if _, err := r.ReadAt(manifest, footerOff-int64(manifestLen)); err != nil {
		return nil, fmt.Errorf("payload: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		return nil, fmt.Errorf("payload: the manifest does not parse: %w", err)
	}
	if m.Format != Format {
		return nil, fmt.Errorf("payload: manifest format %d, this program reads %d", m.Format, Format)
	}
	if len(m.Files) == 0 || len(m.Files) > MaxFiles {
		return nil, fmt.Errorf("payload: the manifest names %d files", len(m.Files))
	}
	in := &Info{Manifest: m, Start: start, End: footerOff + FooterSize, manifest: manifest}
	seen := map[string]bool{}
	off := start
	var total int64
	for _, f := range m.Files {
		if err := validName(f.Name); err != nil {
			return nil, err
		}
		if seen[f.Name] {
			return nil, fmt.Errorf("payload: the manifest names %s twice", f.Name)
		}
		seen[f.Name] = true
		if f.Size < 0 || len(f.SHA256) != 64 {
			return nil, fmt.Errorf("payload: the manifest's entry for %s is malformed", f.Name)
		}
		if _, err := hex.DecodeString(f.SHA256); err != nil {
			return nil, fmt.Errorf("payload: the manifest's hash for %s is not hex", f.Name)
		}
		in.offsets = append(in.offsets, off)
		off += f.Size
		total += f.Size
		if total > MaxTotal {
			return nil, fmt.Errorf("payload: the files add up to more than %d bytes", MaxTotal)
		}
	}
	manifestOff := footerOff - int64(manifestLen)
	if pad := manifestOff - off; pad < 0 || pad >= pesig.FooterAlign {
		return nil, fmt.Errorf("payload: the files end %d bytes from the manifest", pad)
	}
	return in, nil
}

// LocateBytes is Locate on a file held in memory.
func LocateBytes(data []byte) (*Info, error) {
	return Locate(bytes.NewReader(data), int64(len(data)))
}

// ID names the payload's contents: the first sixteen hex digits of the
// manifest's SHA-256. Two packages with the same files, byte for byte,
// have the same ID; the unpacked directory is named by it.
func (in *Info) ID() string {
	sum := sha256.Sum256(in.manifest)
	return hex.EncodeToString(sum[:8])
}

// Open is a reader over one file of the payload, by manifest name.
func (in *Info) Open(r io.ReaderAt, name string) (*io.SectionReader, bool) {
	for i, f := range in.Manifest.Files {
		if f.Name == name {
			return io.NewSectionReader(r, in.offsets[i], f.Size), true
		}
	}
	return nil, false
}

// Verify hashes every file in the payload against the manifest.
func (in *Info) Verify(r io.ReaderAt) error {
	for i, f := range in.Manifest.Files {
		if err := checkHash(io.NewSectionReader(r, in.offsets[i], f.Size), f); err != nil {
			return err
		}
	}
	return nil
}

func checkHash(r io.Reader, f File) error {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return fmt.Errorf("payload: reading %s: %w", f.Name, err)
	}
	if n != f.Size {
		return fmt.Errorf("payload: %s is %d bytes, the manifest says %d", f.Name, n, f.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != f.SHA256 {
		return fmt.Errorf("payload: %s: sha256 %s, the manifest says %s", f.Name, got, f.SHA256)
	}
	return nil
}

// Extract writes the payload's files under dir (created as needed), each
// checked against the manifest as it is written; an error leaves whatever
// was written for the caller to remove. Executables get mode 0755, the
// rest 0644 (Windows ignores both).
func (in *Info) Extract(r io.ReaderAt, dir string) error {
	for i, f := range in.Manifest.Files {
		p := filepath.Join(dir, filepath.FromSlash(f.Name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return fmt.Errorf("payload: %w", err)
		}
		perm := os.FileMode(0o644)
		if strings.EqualFold(path.Ext(f.Name), ".exe") {
			perm = 0o755
		}
		w, err := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
		if err != nil {
			return fmt.Errorf("payload: %w", err)
		}
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(w, h), io.NewSectionReader(r, in.offsets[i], f.Size))
		if cerr := w.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("payload: writing %s: %w", f.Name, err)
		}
		if n != f.Size || hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
			return fmt.Errorf("payload: %s does not match the manifest", f.Name)
		}
	}
	return nil
}

// Check says whether dir holds every file of the payload, each with the
// manifest's size and hash: an unpack that is complete and undamaged.
func (in *Info) Check(dir string) error {
	for _, f := range in.Manifest.Files {
		p := filepath.Join(dir, filepath.FromSlash(f.Name))
		r, err := os.Open(p)
		if err != nil {
			return fmt.Errorf("payload: %w", err)
		}
		err = checkHash(r, f)
		_ = r.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// Strip returns data without its payload: the executable as it was before
// Append, the signing tool's padding after the footer gone with it. Data
// without a payload passes through unchanged. The signature, if any, must
// have been removed first (pesig.Strip), since it follows the payload.
func Strip(data []byte) ([]byte, error) {
	in, err := LocateBytes(data)
	if errors.Is(err, ErrNone) {
		return data, nil
	}
	if err != nil {
		return nil, err
	}
	return data[:in.Start], nil
}

// Carries says whether the executable at path carries a payload.
func Carries(path string) bool {
	r, err := OpenFile(path)
	if err != nil {
		return false
	}
	_ = r.Close()
	return true
}

// Reader is an executable open for its payload.
type Reader struct {
	Info *Info
	f    *os.File
}

// OpenFile opens the executable at path and locates its payload.
func OpenFile(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	in, err := Locate(f, st.Size())
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &Reader{Info: in, f: f}, nil
}

// Extract is Info.Extract from the open executable.
func (r *Reader) Extract(dir string) error { return r.Info.Extract(r.f, dir) }

// Verify is Info.Verify on the open executable.
func (r *Reader) Verify() error { return r.Info.Verify(r.f) }

// Close closes the executable.
func (r *Reader) Close() error { return r.f.Close() }

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
