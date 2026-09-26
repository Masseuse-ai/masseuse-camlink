// machostrip removes the code signature from a Mach-O executable, so that a
// signed macOS release binary can be compared with an unsigned rebuild
// (VERIFY.md, "The macOS binaries"). It runs on any operating system:
//
//	go run github.com/FemLed/masseuse-camlink/cmd/machostrip@vX.Y.Z -sha256 masseuse-camlink
//
// prints the SHA-256 of the binary without its signature; an unsigned binary
// passes through unchanged, so the same command hashes a rebuild. A
// universal binary (the application bundle's, both architectures in one
// file) is hashed one architecture at a time, each line naming its
// architecture with Go's name (arm64, amd64) as the release archives do;
// with -o, each architecture is written to out.<arch>.
package main

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"os"

	"github.com/FemLed/masseuse-camlink/internal/machosig"
)

func main() {
	out := flag.String("o", "", "write the stripped binary here (default: standard output); a universal binary is written to out.<arch>, one file per architecture")
	hash := flag.Bool("sha256", false, "print the SHA-256 of the stripped binary instead of the binary")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: machostrip [-o out] [-sha256] file...")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() == 0 || (*out != "" && flag.NArg() != 1) || (*out != "" && *hash) {
		flag.Usage()
		os.Exit(2)
	}
	for _, name := range flag.Args() {
		data, err := os.ReadFile(name)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		universal := machosig.IsUniversal(data)
		slices, err := machosig.StripAll(data)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
			os.Exit(1)
		}
		for _, s := range slices {
			switch {
			case *hash && universal:
				fmt.Printf("%x  %s (%s)\n", sha256.Sum256(s.Data), name, s.Arch)
			case *hash:
				fmt.Printf("%x  %s\n", sha256.Sum256(s.Data), name)
			case *out != "" && universal:
				if err := os.WriteFile(*out+"."+s.Arch, s.Data, 0o755); err != nil {
					fmt.Fprintln(os.Stderr, err)
					os.Exit(1)
				}
			case *out != "":
				if err := os.WriteFile(*out, s.Data, 0o755); err != nil {
					fmt.Fprintln(os.Stderr, err)
					os.Exit(1)
				}
			case universal:
				fmt.Fprintf(os.Stderr, "%s: a universal binary has one stripped file per architecture; use -o or -sha256\n", name)
				os.Exit(2)
			default:
				if _, err := os.Stdout.Write(s.Data); err != nil {
					fmt.Fprintln(os.Stderr, err)
					os.Exit(1)
				}
			}
		}
	}
}
