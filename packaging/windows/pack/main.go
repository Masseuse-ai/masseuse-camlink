// Command pack assembles the Windows download, Masseuse.exe: the
// published windows_amd64 connector with ffmpeg.exe, the unit driver
// helpers, the notices and a README appended as a payload
// (internal/payload), one file that is the whole program. The connector's
// bytes are left as they are, so the package minus its payload (and minus
// the Authenticode signature the release adds afterwards) is the published
// binary byte for byte, which cmd/pestrip proves; the release workflow
// signs ffmpeg.exe and the helpers before handing them here, and signs the
// package after. Plain Go, so it runs on the Windows release runner and on
// the Linux CI runner alike; nothing is signed here.
//
// usage: go run ./packaging/windows/pack -v VERSION -b CONNECTOR -f FFMPEG_DIR [-u UNITS_DIR] -o OUT.exe [-repo DIR]
//
//	VERSION     the release version without the v (0.0.0 for a local build)
//	CONNECTOR   the masseuse-camlink.exe to pack (goreleaser's windows_amd64
//	            binary, unsigned)
//	FFMPEG_DIR  packaging/ffmpeg/build.sh -t windows's output directory:
//	            ffmpeg.exe and licenses/ (their absence is an error: the
//	            package promises a camera without an install step)
//	UNITS_DIR   packaging/units/fetch.sh's output for windows/amd64: the unit
//	            driver helpers (camlink-unit-*.exe); without -u the package
//	            has none and serves the Mastago alone
//	OUT.exe     the package, (re)created
//	DIR         the repository root, for README.txt, LICENSE, NOTICE and
//	            THIRD_PARTY.md (default: found from this source file)
package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/FemLed/masseuse-camlink/internal/payload"
	"github.com/FemLed/masseuse-camlink/internal/pesig"
)

func main() {
	var o options
	flag.StringVar(&o.version, "v", "", "the release version without the v")
	flag.StringVar(&o.connector, "b", "", "the masseuse-camlink.exe to pack")
	flag.StringVar(&o.ffmpegDir, "f", "", "the directory with ffmpeg.exe and licenses/")
	flag.StringVar(&o.unitsDir, "u", "", "the directory with the unit driver helpers (camlink-unit-*.exe)")
	flag.StringVar(&o.out, "o", "", "the package to write")
	flag.StringVar(&o.repo, "repo", "", "the repository root (default: found from this source file)")
	flag.Parse()
	if o.version == "" || o.connector == "" || o.ffmpegDir == "" || o.out == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: pack -v VERSION -b CONNECTOR -f FFMPEG_DIR [-u UNITS_DIR] -o OUT.exe [-repo DIR]")
		os.Exit(2)
	}
	if o.repo == "" {
		_, file, _, ok := runtime.Caller(0)
		if !ok {
			fmt.Fprintln(os.Stderr, "pack: cannot find the repository; pass -repo")
			os.Exit(2)
		}
		o.repo = filepath.Join(filepath.Dir(file), "..", "..", "..")
	}
	rep, err := build(o)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pack:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d bytes); payload %s; the connector inside is sha256 %s\n", o.out, rep.size, rep.id, rep.connectorSHA256)
	for _, f := range rep.files {
		fmt.Printf("  %s  %10d  %s\n", f.SHA256, f.Size, f.Name)
	}
}

type options struct {
	version, connector, ffmpegDir, unitsDir, out, repo string
}

type report struct {
	size            int64
	id              string
	connectorSHA256 string
	files           []payload.File
}

// versionOK is X.Y.Z, digits only, as the release tags without the v.
func versionOK(v string) bool {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" || strings.Trim(p, "0123456789") != "" {
			return false
		}
	}
	return true
}

// build assembles the package and checks it: the payload located and
// verified in the file written, and the package stripped of it the
// connector again, byte for byte.
func build(o options) (*report, error) {
	if !versionOK(o.version) {
		return nil, fmt.Errorf("version %s is not X.Y.Z", o.version)
	}
	connector, err := os.ReadFile(o.connector)
	if err != nil {
		return nil, err
	}
	if h, err := pesig.ParseBytes(connector); err != nil {
		return nil, fmt.Errorf("%s: %w", o.connector, err)
	} else if h.Signed() {
		return nil, fmt.Errorf("%s is signed; the package is signed after packing, the connector inside never", o.connector)
	}
	var entries []payload.Entry
	add := func(name, path string, text bool) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if text {
			data = crlf(data)
		}
		entries = append(entries, payload.Entry{Name: name, Data: data})
		return nil
	}
	if err := add("ffmpeg.exe", filepath.Join(o.ffmpegDir, "ffmpeg.exe"), false); err != nil {
		return nil, fmt.Errorf("no ffmpeg.exe in %s (packaging/ffmpeg/build.sh -t windows): %w", o.ffmpegDir, err)
	}
	if o.unitsDir != "" {
		helpers, _ := filepath.Glob(filepath.Join(o.unitsDir, "camlink-unit-*.exe"))
		if len(helpers) == 0 {
			return nil, fmt.Errorf("no camlink-unit-*.exe in %s (packaging/units/fetch.sh)", o.unitsDir)
		}
		sort.Strings(helpers)
		for _, h := range helpers {
			if err := add("units/"+filepath.Base(h), h, false); err != nil {
				return nil, err
			}
		}
	}
	// Text files with Windows line endings, so Notepad shows them as lines.
	for _, f := range []struct{ name, path string }{
		{"README.txt", filepath.Join(o.repo, "packaging", "windows", "README.txt")},
		{"LICENSE", filepath.Join(o.repo, "LICENSE")},
		{"NOTICE", filepath.Join(o.repo, "NOTICE")},
		{"THIRD_PARTY.md", filepath.Join(o.repo, "packaging", "ffmpeg", "THIRD_PARTY.md")},
	} {
		if err := add(f.name, f.path, true); err != nil {
			return nil, err
		}
	}
	licenses, err := os.ReadDir(filepath.Join(o.ffmpegDir, "licenses"))
	if err != nil {
		return nil, fmt.Errorf("no %s (packaging/ffmpeg/build.sh): %w", filepath.Join(o.ffmpegDir, "licenses"), err)
	}
	for _, e := range licenses {
		if e.IsDir() {
			continue
		}
		if err := add("licenses/"+e.Name(), filepath.Join(o.ffmpegDir, "licenses", e.Name()), true); err != nil {
			return nil, err
		}
	}
	packed, err := payload.Append(connector, o.version, entries)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(o.out), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(o.out, packed, 0o755); err != nil {
		return nil, err
	}
	// The gates: what was written carries the payload whole, and is the
	// connector once the payload is taken off.
	r, err := payload.OpenFile(o.out)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", o.out, err)
	}
	defer r.Close()
	if err := r.Verify(); err != nil {
		return nil, fmt.Errorf("%s: %w", o.out, err)
	}
	written, err := os.ReadFile(o.out)
	if err != nil {
		return nil, err
	}
	bare, err := payload.Strip(written)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(bare, connector) {
		return nil, errors.New("the package minus its payload is not the connector")
	}
	sum := sha256.Sum256(connector)
	return &report{size: int64(len(written)), id: r.Info.ID(), connectorSHA256: fmt.Sprintf("%x", sum), files: r.Info.Manifest.Files}, nil
}

// crlf gives text Windows line endings, once.
func crlf(b []byte) []byte {
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	return bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n"))
}
