// Package buildinfo reports the version of this module and the Go toolchain
// from the build information Go embeds, so that there is nothing to pass
// with -ldflags and a release is byte-for-byte what `go install ...@vX.Y.Z`
// produces (VERIFY.md).
package buildinfo

import "runtime/debug"

// ModulePath is this module's path.
const ModulePath = "github.com/FemLed/masseuse-camlink"

// Version is the version of this module in the running binary: the main
// module's version for `go install ...@vX.Y.Z`; this module's version as a
// dependency when the binary was built the way goreleaser's proxy mode does
// (a scratch module requiring this one at the tag); "(devel)" for a build
// from a working tree.
func Version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "(devel)"
	}
	if info.Main.Path == ModulePath && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	for _, dep := range info.Deps {
		if dep.Path == ModulePath {
			if dep.Replace != nil {
				return "(devel)"
			}
			return dep.Version
		}
	}
	return "(devel)"
}

// GoVersion is the toolchain that built the binary.
func GoVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	return info.GoVersion
}
