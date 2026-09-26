package mastago_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/ble"
	"github.com/FemLed/masseuse-camlink/internal/estim"
	"github.com/FemLed/masseuse-camlink/internal/estim/mastago"
	"github.com/FemLed/masseuse-camlink/internal/estim/mastago/fakeunit"
)

func finderOver(c *fakeunit.Central, pin string) *mastago.Finder {
	f := mastago.NewFinder(pin, nil)
	f.Open = func(context.Context, *slog.Logger) (ble.Central, error) { return c, nil }
	f.ScanWindow = 200 * time.Millisecond
	return f
}

func TestFinderPrefersHeldUnits(t *testing.T) {
	ctx := context.Background()
	held := fakeunit.New("held-1", "MASTOGO G-12AB")
	adv := fakeunit.New("adv-1", "MASTOGO G-34CD")
	c := &fakeunit.Central{Units: []*fakeunit.Unit{adv}, Held: []*fakeunit.Unit{held}}
	f := finderOver(c, "")
	drv, err := f.Find(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer drv.Close(ctx, false)
	if drv.Port() != "held-1" || drv.Label() != "Mastago TENS G-12AB" {
		t.Fatalf("found %s %s", drv.Port(), drv.Label())
	}
	if c.Scans() != 0 {
		t.Fatal("scanned although a held unit answered")
	}
}

func TestFinderScansWhenNothingIsHeld(t *testing.T) {
	ctx := context.Background()
	adv := fakeunit.New("adv-1", "MASTOGO G-34CD")
	c := &fakeunit.Central{Units: []*fakeunit.Unit{adv}}
	drv, err := finderOver(c, "").Find(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer drv.Close(ctx, false)
	if drv.Port() != "adv-1" {
		t.Fatalf("found %s", drv.Port())
	}
}

func TestFinderSkipsAHeldUnitThatIsOff(t *testing.T) {
	ctx := context.Background()
	held := fakeunit.New("held-1", "MASTOGO G-12AB")
	adv := fakeunit.New("adv-1", "MASTOGO G-34CD")
	c := &fakeunit.Central{Units: []*fakeunit.Unit{adv}, Held: []*fakeunit.Unit{held}}
	held.AutoOff(3)
	drv, err := finderOver(c, "").Find(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer drv.Close(ctx, false)
	if drv.Port() != "adv-1" {
		t.Fatalf("found %s", drv.Port())
	}
}

func TestFinderReportsNoDevice(t *testing.T) {
	ctx := context.Background()
	c := &fakeunit.Central{}
	_, err := finderOver(c, "").Find(ctx)
	if !errors.Is(err, estim.ErrNoDevice) {
		t.Fatalf("err = %v", err)
	}
	other := fakeunit.New("x", "SomeHeadphones")
	other.NoService = true
	c = &fakeunit.Central{Units: []*fakeunit.Unit{other}}
	f := finderOver(c, "")
	_, err = f.Find(ctx)
	if !errors.Is(err, estim.ErrNoDevice) {
		t.Fatalf("a non-unit was accepted: %v", err)
	}
}

func TestFinderPin(t *testing.T) {
	ctx := context.Background()
	a := fakeunit.New("id-a", "MASTOGO G-12AB")
	b := fakeunit.New("id-b", "MASTOGO G-34CD")
	for _, pin := range []string{"G-34CD", "MASTOGO G-34CD", "id-b", "g-34cd"} {
		c := &fakeunit.Central{Units: []*fakeunit.Unit{a, b}}
		drv, err := finderOver(c, pin).Find(ctx)
		if err != nil {
			t.Fatalf("pin %q: %v", pin, err)
		}
		if drv.Port() != "id-b" {
			t.Fatalf("pin %q found %s", pin, drv.Port())
		}
		_ = drv.Close(ctx, false)
	}
	c := &fakeunit.Central{Units: []*fakeunit.Unit{a}}
	if _, err := finderOver(c, "G-34CD").Find(ctx); !errors.Is(err, estim.ErrNoDevice) || !strings.Contains(err.Error(), "G-34CD") {
		t.Fatalf("pinned to an absent unit: %v", err)
	}
}

func TestFinderBluetoothStates(t *testing.T) {
	ctx := context.Background()
	f := mastago.NewFinder("", nil)
	f.Open = func(context.Context, *slog.Logger) (ble.Central, error) { return nil, ble.ErrUnsupported }
	if _, err := f.Find(ctx); !errors.Is(err, estim.ErrNoDevice) {
		t.Fatalf("no Bluetooth: %v", err)
	}
	f = mastago.NewFinder("", nil)
	f.Open = func(context.Context, *slog.Logger) (ble.Central, error) { return nil, ble.ErrUnavailable }
	if _, err := f.Find(ctx); !errors.Is(err, ble.ErrUnavailable) || errors.Is(err, estim.ErrNoDevice) {
		t.Fatalf("Bluetooth off: %v", err)
	}
}

func TestFinderDescribe(t *testing.T) {
	ctx := context.Background()
	held := fakeunit.New("held-1", "MASTOGO G-12AB")
	adv := fakeunit.New("adv-1", "MASTOGO G-34CD")
	c := &fakeunit.Central{Units: []*fakeunit.Unit{adv}, Held: []*fakeunit.Unit{held}}
	var out bytes.Buffer
	if err := finderOver(c, "G-34CD").Describe(ctx, &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"held-1", "held by another program", "skipped: not the pinned unit", "adv-1", "advertising", "will be tried", `pinned to "G-34CD"`} {
		if !strings.Contains(text, want) {
			t.Errorf("describe lacks %q:\n%s", want, text)
		}
	}
	out.Reset()
	if err := finderOver(&fakeunit.Central{}, "").Describe(ctx, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no unit is held or advertising") {
		t.Errorf("empty describe:\n%s", out.String())
	}
}
