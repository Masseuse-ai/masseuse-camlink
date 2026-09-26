package estim_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
