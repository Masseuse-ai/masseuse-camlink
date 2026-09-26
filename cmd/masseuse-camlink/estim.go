package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/estim"
	"github.com/FemLed/masseuse-camlink/internal/estim/mk312"
	"github.com/FemLed/masseuse-camlink/internal/rendezvous"
	"github.com/FemLed/masseuse-camlink/internal/serialport"
)

// estimLink serves the stimulation device plugged into this computer
// (docs/PROTOCOL.md, section 7): it finds the device, holds it released
// until the service attaches a live session, relays that session's bounded
// commands and reports status and telemetry through the rendezvous client.
// Nothing about it needs turning on: a supported device that is plugged in
// is served for whatever session uses this connector.
type estimLink struct {
	rt      *estim.Runtime
	session *estim.Session
	client  *rendezvous.Client
	log     *slog.Logger
	out     func(format string, args ...any)

	mu       sync.Mutex
	lastDesc *estim.Descriptor
	ctx      context.Context
}

// newEstimLink prepares the link. port, when set, names the one serial
// port to use instead of scanning.
func newEstimLink(stateDir, port string, log *slog.Logger, out func(string, ...any)) *estimLink {
	l := &estimLink{log: log, out: out}
	store := mk312.FileKeyStore{Path: filepath.Join(stateDir, "mk312-key")}
	l.rt = &estim.Runtime{
		Connect: func(ctx context.Context) (estim.Driver, error) {
			d, err := mk312.Scan(ctx, port, store, log)
			if err != nil {
				return nil, err
			}
			return d, nil
		},
		Log: log,
	}
	l.rt.OnDevice = l.deviceChanged
	l.session = &estim.Session{Runtime: l.rt, Uplink: l, Log: log}
	return l
}

// run supervises the device and the session until ctx ends; both release
// the device on the way out.
func (l *estimLink) run(ctx context.Context) {
	l.mu.Lock()
	l.ctx = ctx
	l.mu.Unlock()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); l.rt.Run(ctx) }()
	go func() { defer wg.Done(); l.session.Run(ctx) }()
	wg.Wait()
}

// context is the link's lifetime, for work started by rendezvous events.
func (l *estimLink) context() context.Context {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ctx != nil {
		return l.ctx
	}
	return context.Background()
}

// deviceChanged announces a device found or lost, on the console and to
// the service.
func (l *estimLink) deviceChanged(ctx context.Context, d estim.Descriptor) {
	l.mu.Lock()
	l.lastDesc = &d
	l.mu.Unlock()
	switch {
	case d.Connected:
		l.out("Stimulation device connected: %s. It is held at zero until a session on your phone uses this computer.\n", d.Label)
	case l.context().Err() != nil:
		// The connector is exiting and let go of the device on purpose;
		// the service is told, the person is not.
	default:
		l.out("Stimulation device disconnected.\n")
	}
	l.session.DeviceChanged(ctx, d)
}

// reportDevice repeats the device report after every hello: the service
// forgets a connector's device when it forgets the connector.
func (l *estimLink) reportDevice() {
	l.mu.Lock()
	d := l.lastDesc
	l.mu.Unlock()
	if d == nil {
		return
	}
	l.session.DeviceChanged(l.context(), *d)
}

// Send is the estim.Uplink: signed POSTs through the rendezvous client,
// with the service's answers mapped to what the session understands.
func (l *estimLink) Send(ctx context.Context, sessionID string, messages []json.RawMessage) error {
	err := l.client.PostEstim(ctx, sessionID, messages)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, rendezvous.ErrNoEstim):
		return estim.ErrUnsupported
	case errors.Is(err, rendezvous.ErrEstimSessionGone):
		return fmt.Errorf("%w: %v", estim.ErrSessionGone, err)
	}
	return err
}

// OnEstim is a message from the service for the device link.
func (l *estimLink) OnEstim(sessionID string, message json.RawMessage) {
	l.session.Handle(l.context(), sessionID, message)
}

// sessionCleared releases the device when the service ends the session
// the device is attached to.
func (l *estimLink) sessionCleared(sessionID string) {
	if sid, ok := l.session.Attached(); ok && (sessionID == "" || sid == sessionID) {
		l.session.Detach(l.context(), "session cleared")
	}
}

// probeEstim is the `estim probe` command: find the device, print what it
// reports, and leave it released and unkeyed.
func probeEstim(ctx context.Context, stateDir, port string, log *slog.Logger) int {
	cands, err := serialport.Candidates(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "serial ports:", err)
	}
	if port == "" {
		if len(cands) == 0 {
			fmt.Println("No USB serial adapter is connected.")
			fmt.Println("Plug the device's link cable into this computer and switch the device on.")
			return 1
		}
		fmt.Println("USB serial adapters:")
		for _, c := range cands {
			note := ""
			if c.Likely() {
				note = "  (will be probed)"
			}
			fmt.Printf("  %s%s\n", c, note)
		}
	}
	store := mk312.FileKeyStore{Path: filepath.Join(stateDir, "mk312-key")}
	pctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	drv, err := mk312.Scan(pctx, port, store, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		switch {
		case errors.Is(err, mk312.ErrKeyed):
			fmt.Fprintln(os.Stderr, "The device still holds the key of an earlier session. Switch it off, wait ten seconds, switch it on and run the probe again.")
		case errors.Is(err, estim.ErrNoDevice):
			fmt.Fprintln(os.Stderr, "No device answered. Check that it is on, that the link cable is seated, and that nothing else (Bluetooth, another program) has it open.")
		}
		return 1
	}
	fmt.Printf("Found %s on %s.\n", drv.Label(), drv.Port())
	code := 0
	if err := drv.Release(pctx); err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		code = 1
	}
	st, err := drv.Status(pctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "status:", err)
		code = 1
	} else {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(st)
	}
	if err := drv.Close(pctx, true); err != nil {
		fmt.Fprintln(os.Stderr, "close:", err)
		code = 1
	}
	if code == 0 {
		fmt.Println("Released: outputs at zero, front panel live, normal power. The device is ready for a session.")
	}
	return code
}
