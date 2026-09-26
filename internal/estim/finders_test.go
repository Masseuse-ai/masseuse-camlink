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
