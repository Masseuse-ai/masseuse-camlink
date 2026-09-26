package helper_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/ble"
	"github.com/FemLed/masseuse-camlink/internal/estim"
	"github.com/FemLed/masseuse-camlink/internal/estim/helper"
	"github.com/FemLed/masseuse-camlink/internal/estim/mastago"
	"github.com/FemLed/masseuse-camlink/internal/estim/mastago/fakeunit"
)

// TestMain doubles as the helper program for the spawned-process test: run
// with HELPER_TEST_GUEST set, the test binary serves a fake family on its
// stdio and exits when the Host goes away.
func TestMain(m *testing.M) {
	if os.Getenv("HELPER_TEST_GUEST") == "1" {
		log := slog.New(slog.NewTextHandler(os.Stderr, nil))
		log.Info("guest starting", "stateDir", os.Getenv("HELPER_TEST_STATE_DIR"))
		f := &fakeFinder{unit: newFakeDriver("port-1")}
		if err := helper.Serve(context.Background(), os.Stdin, os.Stdout, f, helper.Options{Name: "fake", Kinds: []estim.Kind{"fakekind"}, Log: log}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// -- a fake family, for the spawned test and the death test -----------------------

type fakeFinder struct {
	unit     *fakeDriver
	selected string
	fail     error
}

func (f *fakeFinder) Find(context.Context) (estim.Driver, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	return f.unit, nil
}
func (f *fakeFinder) Describe(_ context.Context, out io.Writer) error {
	_, err := fmt.Fprintf(out, "Fake family: one unit at %s (selected %q)\n", f.unit.port, f.selected)
	return err
}
func (f *fakeFinder) List(context.Context) ([]estim.Unit, error) {
	return []estim.Unit{{ID: f.unit.port, Kind: "fakekind", Label: "Fake unit", Held: true}}, nil
}
func (f *fakeFinder) Select(unit string) { f.selected = unit }

type fakeDriver struct {
	port     string
	level    int
	armed    string
	renewed  time.Time
	released int
	closed   bool
	lost     error
	mu       sync.Mutex
}

func newFakeDriver(port string) *fakeDriver { return &fakeDriver{port: port} }

func (d *fakeDriver) Kind() estim.Kind { return "fakekind" }
func (d *fakeDriver) Label() string    { return "Fake unit" }
func (d *fakeDriver) Port() string     { return d.port }
func (d *fakeDriver) Capabilities() estim.Capabilities {
	return estim.Capabilities{LevelMax: 50, Channels: []string{"a"}, Modes: []int{0, 1}, Timer: true}
}
func (d *fakeDriver) Held() bool { return true }
func (d *fakeDriver) Release(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.released++
	d.level = 0
	return nil
}
func (d *fakeDriver) Arm(_ context.Context, powerMode string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.armed = powerMode
	return nil
}
func (d *fakeDriver) RenewArm(_ context.Context, until time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.renewed = until
	return nil
}
func (d *fakeDriver) Status(context.Context) (estim.Status, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lost != nil {
		return estim.Status{}, d.lost
	}
	level := d.level
	return estim.Status{Connected: true, Port: estim.String(d.port), LevelA: &level, LevelB: estim.Int(0)}, nil
}
func (d *fakeDriver) Telemetry(context.Context) (estim.Frame, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	level := d.level
	return estim.Frame{AtMs: 1, LevelA: &level}, nil
}
func (d *fakeDriver) Execute(ctx context.Context, cmd estim.Command, levelMax int, cancelled func() bool) (estim.Result, error) {
	if cmd.Verb != "set_level" || cmd.Level == nil {
		return estim.Result{}, fmt.Errorf("fake: unsupported %s", cmd.Verb)
	}
	target := min(*cmd.Level, levelMax)
	// A slow ramp, one level per 20 ms, so a cancel can land midway.
	for {
		d.mu.Lock()
		at := d.level
		d.mu.Unlock()
		if at == target {
			break
		}
		if cancelled() {
			return estim.Result{}, estim.ErrCancelled
		}
		select {
		case <-ctx.Done():
			return estim.Result{}, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
		d.mu.Lock()
		if at < target {
			d.level++
		} else {
			d.level--
		}
		d.mu.Unlock()
	}
	level := target
	return estim.Result{Verb: "set_level", Level: &level}, nil
}
func (d *fakeDriver) Close(context.Context, bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	return nil
}

// -- a Host over in-process pipes -----------------------------------------------------

// pipes joins a Host to a guest running in this process. starts counts the
// Host's starts; kill closes the guest's side so its Serve ends.
type pipes struct {
	starts atomic.Int32
	mu     sync.Mutex
	guests []*guestEnd
}

type guestEnd struct {
	in   *io.PipeReader
	out  *io.PipeWriter
	done chan error
}

func (p *pipes) start(finder estim.Finder, opts helper.Options) func(ctx context.Context, path string, args []string) (helper.Process, error) {
	return func(ctx context.Context, path string, args []string) (helper.Process, error) {
		p.starts.Add(1)
		hostToGuestR, hostToGuestW := io.Pipe()
		guestToHostR, guestToHostW := io.Pipe()
		stderrR, stderrW := io.Pipe()
		g := &guestEnd{in: hostToGuestR, out: guestToHostW, done: make(chan error, 1)}
		p.mu.Lock()
		p.guests = append(p.guests, g)
		p.mu.Unlock()
		go func() {
			log := slog.New(slog.NewTextHandler(stderrW, nil))
			o := opts
			o.Log = log
			err := helper.Serve(context.Background(), hostToGuestR, guestToHostW, finder, o)
			_ = guestToHostW.Close()
			_ = stderrW.Close()
			g.done <- err
		}()
		return helper.Process{
			Stdin:  hostToGuestW,
			Stdout: guestToHostR,
			Stderr: stderrR,
			Wait:   func() error { return <-g.done },
			Kill: func() error {
				_ = hostToGuestW.Close()
				return nil
			},
		}, nil
	}
}

// killGuest ends the current guest as a crash would: its stdout closes.
func (p *pipes) killGuest() {
	p.mu.Lock()
	g := p.guests[len(p.guests)-1]
	p.mu.Unlock()
	_ = g.out.Close()
	_ = g.in.Close()
}

// tunedFinder is the Mastago finder over a fake central with a driver fast
// enough for a test.
type tunedFinder struct {
	*mastago.Finder
}

func (t tunedFinder) Find(ctx context.Context) (estim.Driver, error) {
	d, err := t.Finder.Find(ctx)
	if err != nil {
		return nil, err
	}
	drv := d.(*mastago.Driver)
	drv.Gap, drv.Step, drv.Timeout = 0, 0, 500*time.Millisecond
	drv.Sleep = func(context.Context, time.Duration) error { return nil }
	drv.ArmWindow = 20 * time.Minute
	return drv, nil
}

func mastagoFamily(units ...*fakeunit.Unit) (tunedFinder, *fakeunit.Central) {
	c := &fakeunit.Central{Units: units}
	f := mastago.NewFinder("", nil)
	f.Open = func(context.Context, *slog.Logger) (ble.Central, error) { return c, nil }
	f.ScanWindow = 100 * time.Millisecond
	return tunedFinder{f}, c
}

func TestHostServesAMastagoFamilyThroughAGuest(t *testing.T) {
	ctx := context.Background()
	a := fakeunit.New("id-a", "MASTOGO G-12AB")
	b := fakeunit.New("id-b", "MASTOGO G-34CD")
	finder, _ := mastagoFamily(a, b)
	p := &pipes{}
	var logged bytes.Buffer
	h := helper.New("camlink-unit-test", t.TempDir(), slog.New(slog.NewTextHandler(&logged, nil)))
	h.Start = p.start(finder, helper.Options{Name: "test", Kinds: []estim.Kind{estim.KindMastago}})
	t.Cleanup(func() { _ = h.Close() })

	hello, err := h.Hello(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if hello.Name != "test" || len(hello.Kinds) != 1 || hello.Kinds[0] != estim.KindMastago || h.Name() != "test" {
		t.Fatalf("hello = %+v", hello)
	}
	// The family's units, its description, a selection.
	units, err := h.List(ctx)
	if err != nil || len(units) != 2 || units[0].ID != "id-a" || units[1].Kind != estim.KindMastago {
		t.Fatalf("list = %+v, %v", units, err)
	}
	var out bytes.Buffer
	if err := h.Describe(ctx, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "G-12AB") || !strings.Contains(out.String(), "G-34CD") {
		t.Fatalf("describe = %q", out.String())
	}
	h.Select("G-34CD")
	d, err := h.Find(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind() != estim.KindMastago || d.Label() != "Mastago TENS G-34CD" || d.Port() != "id-b" || d.Capabilities().LevelMax != 25 {
		t.Fatalf("found = %s %s %s %+v", d.Kind(), d.Label(), d.Port(), d.Capabilities())
	}
	if h, ok := d.(estim.HeldReporter); !ok || h.Held() {
		t.Fatal("a unit nobody else holds")
	}
	// The driver's calls reach the unit through the wire.
	if err := d.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.Arm(ctx, estim.PowerModeNormal); err != nil {
		t.Fatal(err)
	}
	if r, ok := d.(estim.ArmRenewer); !ok {
		t.Fatal("the proxy renews the arm")
	} else if err := r.RenewArm(ctx, time.Now().Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	st, err := d.Status(ctx)
	if err != nil || !st.Connected || st.LevelA == nil || *st.LevelA != 0 || st.LoadDetected == nil {
		t.Fatalf("status = %+v, %v", st, err)
	}
	level := 3
	res, err := d.Execute(ctx, estim.Command{Verb: "set_level", Channel: "a", Level: &level}, 15, func() bool { return false })
	if err != nil || res.Level == nil || *res.Level != 3 || b.Level() != 3 {
		t.Fatalf("execute = %+v, %v; unit at %d", res, err, b.Level())
	}
	f, err := d.Telemetry(ctx)
	if err != nil || f.LevelA == nil || *f.LevelA != 3 {
		t.Fatalf("telemetry = %+v, %v", f, err)
	}
	if err := d.Close(ctx, true); err != nil {
		t.Fatal(err)
	}
	if b.Level() != 0 {
		t.Fatalf("close with restore left the unit at %d", b.Level())
	}
	// Closed twice is nothing.
	if err := d.Close(ctx, true); err != nil {
		t.Fatal(err)
	}
	if p.starts.Load() != 1 {
		t.Fatalf("the helper was started %d times", p.starts.Load())
	}
}

func TestHostCarriesTheUnitsLossReasonAcrossTheWire(t *testing.T) {
	ctx := context.Background()
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	finder, _ := mastagoFamily(u)
	p := &pipes{}
	h := helper.New("camlink-unit-test", t.TempDir(), nil)
	h.Start = p.start(finder, helper.Options{Name: "test", Kinds: []estim.Kind{estim.KindMastago}})
	t.Cleanup(func() { _ = h.Close() })
	d, err := h.Find(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The unit switches itself off after sitting idle: the helper's driver
	// says so, and the Host's caller reads the same LossError the in-process
	// driver would have given.
	u.AutoOff(0)
	deadline := time.Now().Add(2 * time.Second)
	var got error
	for time.Now().Before(deadline) {
		_, got = d.Status(ctx)
		if got != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got == nil {
		t.Fatal("the unit's shutdown never reached the host")
	}
	if estim.LossReason(got) != estim.ReasonIdleOff || !strings.Contains(got.Error(), "switched itself off") {
		t.Fatalf("loss over the wire = %v (reason %q)", got, estim.LossReason(got))
	}
	// No unit in reach is estim.ErrNoDevice on this side too.
	finder.Select("nobody-by-this-name")
	h.Select("nobody-by-this-name")
	_, err = h.Find(ctx)
	if !errors.Is(err, estim.ErrNoDevice) {
		t.Fatalf("find with nothing selected = %v", err)
	}
}

func TestHostCancelsARunningExecuteAndSurvivesAHelperThatDies(t *testing.T) {
	ctx := context.Background()
	unit := newFakeDriver("port-1")
	finder := &fakeFinder{unit: unit}
	p := &pipes{}
	h := helper.New("camlink-unit-test", t.TempDir(), nil)
	h.Start = p.start(finder, helper.Options{Name: "fake", Kinds: []estim.Kind{"fakekind"}})
	t.Cleanup(func() { _ = h.Close() })
	d, err := h.Find(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A ramp to 40 at 20 ms a level: the latch trips after ~100 ms, the
	// helper hears `cancel`, the ramp ends where it is.
	var latch atomic.Bool
	go func() { time.Sleep(100 * time.Millisecond); latch.Store(true) }()
	level := 40
	_, err = d.Execute(ctx, estim.Command{Verb: "set_level", Level: &level}, 50, latch.Load)
	if !errors.Is(err, estim.ErrCancelled) {
		t.Fatalf("cancelled execute = %v", err)
	}
	unit.mu.Lock()
	reached := unit.level
	unit.mu.Unlock()
	if reached == 0 || reached >= 40 {
		t.Fatalf("the ramp should have stopped midway, at %d", reached)
	}
	// The level cap the Runtime passes is honoured on the far side.
	level = 45
	res, err := d.Execute(ctx, estim.Command{Verb: "set_level", Level: &level}, 10, func() bool { return false })
	if err != nil || *res.Level != 10 {
		t.Fatalf("capped execute = %+v, %v", res, err)
	}

	// The helper dies mid-call: the caller hears a loss (the unit stopped
	// answering, as far as the connector knows), and the next Find starts
	// the program again.
	statusErr := make(chan error, 1)
	unit.mu.Lock() // a status held up in the driver while the helper dies
	go func() { _, err := d.Status(ctx); statusErr <- err }()
	time.Sleep(30 * time.Millisecond)
	p.killGuest()
	select {
	case err := <-statusErr:
		if estim.LossReason(err) != estim.ReasonStoppedAnswering {
			t.Fatalf("status during the death = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the call outlived the helper")
	}
	unit.mu.Unlock()
	d2, err := h.Find(ctx)
	if err != nil {
		t.Fatalf("find after the death: %v", err)
	}
	if p.starts.Load() != 2 {
		t.Fatalf("the helper was started %d times, want 2", p.starts.Load())
	}
	if _, err := d2.Status(ctx); err != nil {
		t.Fatal(err)
	}
}

// syncBuffer is a bytes.Buffer the Host's relay goroutine may write while the test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestASpawnedHelperProgramServesOverItsStdio(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns the test binary")
	}
	ctx := context.Background()
	var logged syncBuffer
	stateDir := t.TempDir()
	h := helper.New(os.Args[0], stateDir, slog.New(slog.NewTextHandler(&logged, nil)))
	h.Start = func(ctx context.Context, path string, args []string) (helper.Process, error) {
		if len(args) != 2 || args[0] != "--state-dir" || args[1] != stateDir {
			return helper.Process{}, fmt.Errorf("args = %v", args)
		}
		return startTestBinary(ctx, path, stateDir)
	}
	t.Cleanup(func() { _ = h.Close() })

	hello, err := h.Hello(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if hello.Name != "fake" || hello.Protocol != helper.Protocol {
		t.Fatalf("hello = %+v", hello)
	}
	d, err := h.Find(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d.Label() != "Fake unit" || d.Port() != "port-1" {
		t.Fatalf("found %s at %s", d.Label(), d.Port())
	}
	if hr, ok := d.(estim.HeldReporter); !ok || !hr.Held() {
		t.Fatal("held travels")
	}
	level := 5
	res, err := d.Execute(ctx, estim.Command{Verb: "set_level", Level: &level}, 50, func() bool { return false })
	if err != nil || *res.Level != 5 {
		t.Fatalf("execute = %+v, %v", res, err)
	}
	units, err := h.List(ctx)
	if err != nil || len(units) != 1 || !units[0].Held {
		t.Fatalf("list = %+v, %v", units, err)
	}
	if err := d.Close(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logged.String(), "guest starting") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// Relayed line by line, tagged with the helper (its path until it has
	// said its name, its name after).
	if !strings.Contains(logged.String(), "guest starting") || !strings.Contains(logged.String(), "helper=") {
		t.Fatalf("the helper's stderr did not reach the log:\n%s", logged.String())
	}
}

func TestNoDeviceCrossesTheWireOnceWorded(t *testing.T) {
	ctx := context.Background()
	finder := &fakeFinder{unit: newFakeDriver("port-1"), fail: fmt.Errorf("%w: nothing on any port", estim.ErrNoDevice)}
	p := &pipes{}
	h := helper.New("camlink-unit-test", t.TempDir(), nil)
	h.Start = p.start(finder, helper.Options{Name: "fake", Kinds: []estim.Kind{"fakekind"}})
	t.Cleanup(func() { _ = h.Close() })
	_, err := h.Find(ctx)
	if !errors.Is(err, estim.ErrNoDevice) {
		t.Fatalf("find = %v", err)
	}
	if got, want := err.Error(), estim.ErrNoDevice.Error()+": nothing on any port"; got != want {
		t.Fatalf("worded %q, want %q", got, want)
	}
	// The bare error, no detail.
	finder.fail = estim.ErrNoDevice
	_, err = h.Find(ctx)
	if !errors.Is(err, estim.ErrNoDevice) || err.Error() != estim.ErrNoDevice.Error() {
		t.Fatalf("bare find = %v", err)
	}
}

// oneDriver is a family whose Find always hands out the one driver given.
type oneDriver struct{ d estim.Driver }

func (o oneDriver) Find(context.Context) (estim.Driver, error) { return o.d, nil }
func (o oneDriver) Describe(_ context.Context, w io.Writer) error {
	_, err := io.WriteString(w, "one\n")
	return err
}

// blockingClose is a driver whose Close waits until released, recording
// what it was asked and whether its context was cancelled while it waited.
type blockingClose struct {
	*fakeDriver
	proceed   chan struct{}
	entered   chan struct{}
	restore   atomic.Bool
	cancelled atomic.Bool
}

func (d *blockingClose) Close(ctx context.Context, restore bool) error {
	d.restore.Store(restore)
	close(d.entered)
	select {
	case <-d.proceed:
	case <-ctx.Done():
		d.cancelled.Store(true)
		return ctx.Err()
	}
	if ctx.Err() != nil {
		d.cancelled.Store(true)
	}
	return d.fakeDriver.Close(ctx, restore)
}

// When stdin ends with a close already running, Serve lets it finish
// (restore and all) instead of cancelling it under the driver: the dead
// end that abandoned a device still keyed when the connector quit during
// a health tick (the plan's connector-exit-grace).
func TestServeLetsAnInFlightCloseFinishOnEOF(t *testing.T) {
	drv := &blockingClose{fakeDriver: newFakeDriver("port-1"), proceed: make(chan struct{}), entered: make(chan struct{})}
	hostToGuestR, hostToGuestW := io.Pipe()
	guestToHostR, guestToHostW := io.Pipe()
	served := make(chan error, 1)
	go func() {
		served <- helper.Serve(context.Background(), hostToGuestR, guestToHostW, oneDriver{drv}, helper.Options{Name: "fake", Kinds: []estim.Kind{"fakekind"}})
	}()
	replies := bufio.NewScanner(guestToHostR)
	readReply := func(what string) map[string]any {
		if !replies.Scan() {
			t.Fatalf("no reply for %s: %v", what, replies.Err())
		}
		var m map[string]any
		if err := json.Unmarshal(replies.Bytes(), &m); err != nil {
			t.Fatalf("reply for %s: %v", what, err)
		}
		return m
	}
	write := func(v any) {
		b, _ := json.Marshal(v)
		if _, err := hostToGuestW.Write(append(b, '\n')); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	// find, and wait for its reply, so the driver is open before the close.
	write(map[string]any{"id": 1, "method": helper.MethodFind})
	if r := readReply("find"); r["result"] == nil {
		t.Fatalf("find failed: %v", r["error"])
	}
	// close(restore=true), then end stdin under it, mid-close.
	write(map[string]any{"id": 2, "method": helper.MethodClose, "params": map[string]any{"restore": true}})
	select {
	case <-drv.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the close was never dispatched")
	}
	_ = hostToGuestW.Close() // stdin EOF while the close runs

	// Serve must wait for the in-flight close, not return and cancel it.
	select {
	case <-served:
		t.Fatal("Serve returned before the in-flight close finished")
	case <-time.After(100 * time.Millisecond):
	}
	if drv.cancelled.Load() {
		t.Fatal("the in-flight close was cancelled by the stdin EOF")
	}

	close(drv.proceed) // let the close finish
	if r := readReply("close"); r["error"] != nil {
		t.Fatalf("close reply carried an error: %v", r["error"])
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after the close finished")
	}
	if !drv.restore.Load() {
		t.Fatal("the close did not carry restore=true")
	}
	if drv.cancelled.Load() {
		t.Fatal("the close saw its context cancelled")
	}
	if !drv.closed {
		t.Fatal("the device was not closed")
	}
}
