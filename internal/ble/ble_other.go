//go:build !darwin && !linux && !windows

package ble

import (
	"context"
	"log/slog"
)

// Open reports ErrUnsupported: this system has no Bluetooth backend (the
// backends are CoreBluetooth on macOS, BlueZ on Linux and the Windows
// Runtime on Windows). The connector still builds and serves its other
// duties.
func Open(ctx context.Context, log *slog.Logger) (Central, error) {
	return nil, ErrUnsupported
}
