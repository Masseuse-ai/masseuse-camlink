package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FemLed/masseuse-camlink/internal/payload"
	"github.com/FemLed/masseuse-camlink/internal/pesig"
	"github.com/FemLed/masseuse-camlink/internal/pesig/petest"
)

// A stand-in for everything the release hands the packer: a connector, an
// ffmpeg with its licence texts, one helper, and the repository's notices.
func fixture(t *testing.T) (options, []byte) {
	t.Helper()
	root := t.TempDir()
	write := func(rel string, data []byte) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	connector := petest.Image(bytes.Repeat([]byte("C"), 1234))
	write("dist/masseuse-camlink.exe", connector)
	write("dist/ffmpeg-windows/ffmpeg.exe", petest.Image(bytes.Repeat([]byte("F"), 777)))
	write("dist/ffmpeg-windows/licenses/ffmpeg-LICENSE.md", []byte("ffmpeg licence\n"))
	write("dist/ffmpeg-windows/licenses/opus-COPYING", []byte("opus\nlicence\n"))
	write("dist/units/camlink-unit-mk312.exe", petest.Image(bytes.Repeat([]byte("H"), 99)))
	write("repo/packaging/windows/README.txt", []byte("Masseuse.ai for your computer (Windows)\nline two\n"))
	write("repo/LICENSE", []byte("Apache\n"))
	write("repo/NOTICE", []byte("notice\r\n"))
	write("repo/packaging/ffmpeg/THIRD_PARTY.md", []byte("# ffmpeg\n"))
	return options{
		version:   "0.13.0",
		connector: filepath.Join(root, "dist", "masseuse-camlink.exe"),
		ffmpegDir: filepath.Join(root, "dist", "ffmpeg-windows"),
		unitsDir:  filepath.Join(root, "dist", "units"),
		out:       filepath.Join(root, "dist", "Masseuse.exe"),
		repo:      filepath.Join(root, "repo"),
	}, connector
}

func TestBuildPacksEverythingAndProvesTheConnector(t *testing.T) {
	o, connector := fixture(t)
	rep, err := build(o)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range rep.files {
		names = append(names, f.Name)
	}
	want := "ffmpeg.exe units/camlink-unit-mk312.exe README.txt LICENSE NOTICE THIRD_PARTY.md licenses/ffmpeg-LICENSE.md licenses/opus-COPYING"
	if got := strings.Join(names, " "); got != want {
		t.Fatalf("files:\n got %s\nwant %s", got, want)
	}
	packed, err := os.ReadFile(o.out)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(packed)) != rep.size || len(packed)%pesig.FooterAlign != 0 {
		t.Fatalf("size %d (report %d)", len(packed), rep.size)
	}
	in, err := payload.LocateBytes(packed)
	if err != nil {
		t.Fatal(err)
	}
	if in.Manifest.Version != "0.13.0" || in.ID() != rep.id {
		t.Fatalf("manifest %+v, id %s", in.Manifest, rep.id)
	}
	// The connector is inside byte for byte, and cmd/pestrip's way back
	// (signature, then payload) gives it, signed or not.
	if !bytes.HasPrefix(packed, connector) {
		t.Fatal("the package does not start with the connector")
	}
	signed := petest.Sign(packed, 300, 0x1111)
	unsigned, err := pesig.Strip(signed)
	if err != nil {
		t.Fatal(err)
	}
	bare, err := payload.Strip(unsigned)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bare, connector) {
		t.Fatal("stripping the signed package does not give the connector back")
	}
	// Text files carry Windows line endings, once; binaries are untouched.
	readme, _ := in.Open(bytes.NewReader(packed), "README.txt")
	var b bytes.Buffer
	_, _ = b.ReadFrom(readme)
	if b.String() != "Masseuse.ai for your computer (Windows)\r\nline two\r\n" {
		t.Fatalf("README.txt: %q", b.String())
	}
	notice, _ := in.Open(bytes.NewReader(packed), "NOTICE")
	b.Reset()
	_, _ = b.ReadFrom(notice)
	if b.String() != "notice\r\n" {
		t.Fatalf("NOTICE: %q", b.String())
	}
	ff, _ := in.Open(bytes.NewReader(packed), "ffmpeg.exe")
	b.Reset()
	_, _ = b.ReadFrom(ff)
	if !bytes.Equal(b.Bytes(), petest.Image(bytes.Repeat([]byte("F"), 777))) {
		t.Fatal("ffmpeg.exe was changed")
	}
}

func TestBuildRefusals(t *testing.T) {
	o, _ := fixture(t)
	bad := o
	bad.version = "v0.13.0"
	if _, err := build(bad); err == nil {
		t.Fatal("a version with the v was accepted")
	}
	bad = o
	bad.unitsDir = t.TempDir()
	if _, err := build(bad); err == nil || !strings.Contains(err.Error(), "camlink-unit-") {
		t.Fatalf("an empty units directory: %v", err)
	}
	bad = o
	bad.ffmpegDir = t.TempDir()
	if _, err := build(bad); err == nil || !strings.Contains(err.Error(), "ffmpeg.exe") {
		t.Fatalf("no ffmpeg: %v", err)
	}
	// A connector already signed is refused: the signature would end up
	// inside the payload's idea of the image.
	signedConnector := filepath.Join(t.TempDir(), "signed.exe")
	_ = os.WriteFile(signedConnector, petest.Sign(petest.Image([]byte("x")), 64, 1), 0o644)
	bad = o
	bad.connector = signedConnector
	if _, err := build(bad); err == nil || !strings.Contains(err.Error(), "signed") {
		t.Fatalf("a signed connector: %v", err)
	}
	// Without -u the package has no helpers and says nothing about it.
	noUnits := o
	noUnits.unitsDir = ""
	noUnits.out = filepath.Join(t.TempDir(), "Masseuse.exe")
	rep, err := build(noUnits)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range rep.files {
		if strings.HasPrefix(f.Name, "units/") {
			t.Fatal("a helper packed without -u")
		}
	}
}
