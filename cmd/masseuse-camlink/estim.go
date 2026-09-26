package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/estim"
	"github.com/FemLed/masseuse-camlink/internal/rendezvous"
)

// heldByAnotherLine is printed when the unit served is one another program
// on this computer has open (the vendor's app, a script keeping it awake):
// the connector shares the Bluetooth link, so both programs' commands reach
// the unit and either's replies may answer the other's questions.
const heldByAnotherLine = "Another program on this computer has this unit open. Close it before a session, or its commands and Masseuse.ai's will collide."

// estimLink serves the stimulation device reachable from this computer
// (docs/PROTOCOL.md, section 7): it finds the device through the registered
// device families (drivers.go), holds it released until the service
// attaches a live session, relays that session's bounded commands and
// reports status and telemetry through the rendezvous client. Nothing
// about it needs turning on: a supported device that is switched on
// nearby is served for whatever session uses this connector.
type estimLink struct {
	finders  estim.Finders
	rt       *estim.Runtime
	session  *estim.Session
	client   *rendezvous.Client
	log      *slog.Logger
	out      func(format string, args ...any)
	stateDir string
	// stdin is where the console picker reads a number from; nil for no
	// picker (not a terminal).
	stdin io.Reader

	mu       sync.Mutex
	lastDesc *estim.Descriptor
	// units is the list as last printed, in the order the numbers refer to.
	units []estim.Unit
	// selection is the unit chosen (estim.json); "" is the first found.
	selection string
	pickerOn  bool
	ctx       context.Context
}

