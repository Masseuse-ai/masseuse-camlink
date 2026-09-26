package mastago

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/ble"
	"github.com/FemLed/masseuse-camlink/internal/estim"
)

// Search timings.
const (
	// DefaultScanWindow bounds one scan for advertising units.
	DefaultScanWindow = 4 * time.Second
	// HeldConnectTimeout bounds a connection to a unit the system already
	// holds for another program: such a connection completes in well
	// under a second, and one that does not means the unit is off.
	HeldConnectTimeout = 5 * time.Second
	// ConnectTimeout bounds a connection to an advertising unit.
	ConnectTimeout = 15 * time.Second
)

// Finder finds a unit over Bluetooth: first among the peripherals the
// system already holds a connection to that offer the unit's service (the
// vendor's own app may have it open on this computer), then by a bounded
// scan for advertising units. It is the estim.Finder the program
// registers.
type Finder struct {
	// Open opens the system's Bluetooth central; ble.Open when nil.
	Open func(ctx context.Context, log *slog.Logger) (ble.Central, error)
	// Pin, when set, restricts the search to the unit whose advertised
	// name ("MASTOGO G-12AB"), name suffix ("G-12AB") or system identifier
	// matches it. Select changes it while the program runs.
	Pin string
	// ScanWindow bounds one scan (DefaultScanWindow when zero).
	ScanWindow time.Duration
	Log        *slog.Logger

	mu      sync.Mutex
	central ble.Central
}

// NewFinder is a Finder for the system's Bluetooth, pinned to pin when it
// is not empty.
func NewFinder(pin string, log *slog.Logger) *Finder {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Finder{Pin: pin, Log: log}
}

func (f *Finder) log() *slog.Logger {
	if f.Log != nil {
		return f.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (f *Finder) window() time.Duration {
	if f.ScanWindow <= 0 {
		return DefaultScanWindow
	}
	return f.ScanWindow
}

// centralFor opens the Bluetooth central once and keeps it.
func (f *Finder) centralFor(ctx context.Context) (ble.Central, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.central != nil {
		return f.central, nil
	}
	open := f.Open
	if open == nil {
		open = ble.Open
	}
	c, err := open(ctx, f.log())
	if err != nil {
		return nil, err
	}
	f.central = c
	return c, nil
}

// Close lets go of the Bluetooth central.
func (f *Finder) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.central == nil {
		return nil
	}
	err := f.central.Close()
	f.central = nil
	return err
}

// IsUnit says whether an advertisement is a unit: the vendor's name
// prefix, or the unit's service.
func IsUnit(a ble.Advertisement) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(a.Name)), AdvertisedPrefix) || a.HasService(ServiceUUID)
}

// pin is the Pin as of now, read under the lock Select writes it under.
func (f *Finder) pin() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.TrimSpace(f.Pin)
}

// Select is estim.Selector: the Pin from here on. A unit of another
// family (a serial port path) matches no advertisement, so the program
// may give one selection to every family.
func (f *Finder) Select(unit string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Pin = strings.TrimSpace(unit)
}

// Matches says whether an advertisement is a unit the Pin allows.
func (f *Finder) Matches(a ble.Advertisement) bool {
	if !IsUnit(a) {
		return false
	}
	pin := f.pin()
	if pin == "" {
		return true
	}
	name := strings.TrimSpace(a.Name)
	suffix := strings.TrimSpace(strings.TrimPrefix(strings.ToUpper(name), AdvertisedPrefix))
	return strings.EqualFold(pin, name) || strings.EqualFold(pin, suffix) || strings.EqualFold(pin, a.ID)
}

// List is estim.Lister: every unit in reach, the Pin notwithstanding: the
// peripherals the system already holds for another program (Held), then
// the units advertising during one scan window. Nothing is connected. A
// computer without Bluetooth has no units and no error; Bluetooth off or
// refused is reported as itself.
func (f *Finder) List(ctx context.Context) ([]estim.Unit, error) {
	c, err := f.centralFor(ctx)
	if err != nil {
		if errors.Is(err, ble.ErrUnsupported) {
			return nil, nil
		}
		return nil, err
	}
	// The scan reports on the central's goroutine; the list is read once
	// the scan is over, under the same lock.
	var mu sync.Mutex
	var units []estim.Unit
	seen := map[string]bool{}
	held, err := c.ConnectedWithService(ctx, ServiceUUID)
	if err != nil {
		f.log().Debug("mastago: listing held peripherals", "err", err)
	}
	for _, a := range held {
		if !IsUnit(a) || seen[a.ID] {
			continue
		}
		seen[a.ID] = true
		units = append(units, estim.Unit{ID: a.ID, Kind: estim.KindMastago, Label: LabelFor(a.Name), Held: true})
	}
	sctx, cancel := context.WithTimeout(ctx, f.window())
	defer cancel()
	_, scanErr := c.Scan(sctx, func(a ble.Advertisement) bool {
		mu.Lock()
		defer mu.Unlock()
		if IsUnit(a) && !seen[a.ID] {
			seen[a.ID] = true
			units = append(units, estim.Unit{ID: a.ID, Kind: estim.KindMastago, Label: LabelFor(a.Name)})
		}
		return false
	})
	mu.Lock()
	out := append([]estim.Unit(nil), units...)
	mu.Unlock()
	if scanErr != nil && ctx.Err() != nil {
		return out, ctx.Err()
	}
	return out, nil
}

