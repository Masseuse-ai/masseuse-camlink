//go:build windows

// Command verinfo prints the strings a Windows program's version resource
// carries, the ones Explorer's Details tab shows: ProductName, CompanyName,
// FileDescription, LegalCopyright, Comments. The workflows run it on the
// built connector to check that the committed resource objects
// (packaging/windows/make-syso.sh) were linked in and say what winres.json
// says.
//
// It is its own reader, and not PowerShell's (Get-Item x.exe).VersionInfo,
// on purpose: .NET's FileVersionInfo treats a string table without a
// FileVersion string as not found, falls back through code pages that are
// not there and reports every string empty, while the connector's resource
// carries no version numbers by design (the version is what the binary's
// build information says, VERIFY.md), so the .syso stays the same bytes from
// release to release and the reproduce job keeps matching. The Win32 calls
// below are what Explorer itself uses: the Translation table names the
// language and code page, and each string is read from that table.
//
// usage: go run ./packaging/windows/verinfo PROGRAM.exe
// Prints one "Name: value" line per string, an absent string as an empty
// value; exits 1 when the file has no version resource at all.
package main

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

var names = []string{"ProductName", "CompanyName", "FileDescription", "LegalCopyright", "Comments"}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: verinfo PROGRAM.exe")
		os.Exit(2)
	}
	path := os.Args[1]
	size, err := windows.GetFileVersionInfoSize(path, nil)
	if err != nil || size == 0 {
		fmt.Fprintf(os.Stderr, "%s: no version resource (%v)\n", path, err)
		os.Exit(1)
	}
	block := make([]byte, size)
	if err := windows.GetFileVersionInfo(path, 0, size, unsafe.Pointer(&block[0])); err != nil {
		fmt.Fprintf(os.Stderr, "%s: GetFileVersionInfo: %v\n", path, err)
		os.Exit(1)
	}
	var p unsafe.Pointer
	var n uint32
	if err := windows.VerQueryValue(unsafe.Pointer(&block[0]), `\VarFileInfo\Translation`, unsafe.Pointer(&p), &n); err != nil || n < 4 {
		fmt.Fprintf(os.Stderr, "%s: no Translation table (%v)\n", path, err)
		os.Exit(1)
	}
	// Language and code page, each a 16-bit word; the string table is keyed
	// by both as hexadecimal, 040904B0 for US English in Unicode.
	translation := unsafe.Slice((*uint16)(p), 2)
	table := fmt.Sprintf(`\StringFileInfo\%04X%04X\`, translation[0], translation[1])
	for _, name := range names {
		value := ""
		if err := windows.VerQueryValue(unsafe.Pointer(&block[0]), table+name, unsafe.Pointer(&p), &n); err == nil && n > 0 {
			value = windows.UTF16PtrToString((*uint16)(p))
		}
		fmt.Printf("%s: %s\n", name, value)
	}
}
