package mastago_test

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

// fast shortens a driver's timings for tests: no inter-command gap or
// ramp pauses, a short reply timeout.
func fast(d *mastago.Driver) {
	d.Gap = 0
	d.Step = 0
	d.Timeout = 500 * time.Millisecond
	d.Sleep = func(context.Context, time.Duration) error { return nil }
}

func connect(t *testing.T, u *fakeunit.Unit) *mastago.Driver {
	t.Helper()
	conn, err := u.Connect()
	if err != nil {
		t.Fatal(err)
	}
	drv, err := mastago.Connect(context.Background(), conn, nil)
	if err != nil {
		t.Fatal(err)
	}
	fast(drv)
	t.Cleanup(func() { _ = drv.Close(context.Background(), false) })
	return drv
}

func TestConnectVerifiesTheUnitAnswers(t *testing.T) {
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	drv := connect(t, u)
	if drv.Kind() != estim.KindMastago || drv.Label() != "Mastago TENS G-12AB" || drv.Port() != "id-1" {
		t.Fatalf("identity: %q %q %q", drv.Kind(), drv.Label(), drv.Port())
	}
	if cmds := u.Commands(); len(cmds) != 1 || cmds[0] != "AT+CMODE?" {
		t.Fatalf("connect sent %v", cmds)
	}
	caps := drv.Capabilities()
	if caps.LevelMax != 25 || len(caps.Modes) != 32 || caps.Tempo || len(caps.PowerModes) != 0 || !caps.Timer || !caps.LoadDetect || caps.LevelMaxDefault != 15 {
		t.Fatalf("capabilities: %+v", caps)
	}
}

func TestConnectRefusesASilentPeripheral(t *testing.T) {
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	u.Mute = map[string]bool{"CMODE": true}
	conn, _ := u.Connect()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	drv, err := mastago.Connect(ctx, conn, nil)
	if err == nil {
		_ = drv.Close(ctx, false)
		t.Fatal("a silent peripheral was accepted")
	}
	if u.Connections() != 0 {
		t.Fatal("the link was left open")
	}
}