// Find returns a driver for the first unit that answers, or an error
// wrapping estim.ErrNoDevice when there is none. A computer without
// Bluetooth is "no device"; Bluetooth switched off or refused to this
// program is reported as itself, so it is logged as a warning once.
func (f *Finder) Find(ctx context.Context) (estim.Driver, error) {
	c, err := f.centralFor(ctx)
	if err != nil {
		if errors.Is(err, ble.ErrUnsupported) {
			return nil, fmt.Errorf("%w: %v", estim.ErrNoDevice, err)
		}
		return nil, err
	}
	held, err := c.ConnectedWithService(ctx, ServiceUUID)
	if err != nil {
		f.log().Debug("mastago: listing held peripherals", "err", err)
	}
	var firstErr error
	for _, a := range held {
		if !f.Matches(a) {
			continue
		}
		drv, err := f.connect(ctx, c, a, HeldConnectTimeout)
		if err == nil {
			return drv, nil
		}
		f.log().Debug("mastago: held unit not usable", "unit", a.Name, "err", err)
		if firstErr == nil && !errors.Is(err, context.DeadlineExceeded) {
			firstErr = err
		}
	}
	sctx, cancel := context.WithTimeout(ctx, f.window())
	adv, err := c.Scan(sctx, f.Matches)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if firstErr != nil {
			return nil, firstErr
		}
		if pin := f.pin(); pin != "" {
			return nil, fmt.Errorf("%w: no unit matching %q is advertising", estim.ErrNoDevice, pin)
		}
		return nil, fmt.Errorf("%w: no unit is advertising; hold its power button until it switches on", estim.ErrNoDevice)
	}
	drv, err := f.connect(ctx, c, adv, ConnectTimeout)
	if err != nil {
		return nil, fmt.Errorf("mastago: %s: %w", adv.Name, err)
	}
	return drv, nil
}

func (f *Finder) connect(ctx context.Context, c ble.Central, a ble.Advertisement, timeout time.Duration) (*Driver, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := c.Connect(cctx, a.ID)
	if err != nil {
		return nil, err
	}
	drv, err := Connect(cctx, conn, f.log())
	if err != nil {
		return nil, err
	}
	drv.held = a.Connected
	f.log().Info("mastago: unit connected", "unit", conn.Name(), "id", conn.ID(), "held", a.Connected)
	return drv, nil
}

// Describe writes what the finder can see: the Bluetooth state, the units
// the system already holds, and the units advertising during one scan
// window, with the ones the Pin would skip marked.
func (f *Finder) Describe(ctx context.Context, out io.Writer) error {
	fmt.Fprintf(out, "Bluetooth (Mastago TENS units, service %s):\n", strings.ToUpper(ServiceUUID.Short()))
	c, err := f.centralFor(ctx)
	if err != nil {
		fmt.Fprintf(out, "  not available: %v\n", err)
		return err
	}
	if pin := f.pin(); pin != "" {
		fmt.Fprintf(out, "  pinned to %q\n", pin)
	}
	held, err := c.ConnectedWithService(ctx, ServiceUUID)
	if err != nil {
		fmt.Fprintf(out, "  held peripherals: %v\n", err)
	}
	for _, a := range held {
		fmt.Fprintf(out, "  %s  %q  held by another program%s\n", a.ID, a.Name, f.skipNote(a))
	}
	seen := map[string]bool{}
	sctx, cancel := context.WithTimeout(ctx, f.window())
	defer cancel()
	_, scanErr := c.Scan(sctx, func(a ble.Advertisement) bool {
		if IsUnit(a) && !seen[a.ID] {
			seen[a.ID] = true
			fmt.Fprintf(out, "  %s  %q  advertising, %d dBm%s\n", a.ID, a.Name, a.RSSI, f.skipNote(a))
		}
		return false
	})
	if scanErr != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if len(held) == 0 && len(seen) == 0 {
		fmt.Fprintln(out, "  no unit is held or advertising; hold the unit's power button until it switches on")
	}
	return nil
}

func (f *Finder) skipNote(a ble.Advertisement) string {
	if f.Matches(a) {
		return "  (will be tried)"
	}
	return "  (skipped: not the pinned unit)"
}
