//go:build linux

package awake

import (
	"errors"
	"fmt"
	"os"

	"github.com/godbus/dbus/v5"
)

// Linux: a logind inhibitor (org.freedesktop.login1.Manager.Inhibit) for
// "idle" and "sleep" in block mode, over the system bus (internal/ble
// reaches BlueZ the same way). logind hands back a pipe; the inhibitor
// lasts as long as this end of it is open, so the process ending releases
// it. `systemd-inhibit --list` shows it with its reason. Desktops that
// idle-suspend through logind honour it; one without logind (no system
// bus, another init) gets an error and the connector runs on without.

const (
	login1        = "org.freedesktop.login1"
	login1Path    = "/org/freedesktop/login1"
	login1Inhibit = "org.freedesktop.login1.Manager.Inhibit"
	inhibitWhat   = "idle:sleep"
	inhibitMode   = "block"
)

func take(name, reason string) (func() error, error) {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, fmt.Errorf("awake: system bus: %w", err)
	}
	var fd dbus.UnixFD
	if err := conn.Object(login1, login1Path).Call(login1Inhibit, 0, inhibitWhat, name, reason, inhibitMode).Store(&fd); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("awake: logind Inhibit: %w", err)
	}
	pipe := os.NewFile(uintptr(fd), "logind-inhibit")
	if pipe == nil {
		_ = conn.Close()
		return nil, errors.New("awake: logind returned no inhibitor descriptor")
	}
	return func() error {
		perr := pipe.Close()
		cerr := conn.Close()
		if perr != nil {
			return fmt.Errorf("awake: %w", perr)
		}
		if cerr != nil {
			return fmt.Errorf("awake: %w", cerr)
		}
		return nil
	}, nil
}