// newEstimLink prepares the link over the device families the flags leave
// on, restricted to the remembered unit when there is one.
func newEstimLink(stateDir string, log *slog.Logger, out func(string, ...any)) *estimLink {
	l := &estimLink{log: log, out: out, stateDir: stateDir}
	l.finders = deviceFinders(finderConfig{stateDir: stateDir, log: log})
	selection, err := resolveEstimSelection(stateDir, *estimUnit)
	if err != nil {
		log.Warn("estim: could not remember the unit selected", "err", err)
	}
	// A family's own pin on the command line (-estim-ble) is this run's
	// word when -estim-unit is not given; a remembered selection would
	// silently override it otherwise.
	if strings.TrimSpace(*estimUnit) == "" && familyPinned() {
		selection = ""
	}
	l.selection = selection
	if selection != "" {
		l.finders.Select(selection)
		log.Info("estim: serving the unit selected", "unit", selection)
	}
	// Closures over the link, so the finders may be replaced (a test's fake
	// central) and the Runtime follows.
	l.rt = &estim.Runtime{
		Connect: func(ctx context.Context) (estim.Driver, error) { return l.finders.Find(ctx) },
		List:    func(ctx context.Context) ([]estim.Unit, error) { return l.finders.List(ctx) },
		Select:  func(unit string) { l.finders.Select(unit) },
		Log:     log,
	}
	l.rt.OnDevice = l.deviceChanged
	l.rt.OnUnits = l.unitsChanged
	l.session = &estim.Session{Runtime: l.rt, Uplink: l, Log: log, Select: l.selectUnit}
	if stdinInteractive() {
		l.stdin = os.Stdin
	}
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

// selectUnit is one selection from wherever it came (the console picker,
// the phone's device_select): remembered for the next start, then applied
// by the Runtime, which lets go of another unit held and opens this one.
// estim.ErrArmed while the unit is armed; the phone stops it first.
func (l *estimLink) selectUnit(ctx context.Context, id string) error {
	if l.rt.Armed() {
		return estim.ErrArmed
	}
	l.mu.Lock()
	l.selection = id
	l.mu.Unlock()
	if err := saveEstimSelection(l.stateDir, id); err != nil {
		l.log.Warn("estim: could not remember the unit selected", "err", err)
	}
	return l.rt.SelectUnit(ctx, id)
}

// unitsChanged prints the units in reach when there is a choice to make,
// numbered so one can be picked by typing its number, and tells the
// service the list.
func (l *estimLink) unitsChanged(ctx context.Context, units []estim.Unit) {
	l.mu.Lock()
	l.units = append([]estim.Unit(nil), units...)
	startPicker := len(units) > 1 && l.stdin != nil && !l.pickerOn
	if startPicker {
		l.pickerOn = true
	}
	l.mu.Unlock()
	if len(units) > 1 {
		desc := l.rt.Descriptor()
		l.out("%s", unitListing(units, &desc, l.stdin != nil))
	}
	if startPicker {
		go l.readPicks(l.context(), l.stdin)
	}
	l.session.UnitsChanged(ctx, units)
}

// unitListing is the console's numbered list of the units in reach, the
// served one marked; withPicker adds how to switch.
func unitListing(units []estim.Unit, serving *estim.Descriptor, withPicker bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Stimulation units in reach (%d):\n", len(units))
	for i, u := range units {
		note := ""
		switch {
		case serving != nil && serving.Connected && serving.ID == u.ID:
			note = "  (serving this one)"
		case u.Held:
			note = "  (another program on this computer has it open)"
		}
		fmt.Fprintf(&b, "  %d  %s%s\n", i+1, u.Label, note)
	}
	if withPicker {
		b.WriteString("Type a number and Enter to serve another unit; the phone can pick one too. A unit in use by a session is switched once the session stops it.\n")
	}
	return b.String()
}

// readPicks reads numbers typed at the console and selects the unit each
// names in the list last printed. Anything else typed is left alone.
func (l *estimLink) readPicks(ctx context.Context, in io.Reader) {
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		if ctx.Err() != nil {
			return
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		n, err := strconv.Atoi(line)
		if err != nil {
			continue
		}
		l.pick(ctx, n)
	}
}

// pick serves the unit numbered n in the list last printed.
func (l *estimLink) pick(ctx context.Context, n int) {
	l.mu.Lock()
	units := append([]estim.Unit(nil), l.units...)
	l.mu.Unlock()
	if n < 1 || n > len(units) {
		l.out("There is no unit %d in the list; the numbers are 1 to %d.\n", n, len(units))
		return
	}
	u := units[n-1]
	l.out("Switching to %s.\n", u.Label)
	if err := l.selectUnit(ctx, u.ID); err != nil {
		switch {
		case errors.Is(err, estim.ErrArmed):
			l.out("Not now: a session on your phone is using the unit. Stop the unit on the phone first.\n")
		case errors.Is(err, estim.ErrNoDevice):
			l.out("%s did not answer; it is served as soon as it does.\n", u.Label)
		default:
			l.out("Could not switch to %s: %v\n", u.Label, err)
		}
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
		if d.Held {
			l.out("%s\n", heldByAnotherLine)
		}
	case l.context().Err() != nil:
		// The connector is exiting and let go of the device on purpose;
		// the service is told, the person is not.
	default:
		l.out("%s\n", disconnectedLine(d.Reason))
	}
	l.session.DeviceChanged(ctx, d)
}

// disconnectedLine is the console's word on a unit gone, by the reason the
// connector knows (estim.Descriptor.Reason): what to do about it differs.
func disconnectedLine(reason string) string {
	switch reason {
	case estim.ReasonIdleOff:
		return "Stimulation device disconnected: it switched itself off after sitting idle at zero. Press its power button; it reconnects on its own."
	case estim.ReasonOutputOff:
		return "Stimulation device disconnected: it switched itself off after running for too long. Press its power button; it reconnects on its own."
	case estim.ReasonBatteryOff:
		return "Stimulation device disconnected: it switched itself off, battery low. Charge it; it reconnects on its own when it is on again."
	case estim.ReasonButtonOff:
		return "Stimulation device disconnected: it was switched off at its power button. It reconnects on its own when it is on again."
	case estim.ReasonLinkLost:
		return "Stimulation device disconnected: the link to it dropped. It reconnects on its own once it is in reach again."
	case estim.ReasonLetGo:
		return "Stimulation device let go for the unit selected."
	}
	return "Stimulation device disconnected."
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
	// A refusal of the messages themselves (400 malformed, 413 too large,
	// 422): the same bytes would be refused again. A signature or clock
	// check (401), a timeout (408) and a rate limit (429) are about the
	// moment and are retried, as is anything 5xx or network.
	var status *rendezvous.StatusError
	if errors.As(err, &status) && status.Status/100 == 4 &&
		status.Status != http.StatusUnauthorized && status.Status != http.StatusRequestTimeout && status.Status != http.StatusTooManyRequests {
		return fmt.Errorf("%w: %v", estim.ErrRefused, err)
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
	fmt.Println(registerHelpers(ctx, stateDir, log))
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
