package mk312

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/FemLed/masseuse-camlink/internal/estim"
)

// Label names the device for people.
const Label = "ErosTek MK-312BT"

// A KeyStore keeps the session key across connector restarts, so a
// connector that restarted mid-session can resume the device instead of
// needing it power-cycled.
type KeyStore interface {
	Load() (key int, ok bool, err error)
	Save(key int) error
	Clear() error
}

// FileKeyStore keeps the key in one small file under the state directory.
type FileKeyStore struct{ Path string }

const keyPrefix = "MK312KEY:"

// Load reads the stored key; ok is false when there is none.
func (s FileKeyStore) Load() (int, bool, error) {
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	text := strings.TrimSpace(string(b))
	if !strings.HasPrefix(text, keyPrefix) {
		return 0, false, nil
	}
	k, err := strconv.Atoi(strings.TrimPrefix(text, keyPrefix))
	if err != nil || k < 0 || k > 0xFF {
		return 0, false, nil
	}
	return k, true, nil
}

// Save writes the key, creating the state directory if this is the first
// thing written there (a probe on a fresh computer).
func (s FileKeyStore) Save(key int) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(s.Path, []byte(keyPrefix+strconv.Itoa(key)+"\n"), 0o600)
}

// Clear forgets the key.
func (s FileKeyStore) Clear() error {
	err := os.Remove(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Driver is the estim.Driver for one connected MK-312BT.
type Driver struct {
	*Device
	store KeyStore
	log   *slog.Logger
}

// The device's own bounds: the level scale's top (the session's maximum
// sits within it; estim.DefaultLevelCap until one is set), Channel A only,
// the pattern allow-list, a tempo control.
var capabilities = estim.Capabilities{LevelMax: LCDMax, Channels: []string{"a"}, Modes: AllowedModes, Tempo: true}

// Capabilities describes what the service may ask of the device.
func Capabilities() estim.Capabilities { return capabilities }

// Connect establishes a session on a device whose port is open: it resumes
// with the stored key when there is one and the device still answers to
// it, and handshakes otherwise. It does not touch the outputs; the Runtime
// releases every fresh connection.
func Connect(ctx context.Context, dev *Device, store KeyStore, log *slog.Logger) (*Driver, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	path := dev.Path()
	resumed := false
	if store != nil {
		if key, ok, err := store.Load(); err == nil && ok {
			switch err := dev.Resume(ctx, key); {
			case err == nil:
				resumed = true
				log.Info("estim: resumed the device's session key from a previous run", "port", path)
			case IsProtocolError(err):
				// The device answered and the key did not decode: it was
				// power-cycled since the key was saved. Silence or a
				// transport failure says nothing about the key and keeps it.
				log.Info("estim: stored session key rejected", "err", err)
				_ = store.Clear()
			default:
				return nil, err
			}
		}
	}
	if !resumed {
		if err := dev.Handshake(ctx); err != nil {
			return nil, err
		}
		if store != nil {
			if err := store.Save(dev.Key()); err != nil {
				log.Warn("estim: could not save the session key", "err", err)
			}
		}
	}
	return &Driver{Device: dev, store: store, log: log}, nil
}

// Kind is estim.KindMK312BT.
func (d *Driver) Kind() estim.Kind { return estim.KindMK312BT }

// Label is the device's name.
func (d *Driver) Label() string { return Label }

// Port is the serial port path.
func (d *Driver) Port() string { return d.Path() }

// Capabilities is what the service may ask.
func (d *Driver) Capabilities() estim.Capabilities { return capabilities }

// Arm selects the power range for the armed window, the session's normal
// or high; both channels are zeroed on the way. The low range is not the
// service's to select.
func (d *Driver) Arm(ctx context.Context, powerMode string) error {
	power, ok := PowerOf(powerMode)
	if !ok {
		return fmt.Errorf("mk312: power range must be %q or %q", estim.PowerModeNormal, estim.PowerModeHigh)
	}
	return d.SetPowerLevel(ctx, power)
}

// Execute runs one actuation command within the caps the Runtime checked;
// levelMax is the level no step may pass (the session's maximum, the
// device's scale at most). Channel B is pinned back to zero after every
// command.
func (d *Driver) Execute(ctx context.Context, cmd estim.Command, levelMax int, cancelled func() bool) (estim.Result, error) {
	res := estim.Result{Verb: cmd.Verb}
	levelMax = clamp(levelMax, 0, LCDMax)
	switch cmd.Verb {
	case "set_mode":
		if cmd.Mode == nil || !ModeAllowed(*cmd.Mode) {
			return res, fmt.Errorf("mk312: pattern is not allowed")
		}
		if err := d.SetMode(ctx, *cmd.Mode); err != nil {
			return res, err
		}
		res.Mode = estim.Int(*cmd.Mode)
	case "set_level":
		if cmd.Level == nil || *cmd.Level < 0 || *cmd.Level > levelMax {
			return res, fmt.Errorf("mk312: level must be 0..%d", levelMax)
		}
		applied, err := d.RampLevelA(ctx, *cmd.Level, cancelled)
		if err != nil {
			return res, err
		}
		res.Level = estim.Int(applied)
	case "adjust_level":
		if cmd.Delta == nil || *cmd.Delta == 0 || *cmd.Delta < -estim.LevelDeltaCap || *cmd.Delta > estim.LevelDeltaCap {
			return res, fmt.Errorf("mk312: delta must be a non-zero integer from -%d to %d", estim.LevelDeltaCap, estim.LevelDeltaCap)
		}
		current, err := d.LevelA(ctx)
		if err != nil {
			return res, err
		}
		target := clamp(current+*cmd.Delta, 0, levelMax)
		applied, err := d.RampLevelA(ctx, target, cancelled)
		if err != nil {
			return res, err
		}
		res.PreviousLevel, res.Level = estim.Int(current), estim.Int(applied)
	case "set_ma":
		if cmd.Percent == nil || *cmd.Percent < 0 || *cmd.Percent > MAPercentCap {
			return res, fmt.Errorf("mk312: percent must be 0..%d", MAPercentCap)
		}
		raw, err := d.SetLevelMA(ctx, *cmd.Percent)
		if err != nil {
			return res, err
		}
		res.Percent, res.LevelMA = estim.Int(*cmd.Percent), estim.Int(raw)
	case "adjust_ma":
		if cmd.Delta == nil || *cmd.Delta == 0 || *cmd.Delta < -estim.TempoDeltaCap || *cmd.Delta > estim.TempoDeltaCap {
			return res, fmt.Errorf("mk312: delta must be a non-zero integer from -%d to %d", estim.TempoDeltaCap, estim.TempoDeltaCap)
		}
		lo, hi, err := d.MARange(ctx)
		if err != nil {
			return res, err
		}
		raw, err := d.LevelMA(ctx)
		if err != nil {
			return res, err
		}
		current, ok := MARawToPercent(raw, lo, hi)
		if !ok {
			return res, fmt.Errorf("mk312: cannot adjust tempo over degenerate range [%d,%d]", lo, hi)
		}
		target := clamp(current+*cmd.Delta, 0, MAPercentCap)
		applied, err := d.SetLevelMA(ctx, target)
		if err != nil {
			return res, err
		}
		res.PreviousPercent, res.Percent, res.LevelMA = estim.Int(current), estim.Int(target), estim.Int(applied)
	default:
		return res, fmt.Errorf("mk312: unsupported command: %s", cmd.Verb)
	}
	// Reassert state the service does not control. Power is fixed for the
	// armed window, so only Channel B is forced back to zero.
	if err := d.Poke(ctx, AddressLevelB, 0); err != nil {
		return res, err
	}
	return res, nil
}

// Close ends the session, releasing first when restore is set, and keeps
// the key store truthful: a close that got through has unkeyed the device,
// so a stored key would be stale; a close that failed says the device is
// out of reach, still keyed if it was, and the stored key is then the only
// way back to it once the cable or the device returns.
func (d *Driver) Close(ctx context.Context, restore bool) error {
	keyed := d.Key() != NoKey
	err := d.Device.Close(ctx, restore)
	if err == nil && keyed && d.store != nil {
		_ = d.store.Clear()
	}
	if cerr := d.Device.port.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return err
}
