//go:build !darwin && !windows && !linux

package awake

// Nothing holds the computer awake on this system yet; Take says so and
// the connector runs on.
func take(string, string) (func() error, error) { return nil, ErrUnsupported }
