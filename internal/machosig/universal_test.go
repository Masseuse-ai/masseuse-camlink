package machosig

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// fat assembles a universal binary from thin ones the way lipo does: a
// big-endian header, then each slice at a 16 KiB boundary.
func fat(slices ...[]byte) []byte {
	const align = 14
	be := binary.BigEndian
	header := make([]byte, fatHeaderLen+len(slices)*fatArchLen)
	be.PutUint32(header, fatMagic)
	be.PutUint32(header[4:], uint32(len(slices)))
	out := header
	offsets := make([]int, len(slices))
	for i, s := range slices {
		for len(out)%(1<<align) != 0 {
			out = append(out, 0)
		}
		offsets[i] = len(out)
		out = append(out, s...)
	}
	for i, s := range slices {
		e := out[fatHeaderLen+i*fatArchLen:]
		be.PutUint32(e, binary.LittleEndian.Uint32(s[4:]))     // cputype
		be.PutUint32(e[4:], binary.LittleEndian.Uint32(s[8:])) // cpusubtype
		be.PutUint32(e[8:], uint32(offsets[i]))
		be.PutUint32(e[12:], uint32(len(s)))
		be.PutUint32(e[16:], align)
	}
	return out
}

func TestSlicesThin(t *testing.T) {
	for arch, want := range map[string]string{"arm64": "arm64", "amd64": "amd64"} {
		data := build(t, arch)
		slices, err := Slices(data)
		if err != nil {
			t.Fatal(err)
		}
		if len(slices) != 1 || slices[0].Arch != want || !bytes.Equal(slices[0].Data, data) {
			t.Fatalf("darwin/%s: slices = %d, arch %q", arch, len(slices), slices[0].Arch)
		}
	}
}

func TestStripAllUniversal(t *testing.T) {
	arm, amd := build(t, "arm64"), build(t, "amd64")
	data := fat(arm, amd)
	if !IsUniversal(data) {
		t.Fatal("fixture is not universal")
	}
	if _, err := Strip(data); !errors.Is(err, ErrUniversal) {
		t.Fatalf("Strip(universal): err = %v, want ErrUniversal", err)
	}
	got, err := StripAll(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Arch != "arm64" || got[1].Arch != "amd64" {
		t.Fatalf("slices: %+v", []string{got[0].Arch, got[1].Arch})
	}
	for i, thin := range [][]byte{arm, amd} {
		want, err := Strip(thin)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got[i].Data, want) {
			t.Fatalf("%s: stripped slice differs from the stripped thin binary (%d vs %d bytes)", got[i].Arch, len(got[i].Data), len(want))
		}
	}
	// A slice re-signed inside the universal file strips to the same bytes.
	resigned := fat(resign(t, arm, true), amd)
	again, err := StripAll(resigned)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again[0].Data, got[0].Data) || !bytes.Equal(again[1].Data, got[1].Data) {
		t.Fatal("re-signed universal binary strips differently")
	}
}

// TestUniversalLipo checks the fat reader against lipo's own output where
// lipo exists (macOS).
func TestUniversalLipo(t *testing.T) {
	lipo, err := exec.LookPath("lipo")
	if err != nil {
		t.Skip("lipo is not on PATH")
	}
	dir := t.TempDir()
	arm, amd := build(t, "arm64"), build(t, "amd64")
	for name, data := range map[string][]byte{"arm": arm, "amd": amd} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(dir, "universal")
	if b, err := exec.Command(lipo, "-create", "-output", out, filepath.Join(dir, "arm"), filepath.Join(dir, "amd")).CombinedOutput(); err != nil {
		t.Fatalf("lipo: %v\n%s", err, b)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got, err := StripAll(data)
	if err != nil {
		t.Fatal(err)
	}
	byArch := map[string][]byte{}
	for _, s := range got {
		byArch[s.Arch] = s.Data
	}
	for arch, thin := range map[string][]byte{"arm64": arm, "amd64": amd} {
		want, err := Strip(thin)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(byArch[arch], want) {
			t.Fatalf("%s: lipo slice strips differently from the thin binary", arch)
		}
	}
}

func TestSlicesRejects(t *testing.T) {
	arm := build(t, "arm64")
	data := fat(arm)
	// Slice count beyond the file.
	bad := append([]byte(nil), data...)
	binary.BigEndian.PutUint32(bad[4:], 1000)
	if _, err := Slices(bad); err == nil {
		t.Fatal("absurd slice count accepted")
	}
	// Slice running past the end.
	bad = append([]byte(nil), data...)
	binary.BigEndian.PutUint32(bad[fatHeaderLen+12:], uint32(len(bad)))
	if _, err := Slices(bad); err == nil {
		t.Fatal("slice past the end accepted")
	}
	// Header and slice disagree on the architecture.
	bad = append([]byte(nil), data...)
	binary.BigEndian.PutUint32(bad[fatHeaderLen:], cpuTypeX86_64)
	if _, err := Slices(bad); err == nil {
		t.Fatal("architecture mismatch accepted")
	}
	if _, err := Slices([]byte("\x7fELF not a mach-o at all, but long enough")); !errors.Is(err, ErrNotMachO) {
		t.Fatalf("ELF: err = %v, want ErrNotMachO", err)
	}
}
