//go:build !darwin && !linux

package ble

import (
	"context"
	"log/slog"
)

// Open reports ErrUnsupported: this system has no Bluetooth backend yet
// (a WinRT backend for Windows is planned). The connector still builds
// and serves its other duties.
func Open(ctx context.Context, log *slog.Logger) (Central, error) {
	return nil, ErrUnsupported
}
