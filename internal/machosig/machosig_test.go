package machosig

import (
	"bytes"
	"crypto/rand"
	"debug/macho"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// build cross-compiles a small program for darwin/arch the way releases are
// built and returns the binary. The Go linker signs darwin/arm64 ad hoc and
// leaves darwin/amd64 unsigned.
func build(t *testing.T, arch string) []byte {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go is not on PATH")
	}
	dir := t.TempDir()
	src := "package main\n\nimport \"os\"\n\nfunc main() { os.Stdout.WriteString(\"hello\\n\") }\n"
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module hello\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "hello_"+arch)
	cmd := exec.Command(goBin, "build", "-trimpath", "-buildvcs=false", "-ldflags=-s -w -buildid=", "-o", out, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS=darwin", "GOARCH="+arch, "CGO_ENABLED=0", "GOFLAGS=", "GOWORK=off")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build darwin/%s: %v\n%s", arch, err, b)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func hasSignature(t *testing.T, data []byte) bool {
	t.Helper()
	f, err := macho.NewFile(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("not a Mach-O: %v", err)
	}
	defer f.Close()
	last := f.Loads[len(f.Loads)-1].Raw()
	return binary.LittleEndian.Uint32(last) == lcCodeSignature
}

func linkedit(t *testing.T, data []byte) *macho.Segment {
	t.Helper()
	f, err := macho.NewFile(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("not a Mach-O: %v", err)
	}
	defer f.Close()
	seg := f.Segment("__LINKEDIT")
	if seg == nil {
		t.Fatal("no __LINKEDIT")
	}
	return seg
}

func TestUnsignedPassesThrough(t *testing.T) {
	data := build(t, "amd64")
	if hasSignature(t, data) {
		t.Skip("this linker signs darwin/amd64; nothing to check here")
	}
	got, err := Strip(data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("unsigned binary changed: %d -> %d bytes", len(data), len(got))
	}
}

func TestStripAdHoc(t *testing.T) {
	data := build(t, "arm64")
	if !hasSignature(t, data) {
		t.Fatal("expected the linker's ad-hoc signature on darwin/arm64")
	}
	got, err := Strip(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) >= len(data) {
		t.Fatalf("stripped binary is not shorter: %d >= %d", len(got), len(data))
	}
	if hasSignature(t, got) {
		t.Fatal("LC_CODE_SIGNATURE still present")
	}
	seg := linkedit(t, got)
	if seg.Filesz != seg.Memsz || seg.Offset+seg.Filesz != uint64(len(got)) {
		t.Fatalf("__LINKEDIT off=%d filesz=%d memsz=%d for a %d-byte file", seg.Offset, seg.Filesz, seg.Memsz, len(got))
	}
	// The header's command count and size shrank by exactly one command.
	if binary.LittleEndian.Uint32(got[16:])+1 != binary.LittleEndian.Uint32(data[16:]) ||
		binary.LittleEndian.Uint32(got[20:])+codeSignatureLen != binary.LittleEndian.Uint32(data[20:]) {
		t.Fatal("header ncmds/sizeofcmds not adjusted by one LC_CODE_SIGNATURE")
	}
	again, err := Strip(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, got) {
		t.Fatal("Strip is not idempotent")
	}
}

// resign imitates a signing tool applied to an already signed binary the
// way quill and Apple's codesign do: the old signature is zeroed where it
// lies (quill) or dropped, a larger one is appended, LC_CODE_SIGNATURE and
// __LINKEDIT are updated, and __LINKEDIT's vmsize is rounded up to a page
// (Apple).
func resign(t *testing.T, data []byte, zeroOld bool) []byte {
	t.Helper()
	le := binary.LittleEndian
	out := append([]byte(nil), data...)
	f, err := macho.NewFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	last := f.Loads[len(f.Loads)-1].Raw()
	if le.Uint32(last) != lcCodeSignature {
		t.Fatal("input is not signed")
	}
	sizeofcmds := int(le.Uint32(data[20:]))
	cmdOff := headerSize + bytes.Index(data[headerSize:headerSize+sizeofcmds], last)
	oldOff, oldSize := int(le.Uint32(last[8:])), int(le.Uint32(last[12:]))
	segOff := headerSize + bytes.Index(data[headerSize:headerSize+sizeofcmds], append([]byte("__LINKEDIT"), make([]byte, 6)...)) - 8
	segFileOff := le.Uint64(data[segOff+40:])

	var newOff int
	if zeroOld {
		copy(out[oldOff:oldOff+oldSize], make([]byte, oldSize))
		newOff = len(out)
	} else {
		out = out[:oldOff]
		newOff = oldOff
	}
	sig := make([]byte, oldSize+4096)
	if _, err := rand.Read(sig); err != nil {
		t.Fatal(err)
	}
	out = append(out, sig...)
	le.PutUint32(out[cmdOff+8:], uint32(newOff))
	le.PutUint32(out[cmdOff+12:], uint32(len(sig)))
	fileSize := uint64(len(out)) - segFileOff
	le.PutUint64(out[segOff+48:], fileSize)
	le.PutUint64(out[segOff+32:], (fileSize+0x3fff)&^0x3fff)
	return out
}

func TestStripResigned(t *testing.T) {
	data := build(t, "arm64")
	want, err := Strip(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		zeroOld bool
	}{{"zeroed in place", true}, {"replaced", false}} {
		t.Run(tc.name, func(t *testing.T) {
			resigned := resign(t, data, tc.zeroOld)
			if _, err := macho.NewFile(bytes.NewReader(resigned)); err != nil {
				t.Fatalf("resigned fixture does not parse: %v", err)
			}
			got, err := Strip(resigned)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("stripped re-signed binary differs from stripped original (%d vs %d bytes)", len(got), len(want))
			}
		})
	}
}

// TestStripCodesign uses Apple's codesign when it is available (macOS):
// re-signing ad hoc with it and stripping must give the same bytes as
// stripping the linker's signature.
func TestStripCodesign(t *testing.T) {
	codesign, err := exec.LookPath("codesign")
	if err != nil {
		t.Skip("codesign is not on PATH")
	}
	data := build(t, "arm64")
	want, err := Strip(data)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "hello")
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatal(err)
	}
	if b, err := exec.Command(codesign, "--force", "--sign", "-", path).CombinedOutput(); err != nil {
		t.Fatalf("codesign: %v\n%s", err, b)
	}
	resigned, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(resigned, data) {
		t.Fatal("codesign left the binary unchanged")
	}
	got, err := Strip(resigned)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("stripped codesign output differs from stripped original (%d vs %d bytes)", len(got), len(want))
	}
}

func TestStripRejects(t *testing.T) {
	if _, err := Strip([]byte("\x7fELF not a mach-o at all, but long enough")); !errors.Is(err, ErrNotMachO) {
		t.Fatalf("ELF: err = %v, want ErrNotMachO", err)
	}
	if _, err := Strip(nil); !errors.Is(err, ErrNotMachO) {
		t.Fatalf("empty: err = %v, want ErrNotMachO", err)
	}
	data := build(t, "arm64")
	if _, err := Strip(data[:len(data)-1]); err == nil {
		t.Fatal("truncated signed binary accepted")
	}
	if _, err := Strip(data[:1000]); err == nil {
		t.Fatal("truncated header accepted")
	}
}
