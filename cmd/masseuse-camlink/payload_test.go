package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FemLed/masseuse-camlink/internal/payload"
	"github.com/FemLed/masseuse-camlink/internal/pesig/petest"
)

// packageExe writes a Windows package to dir: a synthetic connector with
// an ffmpeg and a helper appended, the way packaging/windows/pack does.
func packageExe(t *testing.T, dir string, helper []byte) string {
	t.Helper()
	packed, err := payload.Append(petest.Image([]byte("connector")), "0.13.0", []payload.Entry{
		{Name: "ffmpeg.exe", Data: petest.Image([]byte("ffmpeg"))},
		{Name: "units/camlink-unit-mk312.exe", Data: helper},
		{Name: "README.txt", Data: []byte("readme\r\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "Masseuse.exe")
	if err := os.WriteFile(p, packed, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUnpackPayloadUnpacksOnceAndPrunes(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	exe := packageExe(t, t.TempDir(), petest.Image([]byte("helper")))
	state := t.TempDir()
	dir, err := unpackPayloadFrom(exe, state, log)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(dir) != filepath.Join(state, "bin") {
		t.Fatalf("unpacked to %s", dir)
	}
	for _, name := range []string{"ffmpeg.exe", filepath.Join("units", "camlink-unit-mk312.exe"), "README.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	// A second start finds the unpack whole and leaves it: the files keep
	// their modification times.
	before, _ := os.Stat(filepath.Join(dir, "ffmpeg.exe"))
	again, err := unpackPayloadFrom(exe, state, log)
	if err != nil || again != dir {
		t.Fatalf("second start: %s, %v", again, err)
	}
	after, _ := os.Stat(filepath.Join(dir, "ffmpeg.exe"))
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("a whole unpack was written again")
	}
	// A damaged file is noticed and the payload unpacked afresh.
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg.exe"), []byte("damaged"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := unpackPayloadFrom(exe, state, log); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "ffmpeg.exe"))
	if !bytes.Equal(got, petest.Image([]byte("ffmpeg"))) {
		t.Fatal("the damaged file was not replaced")
	}
	// A new version (another payload) unpacks beside the old, and the old
	// goes, as do leftovers of interrupted unpacks.
	_ = os.MkdirAll(filepath.Join(state, "bin", ".unpack-leftover"), 0o700)
	exe2 := packageExe(t, t.TempDir(), petest.Image([]byte("helper v2")))
	dir2, err := unpackPayloadFrom(exe2, state, log)
	if err != nil {
		t.Fatal(err)
	}
	if dir2 == dir {
		t.Fatal("a different payload unpacked to the same directory")
	}
	entries, _ := os.ReadDir(filepath.Join(state, "bin"))
	if len(entries) != 1 || entries[0].Name() != filepath.Base(dir2) {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("bin holds %v, want only %s", names, filepath.Base(dir2))
	}
}

func TestUnpackPayloadWithoutOne(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	state := t.TempDir()
	bare := filepath.Join(t.TempDir(), "masseuse-camlink.exe")
	_ = os.WriteFile(bare, petest.Image([]byte("connector")), 0o755)
	if dir, err := unpackPayloadFrom(bare, state, log); dir != "" || err != nil {
		t.Fatalf("a bare executable: %q, %v", dir, err)
	}
	notPE := filepath.Join(t.TempDir(), "masseuse-camlink")
	_ = os.WriteFile(notPE, []byte("#!/bin/sh\necho not a PE, an ELF or a Mach-O would be here\n"), 0o755)
	if dir, err := unpackPayloadFrom(notPE, state, log); dir != "" || err != nil {
		t.Fatalf("not a PE: %q, %v", dir, err)
	}
	if dir, err := unpackPayloadFrom(filepath.Join(t.TempDir(), "gone.exe"), state, log); dir != "" || err != nil {
		t.Fatalf("a missing executable: %q, %v", dir, err)
	}
	if _, err := os.Stat(filepath.Join(state, "bin")); err == nil {
		t.Fatal("bin was created for a program without a payload")
	}
	// A damaged payload is an error the caller reports; nothing is unpacked.
	exe := packageExe(t, t.TempDir(), petest.Image([]byte("helper")))
	whole, _ := os.ReadFile(exe)
	in, err := payload.LocateBytes(whole)
	if err != nil {
		t.Fatal(err)
	}
	for name, at := range map[string]int64{"a file": in.Start + 5, "the manifest": in.End - payload.FooterSize - 1, "a hash in the manifest": in.End - payload.FooterSize - 30} {
		data := append([]byte(nil), whole...)
		data[at] ^= 0xff
		_ = os.WriteFile(exe, data, 0o755)
		if dir, err := unpackPayloadFrom(exe, state, log); err == nil {
			t.Fatalf("%s damaged: unpacked to %s", name, dir)
		} else if !strings.Contains(err.Error(), "payload") {
			t.Fatalf("%s damaged: %v", name, err)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(state, "bin")); len(entries) != 0 {
		t.Fatalf("a damaged payload left %d entries under bin", len(entries))
	}
}

func TestHelpersDirPrefersThePayload(t *testing.T) {
	exe := filepath.Join("C:", "Users", "x", "Downloads", "Masseuse.exe")
	unpacked := filepath.Join("C:", "Users", "x", "AppData", "Local", "masseuse-camlink", "bin", "0123456789abcdef")
	if got := helpersDir("", exe, unpacked); got != filepath.Join(unpacked, "units") {
		t.Fatalf("with a payload: %q", got)
	}
	if got := helpersDir("none", exe, unpacked); got != "" {
		t.Fatalf("-estim-helpers none with a payload: %q", got)
	}
	if got := helpersDir(filepath.Join("D:", "units"), exe, unpacked); got != filepath.Join("D:", "units") {
		t.Fatalf("the flag with a payload: %q", got)
	}
	if got := helpersDir("", exe, ""); got != filepath.Join(filepath.Dir(exe), "units") {
		t.Fatalf("without a payload: %q", got)
	}
}
