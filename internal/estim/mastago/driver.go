package mastago

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/ble"
	"github.com/FemLed/masseuse-camlink/internal/estim"
)

// LabelPrefix starts the label people see; the unit's own suffix
// ("G-12AB") follows it.
const LabelPrefix = "Mastago TENS"

// LevelMaxDefault is the intensity cap that applies until the attached
// session sets its own: a little over half the unit's scale.
const LevelMaxDefault = 15

// The unit's own bounds, as the service sees them: one channel, intensity
// on a 0..25 scale, programs 0..31 by the unit's own numbering, no tempo
// control, one power range, a countdown the connector arms to the arm
// window, and electrode contact detection.
var capabilities = estim.Capabilities{
	LevelMax:        LevelMax,
	Channels:        []string{"a"},
	Modes:           AllowedModes,
	Tempo:           false,
	PowerModes:      nil,
	LevelMaxDefault: LevelMaxDefault,
	Timer:           true,
	LoadDetect:      true,
}

// Capabilities describes what the service may ask of the unit.
func Capabilities() estim.Capabilities { return capabilities }

// Driver is the estim.Driver for one connected unit.
type Driver struct {
	*Device
	label string
	log   *slog.Logger
	// ArmWindow is the countdown Arm writes; estim.MaxArmWindow unless a
	// test shortens it.
	ArmWindow time.Duration
	// held says the system already held the unit for another program when
	// this link was opened (estim.HeldReporter).
	held bool
}

// Held is estim.HeldReporter: another program on this computer had the
// unit open when the connector connected, so the two share the link.
func (d *Driver) Held() bool { return d.held }

// LabelFor is the label for a unit with the advertised name: the unit's
// own suffix after the vendor prefix ("Mastago TENS G-12AB").
func LabelFor(name string) string {
	suffix := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(name), AdvertisedPrefix))
	if suffix == "" {
		return LabelPrefix
	}
	return LabelPrefix + " " + suffix
}

// Connect starts the protocol on an open link and verifies the unit
// answers (AT+CMODE?). It does not touch the outputs; the Runtime releases
// every fresh connection. On failure the link is closed.
func Connect(ctx context.Context, conn ble.Conn, log *slog.Logger) (*Driver, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	dev := New(conn, log)
	if err := dev.Listen(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if _, err := dev.Mode(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("mastago: %s did not answer as a unit: %w", conn.Name(), err)
	}
	return &Driver{Device: dev, label: LabelFor(conn.Name()), log: log, ArmWindow: estim.MaxArmWindow}, nil
}

// Kind is estim.KindMastago.
func (d *Driver) Kind() estim.Kind { return estim.KindMastago }

// Label names the unit for people.
func (d *Driver) Label() string { return d.label }

// Port is the system's identifier for the unit.
func (d *Driver) Port() string { return d.ID() }

// Capabilities is what the service may ask.
func (d *Driver) Capabilities() estim.Capabilities { return capabilities }

// Arm arms the unit's countdown to the arm window, so the unit stops its
// output by itself if the connector dies with it armed. The unit has one
// power range; powerMode is accepted and ignored. Outputs are already at
// zero: the Runtime released before arming.
func (d *Driver) Arm(ctx context.Context, powerMode string) error {
	if powerMode != estim.PowerModeNormal && powerMode != estim.PowerModeHigh {
		return fmt.Errorf("mastago: power range must be %q or %q", estim.PowerModeNormal, estim.PowerModeHigh)
	}
	return d.SetTimer(ctx, int(d.window().Seconds()))
}

func (d *Driver) window() time.Duration {
	if d.ArmWindow <= 0 || d.ArmWindow > estim.MaxArmWindow {
		return estim.MaxArmWindow
	}
	return d.ArmWindow
}

// RenewArm brings the countdown up to the renewed arm window (the
// estim.ArmRenewer hook) without touching the output.
func (d *Driver) RenewArm(ctx context.Context, until time.Time) error {
	seconds := int(time.Until(until).Seconds())
	if seconds < TimerMinS {
		seconds = TimerMinS
	}
	return d.SetTimer(ctx, seconds)
}

// Execute runs one actuation command within the caps the Runtime checked;
// levelMax is the intensity no step may pass.
func (d *Driver) Execute(ctx context.Context, cmd estim.Command, levelMax int, cancelled func() bool) (estim.Result, error) {
	res := estim.Result{Verb: cmd.Verb}
	levelMax = max(0, min(levelMax, LevelMax))
	switch cmd.Verb {
	case "set_mode":
		if cmd.Mode == nil || !ModeAllowed(*cmd.Mode) {
			return res, fmt.Errorf("mastago: program is not allowed")
		}
		before, err := d.Level(ctx)
		if err != nil {
			return res, err
		}
		// SetMode pauses the output, as the unit's own controller does;
		// the intensity the session had is brought back with a ramp.
		if err := d.SetMode(ctx, *cmd.Mode); err != nil {
			return res, err
		}
		res.Mode = estim.Int(*cmd.Mode)
		if before > 0 {
			applied, err := d.Ramp(ctx, min(before, levelMax), cancelled)
			res.Level = estim.Int(applied)
			if err != nil {
				return res, err
			}
		}
	case "set_level":
		if cmd.Level == nil || *cmd.Level < 0 || *cmd.Level > levelMax {
			return res, fmt.Errorf("mastago: intensity must be 0..%d", levelMax)
		}
		applied, err := d.Ramp(ctx, *cmd.Level, cancelled)
		res.Level = estim.Int(applied)
		if err != nil {
			return res, err
		}
	case "adjust_level":
		if cmd.Delta == nil || *cmd.Delta == 0 || *cmd.Delta < -estim.LevelDeltaCap || *cmd.Delta > estim.LevelDeltaCap {
			return res, fmt.Errorf("mastago: delta must be a non-zero integer from -%d to %d", estim.LevelDeltaCap, estim.LevelDeltaCap)
		}
		current, err := d.Level(ctx)
		if err != nil {
			return res, err
		}
		target := max(0, min(current+*cmd.Delta, levelMax))
		applied, err := d.Ramp(ctx, target, cancelled)
		res.PreviousLevel, res.Level = estim.Int(current), estim.Int(applied)
		if err != nil {
			return res, err
		}
	case "set_ma", "adjust_ma":
		return res, fmt.Errorf("mastago: the unit has no tempo control")
	default:
		return res, fmt.Errorf("mastago: unsupported command: %s", cmd.Verb)
	}
	return res, nil
}

// Status is the unit's full reading, with the link's end reported as the
// status error once it has happened.
func (d *Driver) Status(ctx context.Context) (estim.Status, error) {
	if err := d.LinkError(); err != nil {
		return estim.Status{}, err
	}
	return d.Device.Status(ctx)
}
