package estim_test

import (
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

// unitRuntime is a Runtime over one fake unit, with the driver's timings
// shortened.
func unitRuntime(t *testing.T, u *fakeunit.Unit) *estim.Runtime {
	t.Helper()
	rt := &estim.Runtime{
		Connect: func(ctx context.Context) (estim.Driver, error) {
			conn, err := u.Connect()
			if err != nil {
				return nil, err
			}
			drv, err := mastago.Connect(ctx, conn, nil)
			if err != nil {
				return nil, err
			}
			drv.Gap, drv.Step, drv.Timeout = 0, 0, 500*time.Millisecond
			drv.Sleep = func(context.Context, time.Duration) error { return nil }
			drv.ArmWindow = 20 * time.Minute
			return drv, nil
		},
	}
	rt.ArmWindow = 20 * time.Minute
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	return rt
}

func TestRuntimeUnitOpenReleasesAndDescribes(t *testing.T) {
	ctx := context.Background()
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	u.PushLevel(7)
	rt := unitRuntime(t, u)
	if err := rt.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if u.Level() != 0 || u.Outputting() {
		t.Fatalf("a fresh connection must be released: level=%d outputting=%v", u.Level(), u.Outputting())
	}
	d := rt.Descriptor()
	if d.Kind != estim.KindMastago || d.Label != "Mastago TENS G-12AB" || !d.Connected || d.Capabilities.LevelMax != 25 {
		t.Fatalf("descriptor: %+v", d)
	}
	if s := rt.Settings(); s.PowerMode != estim.PowerModeNormal || s.LevelMax != 15 {
		t.Fatalf("default settings for the unit: %+v", s)
	}
	st := rt.LastStatus()
	if *st.LevelA != 0 || *st.LevelB != 0 || *st.ADCOverride || *st.Outputting || st.TimerRemainingS == nil {
		t.Fatalf("status: %+v", st)
	}
}

func TestRuntimeUnitArmSetsCountdownAndRenews(t *testing.T) {
	ctx := context.Background()
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	rt := unitRuntime(t, u)
	now := time.Now()
	rt.Now = func() time.Time { return now }
	if err := rt.Open(ctx); err != nil {
		t.Fatal(err)
	}
	u.ResetCommands()
	if err := rt.Arm(ctx); err != nil {
		t.Fatal(err)
	}
	if !rt.Armed() || u.TimerS() != 1200 {
		t.Fatalf("armed=%v timer=%d", rt.Armed(), u.TimerS())
	}
	if _, _, err := rt.Execute(ctx, estim.Command{Verb: "set_level", Level: estim.Int(3)}); err != nil {
		t.Fatal(err)
	}
	// A renewal well within RenewSlack writes nothing.
	u.ResetCommands()
	now = now.Add(10 * time.Second)
	if !rt.ExtendArm() {
		t.Fatal("extend failed")
	}
	rt.RenewDeviceArm(ctx)
	if cmds := u.Commands(); len(cmds) != 0 {
		t.Fatalf("renewed too soon: %v", cmds)
	}
	// Past it, the countdown is written again without touching output.
	now = now.Add(2 * time.Minute)
	rt.ExtendArm()
	rt.RenewDeviceArm(ctx)
	cmds := u.Commands()
	if len(cmds) != 1 || !strings.HasPrefix(cmds[0], "AT+CDCLK=") {
		t.Fatalf("renewal sent %v", cmds)
	}
	if u.Level() != 3 || !u.Outputting() {
		t.Fatal("renewal touched the output")
	}
	// A second tick with nothing new to renew writes nothing.
	u.ResetCommands()
	rt.RenewDeviceArm(ctx)
	if cmds := u.Commands(); len(cmds) != 0 {
		t.Fatalf("renewed again: %v", cmds)
	}
}

func TestRuntimeUnitReleasePausesAndZeroes(t *testing.T) {
	ctx := context.Background()
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	rt := unitRuntime(t, u)
	if err := rt.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if err := rt.Arm(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := rt.Execute(ctx, estim.Command{Verb: "set_level", Level: estim.Int(5)}); err != nil {
		t.Fatal(err)
	}
	st, err := rt.Release(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	if u.Outputting() || u.Level() != 0 || *st.LevelA != 0 || *st.Outputting || rt.Armed() {
		t.Fatalf("after release: unit outputting=%v level=%d status=%+v armed=%v", u.Outputting(), u.Level(), st, rt.Armed())
	}
	if _, _, err := rt.Execute(ctx, estim.Command{Verb: "set_level", Level: estim.Int(2)}); !errors.Is(err, estim.ErrNotArmed) {
		t.Fatalf("actuation after release: %v", err)
	}
}

func TestRuntimeUnitCapsAndSettings(t *testing.T) {
	ctx := context.Background()
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	rt := unitRuntime(t, u)
	if err := rt.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if err := rt.Arm(ctx); err != nil {
		t.Fatal(err)
	}
	// The default cap for the unit is 15 of 25.
	if _, _, err := rt.Execute(ctx, estim.Command{Verb: "set_level", Level: estim.Int(16)}); err == nil {
		t.Fatal("a level past the unit's default cap was accepted")
	}
	if _, err := rt.SetSettings(ctx, estim.Settings{PowerMode: estim.PowerModeNormal, LevelMax: 20}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := rt.Execute(ctx, estim.Command{Verb: "set_level", Level: estim.Int(16)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := rt.Execute(ctx, estim.Command{Verb: "set_mode", Mode: estim.Int(31)}); err != nil {
		t.Fatal(err)
	}
	if u.Mode() != 31 || u.Level() != 16 || !u.Outputting() {
		t.Fatalf("after set_mode: mode=%d level=%d outputting=%v", u.Mode(), u.Level(), u.Outputting())
	}
	if _, _, err := rt.Execute(ctx, estim.Command{Verb: "set_mode", Mode: estim.Int(32)}); err == nil {
		t.Fatal("a program outside the unit's range was accepted")
	}
	if _, _, err := rt.Execute(ctx, estim.Command{Verb: "set_ma", Percent: estim.Int(50)}); err == nil || !strings.Contains(err.Error(), "tempo") {
		t.Fatalf("tempo on a unit without one: %v", err)
	}
}

func TestRuntimeUnitNoContactFailsTheCommandAndReleases(t *testing.T) {
	ctx := context.Background()
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	rt := unitRuntime(t, u)
	if err := rt.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if err := rt.Arm(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := rt.Execute(ctx, estim.Command{Verb: "set_level", Level: estim.Int(3)}); err != nil {
		t.Fatal(err)
	}
	u.SetLoad(false)
	res, _, err := rt.Execute(ctx, estim.Command{Verb: "set_level", Level: estim.Int(8)})
	if !mastago.IsNoLoad(err) {
		t.Fatalf("err = %v", err)
	}
	if *res.Level != 3 {
		t.Fatalf("result: %+v", res)
	}
	// A refusal is not a fault: the link stays up and the session's usual
	// release after a failed command runs through the Runtime.
	if !rt.Connected() {
		t.Fatal("a refused intensity must not fault the device")
	}
	st, err := rt.Release(ctx, "command_failed")
	if err != nil {
		t.Fatalf("release without contact: %v", err)
	}
	if u.Outputting() || *st.LoadDetected || *st.Outputting {
		t.Fatalf("after release: unit outputting=%v status=%+v", u.Outputting(), st)
	}
}

func TestRuntimeUnitAutoOffFaults(t *testing.T) {
	ctx := context.Background()
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	rt := unitRuntime(t, u)
	if err := rt.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if err := rt.Arm(ctx); err != nil {
		t.Fatal(err)
	}
	u.AutoOff(0)
	if rt.HealthCheck(ctx) {
		t.Fatal("health check passed on a unit that switched off")
	}
	if rt.Connected() || rt.Armed() || !rt.CancelLatched() {
		t.Fatal("fault did not fail closed")
	}
	if e := rt.LastError(); !strings.Contains(e, "switched itself off") {
		t.Fatalf("last error = %q", e)
	}
	st := rt.LastStatus()
	if st.Connected || st.LevelA != nil || st.Error == nil {
		t.Fatalf("status after fault: %+v", st)
	}
	// Once the unit is back on, the next Open finds it.
	u.PowerOn()
	if err := rt.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if !rt.Connected() {
		t.Fatal("not reconnected")
	}
}

func TestRuntimeUnitDeviceSideChangeShowsInTelemetry(t *testing.T) {
	ctx := context.Background()
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	rt := unitRuntime(t, u)
	if err := rt.Open(ctx); err != nil {
		t.Fatal(err)
	}
	u.PushLevel(4)
	f := rt.Sample(ctx, time.Now())
	if f.Skipped || f.LevelA == nil || *f.LevelA != 4 {
		t.Fatalf("frame: %+v", f)
	}
	if !rt.HealthCheck(ctx) {
		t.Fatal("health check failed")
	}
	if st := rt.LastStatus(); *st.LevelA != 4 {
		t.Fatalf("status: %+v", st)
	}
}

// twoUnitRuntime is a Runtime over the real Mastago finder and a fake
// central with two units, so selection can be tried end to end.
func twoUnitRuntime(t *testing.T) (*estim.Runtime, *fakeunit.Unit, *fakeunit.Unit, *mastago.Finder, *[]estim.Descriptor, *[][]estim.Unit) {
	t.Helper()
	a := fakeunit.New("id-a", "MASTOGO G-12AB")
	b := fakeunit.New("id-b", "MASTOGO G-34CD")
	c := &fakeunit.Central{Units: []*fakeunit.Unit{a, b}}
	f := mastago.NewFinder("", nil)
	f.Open = func(context.Context, *slog.Logger) (ble.Central, error) { return c, nil }
	f.ScanWindow = 100 * time.Millisecond
	fs := estim.Finders{f}
	var reports []estim.Descriptor
	var lists [][]estim.Unit
	rt := &estim.Runtime{
		Connect: func(ctx context.Context) (estim.Driver, error) {
			d, err := fs.Find(ctx)
			if err != nil {
				return nil, err
			}
			drv := d.(*mastago.Driver)
			drv.Gap, drv.Step, drv.Timeout = 0, 0, 500*time.Millisecond
			drv.Sleep = func(context.Context, time.Duration) error { return nil }
			return drv, nil
		},
		List:     fs.List,
		Select:   fs.Select,
		OnDevice: func(_ context.Context, d estim.Descriptor) { reports = append(reports, d) },
		OnUnits:  func(_ context.Context, us []estim.Unit) { lists = append(lists, us) },
	}
	rt.ArmWindow = 20 * time.Minute
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	return rt, a, b, f, &reports, &lists
}

func TestRuntimeListsAndSelectsAmongUnits(t *testing.T) {
	ctx := context.Background()
	rt, a, b, f, reports, lists := twoUnitRuntime(t)
	if err := rt.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if d := rt.Descriptor(); d.ID != "id-a" || !d.Connected || d.Held {
		t.Fatalf("the first unit found is served, with its id: %+v", d)
	}
	// Listing names both, connects to neither, and tells OnUnits once per
	// change.
	units, err := rt.ListUnits(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 2 || units[0].ID != "id-a" || units[1].ID != "id-b" || units[0].Held || units[1].Held {
		t.Fatalf("units = %+v", units)
	}
	if b.Connections() != 0 {
		t.Fatal("listing must not connect to the unit not served")
	}
	if _, err := rt.ListUnits(ctx); err != nil {
		t.Fatal(err)
	}
	if len(*lists) != 1 {
		t.Fatalf("OnUnits told %d times; the same list is not news", len(*lists))
	}
	if got := rt.Units(); len(got) != 2 {
		t.Fatalf("Units() = %+v", got)
	}

	// Selecting the other unit lets the held one go (released to zero,
	// disconnected) and opens the selected one.
	a.PushLevel(3)
	if err := rt.SelectUnit(ctx, "id-b"); err != nil {
		t.Fatal(err)
	}
	if d := rt.Descriptor(); d.ID != "id-b" || d.Label != "Mastago TENS G-34CD" || !d.Connected {
		t.Fatalf("after selecting id-b: %+v", d)
	}
	if a.Connections() != 0 || a.Level() != 0 || a.Outputting() {
		t.Fatalf("the unit let go must be released and disconnected: conns=%d level=%d out=%v", a.Connections(), a.Level(), a.Outputting())
	}
	if f.Pin != "id-b" {
		t.Fatalf("the finder is pinned to the selection: %q", f.Pin)
	}
	n := len(*reports)
	if n < 3 || (*reports)[n-2].Connected || (*reports)[n-2].ID != "id-a" || !(*reports)[n-1].Connected || (*reports)[n-1].ID != "id-b" {
		t.Fatalf("reports = %+v; a switch is the unit let go, then the one selected", *reports)
	}

	// Selecting the unit already served keeps it.
	before := b.Connections()
	if err := rt.SelectUnit(ctx, "G-34CD"); err != nil {
		t.Fatal(err)
	}
	if b.Connections() != before || rt.Descriptor().ID != "id-b" {
		t.Fatal("selecting the unit served must not reconnect it")
	}

	// Refused while armed: the phone stops the unit first.
	if err := rt.Arm(ctx); err != nil {
		t.Fatal(err)
	}
	if err := rt.SelectUnit(ctx, "id-a"); !errors.Is(err, estim.ErrArmed) {
		t.Fatalf("selecting while armed: %v", err)
	}
	if rt.Descriptor().ID != "id-b" || !rt.Armed() {
		t.Fatal("a refused selection changes nothing")
	}
	if _, err := rt.Release(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	// Lifting the selection keeps what is held.
	if err := rt.SelectUnit(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if rt.Descriptor().ID != "id-b" || f.Pin != "" {
		t.Fatalf("lifting the selection: id=%s pin=%q", rt.Descriptor().ID, f.Pin)
	}
	// A selection the finders cannot satisfy: the unit is let go and
	// nothing is served until it answers.
	if err := rt.SelectUnit(ctx, "G-99ZZ"); !errors.Is(err, estim.ErrNoDevice) {
		t.Fatalf("selecting an absent unit: %v", err)
	}
	if rt.Connected() {
		t.Fatal("the unit not selected is not served")
	}
}