func TestStatusAndTelemetry(t *testing.T) {
	ctx := context.Background()
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	u.SetVolts(3.6)
	drv := connect(t, u)
	st, err := drv.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Connected || *st.Port != "id-1" || *st.Mode != 0 || *st.LevelA != 0 || *st.LevelB != 0 || *st.Power != "normal" {
		t.Fatalf("status: %+v", st)
	}
	if *st.ADCOverride || *st.MAPotOverride || st.LevelMA != nil || st.RoutineState != nil {
		t.Fatalf("device-neutral fields: %+v", st)
	}
	if *st.Outputting || !*st.LoadDetected || *st.BatteryPercent != 50 || *st.TimerRemainingS < 3590 || *st.TimerRemainingS > 3600 {
		t.Fatalf("unit fields: outputting=%v load=%v battery=%d timer=%d", *st.Outputting, *st.LoadDetected, *st.BatteryPercent, *st.TimerRemainingS)
	}
	f, err := drv.Telemetry(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if *f.LevelA != 0 || *f.Outputting || *f.Mode != 0 || !*f.LoadDetected || f.TimerRemainingS == nil {
		t.Fatalf("frame: %+v", f)
	}
}

func TestRampUpReadsBackAndStartsOutput(t *testing.T) {
	ctx := context.Background()
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	drv := connect(t, u)
	u.ResetCommands()
	applied, err := drv.Ramp(ctx, 3, nil)
	if err != nil || applied != 3 {
		t.Fatalf("ramp = %d, %v", applied, err)
	}
	if !u.Outputting() || u.Level() != 3 {
		t.Fatalf("unit: outputting=%v level=%d", u.Outputting(), u.Level())
	}
	cmds := strings.Join(u.Commands(), " ")
	want := "AT+CSTR? AT+QPOWP? AT+QPOWS AT+CSTR=0,1 AT+CSTR? AT+CSTR=0,2 AT+CSTR? AT+CSTR=0,3 AT+CSTR?"
	if cmds != want {
		t.Fatalf("ramp sent\n%s\nwant\n%s", cmds, want)
	}
}

func TestRampDownIsOneWriteAndZeroPauses(t *testing.T) {
	ctx := context.Background()
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	drv := connect(t, u)
	if _, err := drv.Ramp(ctx, 5, nil); err != nil {
		t.Fatal(err)
	}
	u.ResetCommands()
	if applied, err := drv.Ramp(ctx, 2, nil); err != nil || applied != 2 {
		t.Fatalf("ramp down = %d, %v", applied, err)
	}
	if cmds := strings.Join(u.Commands(), " "); cmds != "AT+CSTR? AT+CSTR=0,2 AT+CSTR? AT+QPOWP?" {
		t.Fatalf("ramp down sent %s", cmds)
	}
	if !u.Outputting() {
		t.Fatal("lowering the intensity must not stop output")
	}
	u.ResetCommands()
	if applied, err := drv.Ramp(ctx, 0, nil); err != nil || applied != 0 {
		t.Fatalf("ramp to zero = %d, %v", applied, err)
	}
	if u.Outputting() || u.Level() != 0 {
		t.Fatalf("unit after zero: outputting=%v level=%d", u.Outputting(), u.Level())
	}
	if cmds := strings.Join(u.Commands(), " "); cmds != "AT+CSTR? AT+QPOWP AT+CSTR=0,0" {
		t.Fatalf("zero sent %s", cmds)
	}
}

func TestRampStopsWithoutElectrodeContact(t *testing.T) {
	ctx := context.Background()
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	drv := connect(t, u)
	if _, err := drv.Ramp(ctx, 2, nil); err != nil {
		t.Fatal(err)
	}
	u.SetLoad(false)
	applied, err := drv.Ramp(ctx, 6, nil)
	if !mastago.IsNoLoad(err) {
		t.Fatalf("ramp without contact = %d, %v", applied, err)
	}
	if applied != 2 || u.Level() != 2 {
		t.Fatalf("intensity moved: applied=%d unit=%d", applied, u.Level())
	}
	// The refusal is folded into the state.
	if st := drv.State(); st.LoadDetected == nil || *st.LoadDetected {
		t.Fatalf("state after refusal: %+v", st)
	}
}

func TestRampIsPreemptedByCancel(t *testing.T) {
	ctx := context.Background()
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	drv := connect(t, u)
	steps := 0
	cancelled := func() bool { steps++; return steps > 2 }
	applied, err := drv.Ramp(ctx, 10, cancelled)
	if !errors.Is(err, estim.ErrCancelled) {
		t.Fatalf("err = %v", err)
	}
	if applied != 1 || u.Level() != 1 {
		t.Fatalf("applied %d, unit %d", applied, u.Level())
	}
}

func TestReleasePausesAndZeroes(t *testing.T) {
	ctx := context.Background()
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	drv := connect(t, u)
	if _, err := drv.Ramp(ctx, 4, nil); err != nil {
		t.Fatal(err)
	}
	if err := drv.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if u.Outputting() || u.Level() != 0 {
		t.Fatalf("after release: outputting=%v level=%d", u.Outputting(), u.Level())
	}
}

func TestReleaseWithoutContactAcceptsAPausedUnit(t *testing.T) {
	ctx := context.Background()
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	drv := connect(t, u)
	if _, err := drv.Ramp(ctx, 4, nil); err != nil {
		t.Fatal(err)
	}
	u.SetLoad(false)
	if err := drv.Release(ctx); err != nil {
		t.Fatalf("release without contact: %v", err)
	}
	if u.Outputting() {
		t.Fatal("still outputting")
	}
	// The unit keeps the old number while paused; the status says so.
	st, err := drv.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if *st.LevelA != 4 || *st.Outputting || *st.LoadDetected {
		t.Fatalf("status: level=%d outputting=%v load=%v", *st.LevelA, *st.Outputting, *st.LoadDetected)
	}
}

func TestDeviceSideLevelChangeIsSeen(t *testing.T) {
	ctx := context.Background()
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	drv := connect(t, u)
	u.PushLevel(6)
	deadline := time.Now().Add(2 * time.Second)
	for drv.State().DeviceLevelChanges == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if st := drv.State(); st.DeviceLevelChanges != 1 || st.Level == nil || *st.Level != 6 {
		t.Fatalf("state after push: %+v", st)
	}
	f, err := drv.Telemetry(ctx)
	if err != nil || *f.LevelA != 6 {
		t.Fatalf("frame after push: %+v, %v", f, err)
	}
}

func TestFragmentedRepliesReassemble(t *testing.T) {
	ctx := context.Background()
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	u.SetFragment(true)
	drv := connect(t, u)
	if _, err := drv.Ramp(ctx, 3, nil); err != nil {
		t.Fatal(err)
	}
	st, err := drv.Status(ctx)
	if err != nil || *st.LevelA != 3 || *st.BatteryPercent != 100 {
		t.Fatalf("status over fragments: %+v, %v", st, err)
	}
}

func TestAutoOffEndsTheLinkWithTheReason(t *testing.T) {
	ctx := context.Background()
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	drv := connect(t, u)
	u.AutoOff(0)
	deadline := time.Now().Add(2 * time.Second)
	for drv.LinkError() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	err := drv.LinkError()
	if !errors.Is(err, mastago.ErrLinkLost) || !strings.Contains(err.Error(), "switched itself off") || !strings.Contains(err.Error(), "no output") {
		t.Fatalf("link error = %v", err)
	}
	if _, err := drv.Status(ctx); !errors.Is(err, mastago.ErrLinkLost) {
		t.Fatalf("status after auto-off = %v", err)
	}
	if _, err := drv.Level(ctx); !errors.Is(err, mastago.ErrLinkLost) {
		t.Fatalf("command after auto-off = %v", err)
	}
}

func TestSilentUnitTimesOut(t *testing.T) {
	ctx := context.Background()
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	drv := connect(t, u)
	u.Mute = map[string]bool{"CSTR": true}
	if _, err := drv.Level(ctx); !errors.Is(err, mastago.ErrTimeout) {
		t.Fatalf("err = %v", err)
	}
	// Other commands still work afterwards.
	if _, err := drv.Mode(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestUnsupportedCommandErrorTolerated(t *testing.T) {
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	conn, _ := u.Connect()
	dev := mastago.New(conn, nil)
	dev.Gap = 0
	dev.Timeout = time.Second
	if err := dev.Listen(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer dev.Close(context.Background(), false)
	if err := dev.SetTimer(context.Background(), 60); err != nil {
		t.Fatal(err)
	}
	// The unit prefixes an unsupported-command error with stray bytes.
	_, err := dev.Level(context.Background())
	if err != nil {
		t.Fatal(err)
	}
}

// -- driver -------------------------------------------------------------------------

func TestArmSetsTheCountdownToTheWindow(t *testing.T) {
	ctx := context.Background()
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	drv := connect(t, u)
	drv.ArmWindow = 20 * time.Minute
	u.ResetCommands()
	if err := drv.Arm(ctx, estim.PowerModeHigh); err != nil {
		t.Fatal(err)
	}
	if cmds := u.Commands(); len(cmds) != 1 || cmds[0] != "AT+CDCLK=002000" {
		t.Fatalf("arm sent %v", cmds)
	}
	if u.TimerS() != 1200 || u.Outputting() {
		t.Fatalf("unit after arm: timer=%d outputting=%v", u.TimerS(), u.Outputting())
	}
	if err := drv.Arm(ctx, "low"); err == nil {
		t.Fatal("an unknown power range was accepted")
	}
}

func TestRenewArmRewritesTheCountdown(t *testing.T) {
	ctx := context.Background()
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	drv := connect(t, u)
	if _, err := drv.Ramp(ctx, 3, nil); err != nil {
		t.Fatal(err)
	}
	u.ResetCommands()
	var renewer estim.ArmRenewer = drv
	if err := renewer.RenewArm(ctx, time.Now().Add(10*time.Minute+30*time.Second)); err != nil {
		t.Fatal(err)
	}
	cmds := u.Commands()
	if len(cmds) != 1 || !strings.HasPrefix(cmds[0], "AT+CDCLK=0010") {
		t.Fatalf("renew sent %v", cmds)
	}
	if !u.Outputting() || u.Level() != 3 {
		t.Fatal("a renewal must not touch the output")
	}
}

func TestExecuteSetLevelAndAdjust(t *testing.T) {
	ctx := context.Background()
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	drv := connect(t, u)
	res, err := drv.Execute(ctx, estim.Command{Verb: "set_level", Level: estim.Int(4)}, 15, func() bool { return false })
	if err != nil || *res.Level != 4 || u.Level() != 4 {
		t.Fatalf("set_level: %+v, %v, unit %d", res, err, u.Level())
	}
	res, err = drv.Execute(ctx, estim.Command{Verb: "adjust_level", Delta: estim.Int(3)}, 6, func() bool { return false })
	if err != nil || *res.PreviousLevel != 4 || *res.Level != 6 || u.Level() != 6 {
		t.Fatalf("adjust_level capped: %+v, %v, unit %d", res, err, u.Level())
	}
	res, err = drv.Execute(ctx, estim.Command{Verb: "adjust_level", Delta: estim.Int(-5)}, 6, func() bool { return false })
	if err != nil || *res.Level != 1 || u.Level() != 1 {
		t.Fatalf("adjust_level down: %+v, %v", res, err)
	}
	if _, err := drv.Execute(ctx, estim.Command{Verb: "set_level", Level: estim.Int(20)}, 15, nil); err == nil {
		t.Fatal("a level past the cap was accepted")
	}
	if _, err := drv.Execute(ctx, estim.Command{Verb: "set_ma", Percent: estim.Int(50)}, 15, nil); err == nil {
		t.Fatal("a tempo command was accepted")
	}
}

func TestExecuteSetModeKeepsTheIntensity(t *testing.T) {
	ctx := context.Background()
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	drv := connect(t, u)
	if _, err := drv.Execute(ctx, estim.Command{Verb: "set_level", Level: estim.Int(3)}, 15, nil); err != nil {
		t.Fatal(err)
	}
	u.ResetCommands()
	res, err := drv.Execute(ctx, estim.Command{Verb: "set_mode", Mode: estim.Int(21)}, 15, nil)
	if err != nil || *res.Mode != 21 || *res.Level != 3 {
		t.Fatalf("set_mode: %+v, %v", res, err)
	}
	if u.Mode() != 21 || u.Level() != 3 || !u.Outputting() {
		t.Fatalf("unit: mode=%d level=%d outputting=%v", u.Mode(), u.Level(), u.Outputting())
	}
	cmds := strings.Join(u.Commands(), " ")
	if !strings.HasPrefix(cmds, "AT+CSTR? AT+QPOWP AT+CMODE=0,21 ") {
		t.Fatalf("set_mode sent %s", cmds)
	}
	if _, err := drv.Execute(ctx, estim.Command{Verb: "set_mode", Mode: estim.Int(40)}, 15, nil); err == nil {
		t.Fatal("a program past the range was accepted")
	}
}

func TestExecuteSetModeAtZeroStaysAtZero(t *testing.T) {
	ctx := context.Background()
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	drv := connect(t, u)
	res, err := drv.Execute(ctx, estim.Command{Verb: "set_mode", Mode: estim.Int(4)}, 15, nil)
	if err != nil || *res.Mode != 4 || res.Level != nil {
		t.Fatalf("set_mode at zero: %+v, %v", res, err)
	}
	if u.Outputting() || u.Level() != 0 {
		t.Fatal("output started on a program change at zero")
	}
}

func TestCloseWithRestoreReleases(t *testing.T) {
	ctx := context.Background()
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	conn, _ := u.Connect()
	drv, err := mastago.Connect(ctx, conn, nil)
	if err != nil {
		t.Fatal(err)
	}
	fast(drv)
	if _, err := drv.Ramp(ctx, 5, nil); err != nil {
		t.Fatal(err)
	}
	if err := drv.Close(ctx, true); err != nil {
		t.Fatal(err)
	}
	if u.Outputting() || u.Level() != 0 || u.Connections() != 0 {
		t.Fatalf("after close: outputting=%v level=%d links=%d", u.Outputting(), u.Level(), u.Connections())
	}
}
