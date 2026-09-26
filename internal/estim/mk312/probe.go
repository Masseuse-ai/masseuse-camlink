package mk312

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/estim"
	"github.com/FemLed/masseuse-camlink/internal/serialport"
)

// PassiveListen is how long a probe listens for the beacon before it sends
// anything: an unkeyed device beacons continuously, so a device is found
// without writing a byte to a port that may belong to something else.
const PassiveListen = time.Second

// ErrKeyed is returned when a device answers but will not exchange keys: it
// still holds the key of a session this connector has no record of.
var ErrKeyed = errors.New("mk312: the device holds an old session key; power it off for about ten seconds and back on")

// Probe opens a candidate port and looks for an MK-312BT behind it. It
// listens first; a port that is silent and has no stored key is only spoken
// to when the adapter is one the device ships with. On success the driver
// owns the port; on failure the port is closed.
func Probe(ctx context.Context, cand serialport.Candidate, store KeyStore, log *slog.Logger) (*Driver, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	port, err := serialport.Open(cand.Path, serialport.Config{BaudRate: Baud, ReadTimeout: ReadTimeout})
	if err != nil {
		return nil, err
	}
	return ProbePort(ctx, port, cand, store, log, ProbeOptions{})
}

// ProbeOptions tune a probe; the zero value is the real device's timing.
type ProbeOptions struct {
	// Listen is how long to listen for the beacon (PassiveListen).
	Listen time.Duration
	// Tune, when set, adjusts the Device before anything is sent.
	Tune func(*Device)
}

// ProbePort is Probe on a port that is already open. The port is closed on
// failure.
func ProbePort(ctx context.Context, port serialport.Port, cand serialport.Candidate, store KeyStore, log *slog.Logger, opts ProbeOptions) (*Driver, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if opts.Listen <= 0 {
		opts.Listen = PassiveListen
	}
	drv, err := probe(ctx, port, cand, store, log, opts)
	if err != nil {
		_ = port.Close()
		return nil, err
	}
	return drv, nil
}

func probe(ctx context.Context, port serialport.Port, cand serialport.Candidate, store KeyStore, log *slog.Logger, opts ProbeOptions) (*Driver, error) {
	dev := New(port, cand.Path)
	if opts.Tune != nil {
		opts.Tune(dev)
	}
	beaconing, err := listen(ctx, port, opts.Listen)
	if err != nil {
		return nil, err
	}
	_ = port.ResetInput()
	stored := false
	if store != nil {
		_, stored, _ = store.Load()
	}
	switch {
	case beaconing:
		// Unkeyed: any stored key is from a session the device has since
		// forgotten.
		if stored {
			_ = store.Clear()
		}
	case stored:
		// Silent and possibly still ours: Connect tries the key first.
	case !cand.Likely():
		return nil, fmt.Errorf("%w: %s is silent and not an adapter the device ships with", estim.ErrNoDevice, cand)
	default:
		// Silent, no key. A keyed device answers a sync byte at once with a
		// NAK, which is what the handshake needs to diagnose it; an absent
		// one costs one short try.
		dev.SyncTries = 2
	}
	drv, err := Connect(ctx, dev, store, log)
	if err != nil {
		switch {
		case errors.Is(err, ErrSilent), errors.Is(err, ErrNoBeacon):
			// Nothing answered: an empty port.
			return nil, fmt.Errorf("%w: %s did not answer", estim.ErrNoDevice, cand)
		case errors.Is(err, ErrNoKeyExchange) && !beaconing:
			// Something answered the sync byte (a NAK) yet nothing beaconed
			// beforehand: a device still keyed from a session this connector
			// has no record of.
			return nil, ErrKeyed
		}
		return nil, err
	}
	return drv, nil
}

// listen reads for up to d and reports whether a beacon byte arrived.
func listen(ctx context.Context, port serialport.Port, d time.Duration) (bool, error) {
	deadline := time.Now().Add(d)
	buf := make([]byte, 64)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		n, err := port.Read(buf)
		if err != nil {
			return false, fmt.Errorf("mk312: read: %w", err)
		}
		for _, b := range buf[:n] {
			if b == BeaconByte {
				return true, nil
			}
		}
	}
	return false, nil
}

// Scan tries the adapters on this computer in order and returns the first
// MK-312BT found. With path set only that port is tried, and it is spoken
// to even if it is not an adapter the device ships with. Without a device
// the error wraps estim.ErrNoDevice; a device that cannot be used (ErrKeyed,
// a handshake failure) is reported as itself.
func Scan(ctx context.Context, path string, store KeyStore, log *slog.Logger) (*Driver, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	var cands []serialport.Candidate
	if path != "" {
		cands = []serialport.Candidate{{Path: path, VendorID: serialport.FTDIVendor}}
	} else {
		var err error
		if cands, err = serialport.Candidates(ctx); err != nil {
			return nil, err
		}
	}
	var firstErr error
	tried := 0
	for _, c := range cands {
		if !c.Likely() && path == "" {
			continue
		}
		tried++
		drv, err := Probe(ctx, c, store, log)
		if err == nil {
			return drv, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		log.Debug("estim: probe", "port", c.String(), "err", err)
		if !errors.Is(err, estim.ErrNoDevice) && firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", c.Path, err)
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	if tried == 0 {
		return nil, fmt.Errorf("%w: no USB serial adapter is connected", estim.ErrNoDevice)
	}
	return nil, fmt.Errorf("%w: %d serial adapter(s) answered nothing", estim.ErrNoDevice, tried)
}
