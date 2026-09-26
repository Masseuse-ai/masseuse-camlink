package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/estim"
	"github.com/FemLed/masseuse-camlink/internal/rendezvous"
)

// estimLink serves the stimulation device reachable from this computer
// (docs/PROTOCOL.md, section 7): it finds the device through the registered
// device families (drivers.go), holds it released until the service
// attaches a live session, relays that session's bounded commands and
// reports status and telemetry through the rendezvous client. Nothing
// about it needs turning on: a supported device that is switched on
// nearby is served for whatever session uses this connector.
type estimLink struct {
	finders estim.Finders
	rt      *estim.Runtime
	session *estim.Session
	client  *rendezvous.Client
	log     *slog.Logger
	out     func(format string, args ...any)

	mu       sync.Mutex
	lastDesc *estim.Descriptor
	ctx      context.Context
}

// newEstimLink prepares the link over the device families the flags leave
// on.
func newEstimLink(stateDir string, log *slog.Logger, out func(string, ...any)) *estimLink {
	l := &estimLink{log: log, out: out}
	l.finders = deviceFinders(finderConfig{stateDir: stateDir, log: log})
	l.rt = &estim.Runtime{Connect: l.finders.Find, Log: log}
	l.rt.OnDevice = l.deviceChanged
	l.session = &estim.Session{Runtime: l.rt, Uplink: l, Log: log}
	return l
}

// run supervises the device and the session until ctx ends; both release
// the device on the way out, and the finders let go of what they hold
// (the Bluetooth central).
func (l *estimLink) run(ctx context.Context) {
	l.mu.Lock()
	l.ctx = ctx
	l.mu.Unlock()
	if len(l.finders) == 0 {
		l.log.Info("estim: every device family is switched off by the flags; no stimulation device is served")
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); l.rt.Run(ctx) }()
	go func() { defer wg.Done(); l.session.Run(ctx) }()
	wg.Wait()
	if err := l.finders.Close(); err != nil {
		l.log.Debug("estim: closing the device finders", "err", err)
	}
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

// probeEstim is the `estim probe` command: say what each device family can
// see, find the device, print what it reports, and leave it released.
func probeEstim(ctx context.Context, stateDir string, log *slog.Logger) int {
	finders := deviceFinders(finderConfig{stateDir: stateDir, log: log})
	defer func() { _ = finders.Close() }()
	if len(finders) == 0 {
		fmt.Fprintf(os.Stderr, "Every device family is switched off by the flags (%s).\n", strings.Join(familyNames(), ", "))
		return 2
	}
	pctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if err := finders.Describe(pctx, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
	}
	fmt.Println()
	drv, err := finders.Find(pctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		if errors.Is(err, estim.ErrNoDevice) {
			fmt.Fprintln(os.Stderr, "No device answered. Check that it is switched on and within reach, and that nothing else has it open.")
		}
		return 1
	}
	fmt.Printf("Found %s (%s).\n", drv.Label(), drv.Port())
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
		fmt.Println("Released: output stopped and at zero, the device's own controls live. It is ready for a session.")
	}
	return code
}
