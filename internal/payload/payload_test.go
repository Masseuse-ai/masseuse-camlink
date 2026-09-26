package payload

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FemLed/masseuse-camlink/internal/pesig"
	"github.com/FemLed/masseuse-camlink/internal/pesig/petest"
)

var entries = []Entry{
	{Name: "ffmpeg.exe", Data: bytes.Repeat([]byte("F"), 1001)},
	{Name: "units/camlink-unit-mk312.exe", Data: bytes.Repeat([]byte("H"), 333)},
	{Name: "licenses/opus-COPYING", Data: []byte("license text\r\n")},
	{Name: "README.txt", Data: []byte{}}, // an empty file is a file
}

func pack(t *testing.T, exe []byte) []byte {
	t.Helper()
	out, err := Append(exe, "0.13.0", entries)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAppendAndLocateRoundTrip(t *testing.T) {
	for _, n := range []int{1, 8, 17} { // image ends unaligned and aligned
		bare := petest.Image(bytes.Repeat([]byte{9}, n))
		packed := pack(t, bare)
		if len(packed)%pesig.FooterAlign != 0 {
			t.Fatalf("n=%d: the package is %d bytes, not aligned", n, len(packed))
		}
		in, err := LocateBytes(packed)
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if in.Start != int64(len(bare)) || in.End != int64(len(packed)) {
			t.Fatalf("n=%d: payload %d..%d, bare %d, package %d", n, in.Start, in.End, len(bare), len(packed))
		}
		if in.Manifest.Version != "0.13.0" || in.Manifest.Format != Format || len(in.Manifest.Files) != len(entries) {
			t.Fatalf("manifest %+v", in.Manifest)
		}
		if err := in.Verify(bytes.NewReader(packed)); err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			r, ok := in.Open(bytes.NewReader(packed), e.Name)
			if !ok {
				t.Fatalf("no %s", e.Name)
			}
			var got bytes.Buffer
			if _, err := got.ReadFrom(r); err != nil || !bytes.Equal(got.Bytes(), e.Data) {
				t.Fatalf("%s: read back %d bytes, err %v", e.Name, got.Len(), err)
			}
		}
		stripped, err := Strip(packed)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(stripped, bare) {
			t.Fatal("Strip does not give the bare executable back")
		}
	}
}

func TestLocateAfterSigning(t *testing.T) {
	bare := petest.Image([]byte("connector"))
	packed := pack(t, bare)
	want, err := LocateBytes(packed)
	if err != nil {
		t.Fatal(err)
	}
	signed := petest.Sign(packed, 200, 0xfeed)
	if len(signed) != len(packed)+200 {
		t.Fatalf("signing padded an aligned package: %d -> %d", len(packed), len(signed))
	}
	in, err := LocateBytes(signed)
	if err != nil {
		t.Fatal(err)
	}
	if in.Start != want.Start || in.End != want.End || in.ID() != want.ID() {
		t.Fatalf("signed: %d..%d %s; unsigned: %d..%d %s", in.Start, in.End, in.ID(), want.Start, want.End, want.ID())
	}
	if err := in.Verify(bytes.NewReader(signed)); err != nil {
		t.Fatal(err)
	}
	// The whole way back: signature off, payload off, the bare executable.
	unsigned, err := pesig.Strip(signed)
	if err != nil {
		t.Fatal(err)
	}
	stripped, err := Strip(unsigned)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stripped, bare) {
		t.Fatal("pesig.Strip then Strip does not give the bare executable back")
	}
	// A tool that pads before its table even so is tolerated: the footer
	// is looked for up to seven zero bytes back. Built by hand, since
	// petest.Sign pads like signtool, only to the boundary.
	between := func(fill []byte) []byte {
		out := append(append(append([]byte(nil), packed...), fill...), signed[len(packed):]...)
		h, err := pesig.ParseBytes(signed)
		if err != nil {
			t.Fatal(err)
		}
		// out has packed's (unsigned) header: point its security entry at
		// the table, moved by len(fill).
		entry := out[h.SecurityEntryOffset:]
		binary.LittleEndian.PutUint32(entry, h.SecurityOffset+uint32(len(fill)))
		binary.LittleEndian.PutUint32(entry[4:], h.SecuritySize)
		return out
	}
	padded := between([]byte{0, 0, 0})
	if h, err := pesig.ParseBytes(padded); err != nil || !h.Signed() || int(h.SecurityOffset) != len(packed)+3 {
		t.Fatalf("hand-built padding: header %+v, err %v (package %d bytes)", h, err, len(packed))
	}
	if in, err := LocateBytes(padded); err != nil || in.Start != want.Start {
		t.Fatalf("with padding before the table: %v", err)
	}
	// Non-zero bytes there are not padding.
	if _, err := LocateBytes(between([]byte{'x', 0, 0})); err == nil || errors.Is(err, ErrNone) {
		t.Fatalf("junk between the footer and the signature: %v", err)
	}
}

func TestNoPayload(t *testing.T) {
	bare := petest.Image([]byte("connector"))
	if _, err := LocateBytes(bare); !errors.Is(err, ErrNone) {
		t.Fatalf("bare: %v", err)
	}
	if _, err := LocateBytes(petest.Sign(bare, 64, 1)); !errors.Is(err, ErrNone) {
		t.Fatalf("signed bare: %v", err)
	}
	if out, err := Strip(bare); err != nil || !bytes.Equal(out, bare) {
		t.Fatalf("Strip of a bare executable: %v", err)
	}
	if _, err := LocateBytes([]byte("not a PE at all, not even close to one")); !errors.Is(err, pesig.ErrNotPE) {
		t.Fatalf("not a PE: %v", err)
	}
	p := filepath.Join(t.TempDir(), "bare.exe")
	_ = os.WriteFile(p, bare, 0o644)
	if Carries(p) {
		t.Fatal("Carries says a bare executable carries a payload")
	}
	if Carries(filepath.Join(t.TempDir(), "missing.exe")) {
		t.Fatal("Carries says a missing file carries a payload")
	}
}

