//go:build !windows

package serialport

// windowsCandidates reads the Windows registry; on any other system there
// is nothing to read.
func windowsCandidates() ([]Candidate, error) { return nil, nil }
