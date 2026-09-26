// pestrip removes the Authenticode signature and the appended payload from
// a Windows executable, so that a signed Windows release file can be
// compared with an unsigned rebuild (VERIFY.md, "The Windows package"). It
// is the Windows twin of cmd/machostrip and runs on any operating system:
//
//	go run github.com/FemLed/masseuse-camlink/cmd/pestrip@vX.Y.Z -sha256 Masseuse.exe
//
// prints the SHA-256 of the executable without its signature (internal/pesig)
// and without the payload the Windows package carries (internal/payload),
// which for Masseuse.exe is the published windows_amd64 connector's
// hash. An unsigned file without a payload passes through unchanged, so
// the same command hashes a rebuild; a file that has a checksum written by
// its linker (ffmpeg.exe) has it zeroed on both sides, so compare
// `pestrip -sha256` with `pestrip -sha256`, not with a plain hash.
//
//	go run github.com/FemLed/masseuse-camlink/cmd/pestrip@vX.Y.Z -payload DIR Masseuse.exe
//
// writes the payload's files into DIR, each checked against the payload's
// manifest, and lists them with their hashes (the ffmpeg.exe and the
// helpers inside, signed as they are).
package main

import (
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/FemLed/masseuse-camlink/internal/payload"
	"github.com/FemLed/masseuse-camlink/internal/pesig"
)

func main() {
	out := flag.String("o", "", "write the stripped executable here (default: standard output)")
	hash := flag.Bool("sha256", false, "print the SHA-256 of the stripped executable instead of the executable")
	payloadDir := flag.String("payload", "", "write the payload's files into this directory and list them with their hashes")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: pestrip [-o out] [-sha256] [-payload dir] file...")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() == 0 || (*out != "" && flag.NArg() != 1) || (*out != "" && *hash) || (*payloadDir != "" && flag.NArg() != 1) {
		flag.Usage()
		os.Exit(2)
	}
	for _, name := range flag.Args() {
		if err := run(name, *out, *hash, *payloadDir, flag.NArg() > 1); err != nil {
			fmt.Fprintln(os.Stderr, name+":", err)
			os.Exit(1)
		}
	}
}

func run(name, out string, hash bool, payloadDir string, several bool) error {
	data, err := os.ReadFile(name)
	if err != nil {
		return err
	}
	unsigned, err := pesig.Strip(data)
	if err != nil {
		return err
	}
	if payloadDir != "" {
		in, err := payload.LocateBytes(unsigned)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(payloadDir, 0o755); err != nil {
			return err
		}
		if err := in.Extract(bytesAt(unsigned), payloadDir); err != nil {
			return err
		}
		for _, f := range in.Manifest.Files {
			fmt.Printf("%s  %s\n", f.SHA256, filepath.ToSlash(f.Name))
		}
		fmt.Fprintf(os.Stderr, "payload %s (version %s, %d files) written to %s\n", in.ID(), in.Manifest.Version, len(in.Manifest.Files), payloadDir)
	}
	bare, err := payload.Strip(unsigned)
	if err != nil && !errors.Is(err, payload.ErrNone) {
		return err
	}
	switch {
	case hash:
		sum := sha256.Sum256(bare)
		if several {
			fmt.Printf("%x  %s\n", sum, name)
		} else {
			fmt.Printf("%x\n", sum)
		}
	case out != "":
		return os.WriteFile(out, bare, 0o755)
	case payloadDir == "":
		_, err = os.Stdout.Write(bare)
		return err
	}
	return nil
}

type bytesAt []byte

func (b bytesAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(b)) {
		return 0, errors.New("read past the end")
	}
	n := copy(p, b[off:])
	if n < len(p) {
		return n, errors.New("short read")
	}
	return n, nil
}
