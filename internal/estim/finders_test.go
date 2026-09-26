package estim_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/FemLed/masseuse-camlink/internal/estim"
)

// stubFinder answers Find with a fixed result and Describe with its name.
type stubFinder struct {
	name   string
	drv    estim.Driver
	err    error
	calls  int
	closed bool
}

func (f *stubFinder) Find(context.Context) (estim.Driver, error) {
	f.calls++
	return f.drv, f.err
}

func (f *stubFinder) Describe(_ context.Context, out io.Writer) error {
	fmt.Fprintf(out, "%s\n", f.name)
	return f.err
}

func (f *stubFinder) Close() error { f.closed = true; return nil }

// stubDriver is a Driver that only needs to be told apart from another.
type stubDriver struct {
	estim.Driver
	label string
}

func (d stubDriver) Label() string { return d.label }

func TestFindersFirstFoundWins(t *testing.T) {
	ctx := context.Background()
	a := &stubFinder{name: "a", err: fmt.Errorf("%w: none", estim.ErrNoDevice)}
	b := &stubFinder{name: "b", drv: stubDriver{label: "B"}}
	c := &stubFinder{name: "c", drv: stubDriver{label: "C"}}
	fs := estim.Finders{a, b, c}
	d, err := fs.Find(ctx)
	if err != nil || d.Label() != "B" {
		t.Fatalf("Find = %v, %v", d, err)
	}
	if a.calls != 1 || b.calls != 1 || c.calls != 0 {
		t.Fatalf("calls a=%d b=%d c=%d; the search must stop at the first device", a.calls, b.calls, c.calls)
	}
}

func TestFindersPreferTheReasonADeviceIsUnusable(t *testing.T) {
	ctx := context.Background()
	unusable := errors.New("held by another program")
	fs := estim.Finders{
		&stubFinder{name: "a", err: fmt.Errorf("%w: none", estim.ErrNoDevice)},
		&stubFinder{name: "b", err: unusable},
		&stubFinder{name: "c", err: fmt.Errorf("%w: none either", estim.ErrNoDevice)},
	}
	if _, err := fs.Find(ctx); !errors.Is(err, unusable) {
		t.Fatalf("err = %v, want the unusable device's", err)
	}
	// Nothing anywhere is ErrNoDevice.
	fs = estim.Finders{
		&stubFinder{name: "a", err: fmt.Errorf("%w: none", estim.ErrNoDevice)},
		&stubFinder{name: "c", err: fmt.Errorf("%w: none either", estim.ErrNoDevice)},
	}
	if _, err := fs.Find(ctx); !errors.Is(err, estim.ErrNoDevice) {
		t.Fatalf("err = %v", err)
	}
	// No finder at all is ErrNoDevice too.
	var none estim.Finders
	if _, err := none.Find(ctx); !errors.Is(err, estim.ErrNoDevice) {
		t.Fatalf("empty: err = %v", err)
	}
}

func TestFindersStopWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b := &stubFinder{name: "b", drv: stubDriver{label: "B"}}
	fs := estim.Finders{&stubFinder{name: "a", err: errors.New("slow")}, b}
	if _, err := fs.Find(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if b.calls != 0 {
		t.Fatal("kept searching after cancellation")
	}
}

func TestFindersDescribeAndClose(t *testing.T) {
	ctx := context.Background()
	failing := errors.New("bluetooth is off")
	a := &stubFinder{name: "family a", err: failing}
	b := &stubFinder{name: "family b"}
	fs := estim.Finders{a, b}
	var out bytes.Buffer
	if err := fs.Describe(ctx, &out); !errors.Is(err, failing) {
		t.Fatalf("Describe err = %v", err)
	}
	if out.String() != "family a\n\nfamily b\n" {
		t.Fatalf("Describe wrote %q", out.String())
	}
	if err := fs.Close(); err != nil {
		t.Fatal(err)
	}
	if !a.closed || !b.closed {
		t.Fatal("Close did not reach every finder")
	}
}

// listingFinder is a stubFinder that can also list and be restricted.
type listingFinder struct {
	stubFinder
	units    []estim.Unit
	listErr  error
	selected string
}

func (f *listingFinder) List(context.Context) ([]estim.Unit, error) { return f.units, f.listErr }
func (f *listingFinder) Select(unit string)                         { f.selected = unit }

func TestFindersListAndSelectAcrossFamilies(t *testing.T) {
	ctx := context.Background()
	ble := &listingFinder{stubFinder: stubFinder{name: "ble"}, units: []estim.Unit{
		{ID: "id-a", Kind: estim.KindMastago, Label: "Mastago TENS G-12AB", Held: true},
		{ID: "id-b", Kind: estim.KindMastago, Label: "Mastago TENS G-34CD"},
	}}
	plain := &stubFinder{name: "plain"}
	serial := &listingFinder{stubFinder: stubFinder{name: "serial"}, listErr: errors.New("no serial ports"),
		units: []estim.Unit{{ID: "/dev/cu.usbserial-1", Kind: estim.KindEstim2B, Label: "E-Stim Systems 2B"}}}
	fs := estim.Finders{ble, plain, serial}
	units, err := fs.List(ctx)
	if err == nil || err.Error() != "no serial ports" {
		t.Fatalf("the first failure is reported after listing every family: %v", err)
	}
	if len(units) != 3 || units[0].ID != "id-a" || units[1].ID != "id-b" || units[2].ID != "/dev/cu.usbserial-1" {
		t.Fatalf("units = %+v", units)
	}
	// One selection is given to every family that can take one.
	fs.Select("id-b")
	if ble.selected != "id-b" || serial.selected != "id-b" {
		t.Fatalf("selected ble=%q serial=%q", ble.selected, serial.selected)
	}
	// A list of finders that cannot list is empty, not an error.
	only := estim.Finders{plain}
	if units, err := only.List(ctx); err != nil || len(units) != 0 {
		t.Fatalf("no lister: %v, %v", units, err)
	}
}