func TestAppendRefusals(t *testing.T) {
	bare := petest.Image([]byte("connector"))
	packed := pack(t, bare)
	if _, err := Append(packed, "1", entries); err == nil || !strings.Contains(err.Error(), "already carries") {
		t.Fatalf("packing a package: %v", err)
	}
	if _, err := Append(petest.Sign(bare, 64, 1), "1", entries); err == nil || !strings.Contains(err.Error(), "signed") {
		t.Fatalf("packing a signed executable: %v", err)
	}
	if _, err := Append(bare, "1", nil); err == nil {
		t.Fatal("packing nothing")
	}
	for _, name := range []string{"", "/abs", "a/../b", "./a", "a//b", `units\x.exe`, "c:x", "a\x01b", "dir/", strings.Repeat("n", MaxName+1)} {
		if _, err := Append(bare, "1", []Entry{{Name: name, Data: []byte("x")}}); err == nil {
			t.Fatalf("name %q accepted", name)
		}
	}
	if _, err := Append(bare, "1", []Entry{{Name: "a", Data: []byte("x")}, {Name: "a", Data: []byte("y")}}); err == nil {
		t.Fatal("a duplicate name accepted")
	}
	if _, err := Append([]byte("not a PE, not at all, nothing like one"), "1", entries); !errors.Is(err, pesig.ErrNotPE) {
		t.Fatalf("not a PE: %v", err)
	}
}

func TestDamageIsCaught(t *testing.T) {
	bare := petest.Image([]byte("connector"))
	packed := pack(t, bare)
	in, err := LocateBytes(packed)
	if err != nil {
		t.Fatal(err)
	}
	// A byte of a file flipped: located still (the manifest is whole), but
	// Verify and Extract refuse.
	damaged := append([]byte(nil), packed...)
	damaged[in.Start+10] ^= 0xff
	if err := in.Verify(bytes.NewReader(damaged)); err == nil || !strings.Contains(err.Error(), "ffmpeg.exe") {
		t.Fatalf("Verify on damaged data: %v", err)
	}
	dir := t.TempDir()
	if err := in.Extract(bytes.NewReader(damaged), dir); err == nil {
		t.Fatal("Extract wrote a damaged file without complaint")
	}
	// A byte of the manifest flipped: not a payload at all.
	badManifest := append([]byte(nil), packed...)
	badManifest[in.End-FooterSize-5] ^= 0xff
	if _, err := LocateBytes(badManifest); err == nil || errors.Is(err, ErrNone) {
		t.Fatalf("a damaged manifest located: %v", err)
	}
	// A footer with impossible lengths.
	badFooter := append([]byte(nil), packed...)
	badFooter[in.End-FooterSize+8] = 0xff
	badFooter[in.End-FooterSize+15] = 0x7f
	if _, err := LocateBytes(badFooter); err == nil || errors.Is(err, ErrNone) {
		t.Fatalf("a footer with an impossible length located: %v", err)
	}
	// A truncated package: no footer where one is looked for.
	if _, err := LocateBytes(packed[:len(packed)-1]); !errors.Is(err, ErrNone) {
		t.Fatalf("truncated: %v", err)
	}
}

func TestExtractAndCheck(t *testing.T) {
	bare := petest.Image([]byte("connector"))
	packed := pack(t, bare)
	p := filepath.Join(t.TempDir(), "Masseuse.exe")
	if err := os.WriteFile(p, packed, 0o644); err != nil {
		t.Fatal(err)
	}
	if !Carries(p) {
		t.Fatal("Carries says no")
	}
	r, err := OpenFile(p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Verify(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "bin", r.Info.ID())
	if err := r.Extract(dir); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(e.Name)))
		if err != nil || !bytes.Equal(got, e.Data) {
			t.Fatalf("%s: %v", e.Name, err)
		}
	}
	if err := r.Info.Check(dir); err != nil {
		t.Fatal(err)
	}
	// A file changed on disk, or missing: Check says so.
	_ = os.WriteFile(filepath.Join(dir, "ffmpeg.exe"), []byte("replaced"), 0o644)
	if err := r.Info.Check(dir); err == nil || !strings.Contains(err.Error(), "ffmpeg.exe") {
		t.Fatalf("Check with a changed file: %v", err)
	}
	_ = os.Remove(filepath.Join(dir, "ffmpeg.exe"))
	if err := r.Info.Check(dir); err == nil {
		t.Fatal("Check with a missing file")
	}
	if len(r.Info.ID()) != 16 {
		t.Fatalf("ID %q", r.Info.ID())
	}
	// The same files packed again have the same ID; a different file, another.
	again, _ := LocateBytes(pack(t, bare))
	if again.ID() != r.Info.ID() {
		t.Fatal("the same payload has two IDs")
	}
	other, _ := Append(bare, "0.13.0", []Entry{{Name: "ffmpeg.exe", Data: []byte("other")}})
	oin, _ := LocateBytes(other)
	if oin.ID() == r.Info.ID() {
		t.Fatal("different payloads share an ID")
	}
}
