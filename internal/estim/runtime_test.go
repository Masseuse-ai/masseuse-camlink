package estim_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/estim"
	"github.com/FemLed/masseuse-camlink/internal/estim/mastago/fakeunit"
)

// The Runtime's own rules (arm, latch, caps, settings, fault, telemetry),
// exercised over a fake Mastago unit (unitRuntime, runtime_mastago_test.go);
// what is the unit's alone is in that file.

// watched is unitRuntime with the OnDevice reports recorded.
func watched(t *testing.T, u *fakeunit.Unit) (*estim.Runtime, func() []estim.Descriptor) {
	t.Helper()
	rt := unitRuntime(t, u)
	var mu sync.Mutex
	var seen []estim.Descriptor
	rt.OnDevice = func(ctx context.Context, d estim.Descriptor) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, d)
	}
	return rt, func() []estim.Descriptor {
		mu.Lock()
		defer mu.Unlock()
		return append([]estim.Descriptor(nil), seen...)
	}
}

func TestRuntimeOpenArmExecuteRelease(t *testing.T) {
	ctx := context.Background()
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	u.PushLevel(7) // left running by someone
	rt, seen := watched(t, u)
	if rt.Connected() || rt.Armed() {
		t.Fatal("fresh runtime should hold nothing")
	}
	if st := rt.LastStatus(); st.Connected || *st.LevelA != 0 || st.Mode != nil {
		t.Fatalf("disconnected status = %+v", st)
	}
	if err := rt.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if !rt.Connected() || u.Level() != 0 || u.Outputting() {
		t.Fatal("open must release the device before trusting it")
	}
	if d := rt.Descriptor(); d.Kind != estim.KindMastago || !d.Connected || d.Capabilities.LevelMax != 25 || d.Capabilities.LevelMaxDefault != 15 {
		t.Fatalf("descriptor = %+v", d)
	}
	if s := rt.Settings(); s != estim.DefaultSettingsFor(rt.Descriptor().Capabilities) || s.PowerMode != estim.PowerModeNormal || s.LevelMax != 15 {
		t.Fatalf("settings before a session = %+v", s)
	}
	if got := seen(); len(got) != 1 || !got[0].Connected {
		t.Fatalf("OnDevice calls = %+v", got)
	}
	if err := rt.Open(ctx); err != nil {
		t.Fatal("second open should be a no-op")
	}

	// Not armed: actuation refused, status and release allowed.
	if _, _, err := rt.Execute(ctx, estim.Command{Verb: "set_level", Level: estim.Int(5)}); !errors.Is(err, estim.ErrNotArmed) {
		t.Fatalf("unarmed set_level: %v", err)
	}
	if res, st, err := rt.Execute(ctx, estim.Command{Verb: "status"}); err != nil || res.Verb != "status" || !st.Connected {
		t.Fatalf("status: %+v %v", res, err)
	}

	if err := rt.Arm(ctx); err != nil {
		t.Fatal(err)
	}
	// The arm sets the unit's own countdown to the arm window (20 minutes
	// in unitRuntime) so the unit stops by itself if the connector dies.
	if !rt.Armed() || rt.CancelLatched() || u.TimerS() != 20*60 {
		t.Fatalf("armed=%v latched=%v timer=%d", rt.Armed(), rt.CancelLatched(), u.TimerS())
	}
	until, ok := rt.ArmedUntil()
	if !ok || time.Until(until) > 20*time.Minute || time.Until(until) < 19*time.Minute {
		t.Fatalf("arm window = %v", time.Until(until))
	}
	if !rt.ExtendArm() {
		t.Fatal("extend while armed")
	}
	res, st, err := rt.Execute(ctx, estim.Command{Verb: "set_level", Level: estim.Int(12)})
	if err != nil || *res.Level != 12 || *st.LevelA != 12 || u.Level() != 12 || !u.Outputting() {
		t.Fatalf("set_level: %+v %+v %v", res, st, err)
	}
	if _, _, err := rt.Execute(ctx, estim.Command{Verb: "set_level", Level: estim.Int(16)}); err == nil {
		t.Fatal("level over the default cap accepted")
	}
	if _, _, err := rt.Execute(ctx, estim.Command{Verb: "set_mode", Mode: estim.Int(32)}); err == nil {
		t.Fatal("program outside the device's list accepted")
	}

	// Release: outputs zero, latch set, arm revoked; a renewal is refused.
	st, err = rt.Release(ctx, "test")
	if err != nil || *st.LevelA != 0 || u.Level() != 0 || u.Outputting() {
		t.Fatalf("release: %+v %v", st, err)
	}
	if rt.Armed() || !rt.CancelLatched() || rt.ExtendArm() {
		t.Fatal("release must revoke the arm and set the latch")
	}
	if _, _, err := rt.Execute(ctx, estim.Command{Verb: "set_level", Level: estim.Int(5)}); !errors.Is(err, estim.ErrNotArmed) {
		t.Fatalf("after release: %v", err)
	}
	// The release verb works unarmed too.
	if res, _, err := rt.Execute(ctx, estim.Command{Verb: "release"}); err != nil || !res.Released {
		t.Fatalf("release verb: %+v %v", res, err)
	}

	// Arming again clears the latch.
	if err := rt.Arm(ctx); err != nil || !rt.Armed() {
		t.Fatalf("re-arm: %v", err)
	}
	if err := rt.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if rt.Connected() || u.Outputting() || u.Level() != 0 || u.Connections() != 0 {
		t.Fatal("close must release and disconnect")
	}
	if st := rt.LastStatus(); st.Connected || *st.LevelA != 0 {
		t.Fatalf("status after close = %+v", st)
	}
	if got := seen(); len(got) != 2 || got[1].Connected {
		t.Fatalf("OnDevice calls = %+v", got)
	}
}

func TestRuntimeSettings(t *testing.T) {
	ctx := context.Background()
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	rt := unitRuntime(t, u)
	if err := rt.Open(ctx); err != nil {
		t.Fatal(err)
	}
	defaults := rt.Settings()
	// Refused whole; nothing changes.
	for _, bad := range []estim.Settings{{PowerMode: "low", LevelMax: 10}, {PowerMode: "normal", LevelMax: 100}, {PowerMode: "", LevelMax: 10}, {PowerMode: "normal", LevelMax: -1}} {
		if released, err := rt.SetSettings(ctx, bad); err == nil || released {
			t.Errorf("%+v accepted", bad)
		}
	}
	if rt.Settings() != defaults {
		t.Fatal("refused settings changed something")
	}
	// Not armed: taken silently, and the next arm is inside them.
	normal10 := estim.Settings{PowerMode: "normal", LevelMax: 10}
	if released, err := rt.SetSettings(ctx, normal10); err != nil || released {
		t.Fatalf("unarmed change: released=%v err=%v", released, err)
	}
	if err := rt.Arm(ctx); err != nil {
		t.Fatal(err)
	}
	// The session's maximum bounds every level, below the default cap or above it.
	if _, _, err := rt.Execute(ctx, estim.Command{Verb: "set_level", Level: estim.Int(11)}); err == nil {
		t.Fatal("level over the session's maximum accepted")
	}
	if _, st, err := rt.Execute(ctx, estim.Command{Verb: "set_level", Level: estim.Int(10)}); err != nil || *st.LevelA != 10 {
		t.Fatalf("set_level at the maximum: %+v %v", st, err)
	}
	if res, _, err := rt.Execute(ctx, estim.Command{Verb: "adjust_level", Delta: estim.Int(5)}); err != nil || *res.Level != 10 {
		t.Fatalf("adjust_level past the maximum must clamp at it: %+v %v", res, err)
	}
	// The same again is no change.
	if released, err := rt.SetSettings(ctx, normal10); err != nil || released || u.Level() != 10 {
		t.Fatalf("unchanged settings: released=%v err=%v level=%d", released, err, u.Level())
	}
	// A maximum at or above the level applies in place.
	if released, err := rt.SetSettings(ctx, estim.Settings{PowerMode: "normal", LevelMax: 20}); err != nil || released || u.Level() != 10 || !rt.Armed() {
		t.Fatalf("raised maximum: released=%v err=%v level=%d armed=%v", released, err, u.Level(), rt.Armed())
	}
	if _, st, err := rt.Execute(ctx, estim.Command{Verb: "set_level", Level: estim.Int(20)}); err != nil || *st.LevelA != 20 {
		t.Fatalf("set_level to 20: %+v %v", st, err)
	}
	if _, _, err := rt.Execute(ctx, estim.Command{Verb: "set_level", Level: estim.Int(21)}); err == nil {
		t.Fatal("level over the raised maximum accepted")
	}
	// The device's own scale holds over the session's maximum.
	if released, err := rt.SetSettings(ctx, estim.Settings{PowerMode: "normal", LevelMax: 99}); err != nil || released {
		t.Fatalf("maximum past the scale: released=%v err=%v", released, err)
	}
	if _, _, err := rt.Execute(ctx, estim.Command{Verb: "set_level", Level: estim.Int(26)}); err == nil {
		t.Fatal("level past the device's scale accepted")
	}
	// A maximum below the level cannot apply in place: released, latched, not armed.
	if released, err := rt.SetSettings(ctx, estim.Settings{PowerMode: "normal", LevelMax: 8}); err != nil || !released {
		t.Fatalf("lowered maximum under the level: released=%v err=%v", released, err)
	}
	if u.Level() != 0 || u.Outputting() || rt.Armed() || !rt.CancelLatched() {
		t.Fatalf("after the release: level=%d outputting=%v armed=%v latched=%v", u.Level(), u.Outputting(), rt.Armed(), rt.CancelLatched())
	}
	if rt.Settings().LevelMax != 8 {
		t.Fatal("the settings must stand after the release")
	}
	// Arming again is inside the new bounds.
	if err := rt.Arm(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := rt.Execute(ctx, estim.Command{Verb: "set_level", Level: estim.Int(9)}); err == nil {
		t.Fatal("level over the lowered maximum accepted")
	}
	if _, st, err := rt.Execute(ctx, estim.Command{Verb: "set_level", Level: estim.Int(6)}); err != nil || *st.LevelA != 6 {
		t.Fatalf("set_level to 6: %+v %v", st, err)
	}
	// A power range change while armed cannot apply in place either, even
	// on a device with one range: the arm is what selects it.
	if released, err := rt.SetSettings(ctx, estim.Settings{PowerMode: "high", LevelMax: 8}); err != nil || !released {
		t.Fatalf("power change: released=%v err=%v", released, err)
	}
	if u.Level() != 0 || rt.Armed() {
		t.Fatal("power change must release")
	}
	if err := rt.Arm(ctx); err != nil {
		t.Fatal(err)
	}
	if !rt.Armed() || u.TimerS() < 19*60 {
		t.Fatalf("re-armed: armed=%v timer=%d", rt.Armed(), u.TimerS())
	}
	// Reset: the defaults, with no device I/O.
	u.ResetCommands()
	rt.ResetSettings()
	if rt.Settings() != defaults || !rt.Armed() || len(u.Commands()) != 0 {
		t.Fatalf("reset must restore the defaults and touch nothing: %+v %v", rt.Settings(), u.Commands())
	}
	if err := rt.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeArmExpiry(t *testing.T) {
	ctx := context.Background()
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	rt := unitRuntime(t, u)
	now := time.Now()
	rt.Now = func() time.Time { return now }
	rt.ArmWindow = time.Minute
	if err := rt.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if err := rt.Arm(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := rt.Execute(ctx, estim.Command{Verb: "set_level", Level: estim.Int(4)}); err != nil {
		t.Fatal(err)
	}
	if released, _ := rt.ReleaseIfArmExpired(ctx); released {
		t.Fatal("released before expiry")
	}
	now = now.Add(61 * time.Second)
	if rt.Armed() {
		t.Fatal("armed past the window")
	}
	released, err := rt.ReleaseIfArmExpired(ctx)
	if !released || err != nil || u.Level() != 0 || u.Outputting() || !rt.CancelLatched() {
		t.Fatalf("expiry: %v %v level=%d", released, err, u.Level())
	}
	if released, _ := rt.ReleaseIfArmExpired(ctx); released {
		t.Fatal("released twice")
	}
	// A window longer than the cap is cut to it.
	rt.ArmWindow = 2 * time.Hour
	if err := rt.Arm(ctx); err != nil {
		t.Fatal(err)
	}
	if until, _ := rt.ArmedUntil(); until.Sub(now) != estim.MaxArmWindow {
		t.Fatalf("window = %v", until.Sub(now))
	}
}

func TestRuntimeFaultAndReconnect(t *testing.T) {
	ctx := context.Background()
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	rt, seen := watched(t, u)
	if err := rt.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if err := rt.Arm(ctx); err != nil {
		t.Fatal(err)
	}
	if !rt.HealthCheck(ctx) {
		t.Fatal("healthy device failed its check")
	}
	// The unit stops answering: the check fails, the device is forgotten
	// and the latch set.
	u.Mute = map[string]bool{"CMODE": true}
	if rt.HealthCheck(ctx) {
		t.Fatal("silent device passed its check")
	}
	if rt.Connected() || rt.Armed() || !rt.CancelLatched() {
		t.Fatal("fault must forget the device and latch")
	}
	st := rt.LastStatus()
	if st.Connected || st.LevelA != nil || st.LevelB != nil || st.ADCOverride != nil || st.Error == nil {
		t.Fatalf("faulted status must have unknown safety fields: %+v", st)
	}
	raw, _ := json.Marshal(st)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if v, ok := m["levelA"]; !ok || v != nil {
		t.Fatalf("faulted levelA must serialize as null: %s", raw)
	}
	if rt.LastError() == "" {
		t.Fatal("no last error")
	}
	if got := seen(); len(got) != 2 || got[1].Connected || got[1].Kind != estim.KindMastago {
		t.Fatalf("OnDevice calls = %+v", got)
	}
	// While the unit is silent, reconnecting fails quietly.
	if err := rt.Open(ctx); err == nil {
		t.Fatal("open succeeded against a silent device")
	}
	// The unit answers again and is picked up released and unarmed.
	u.Mute = nil
	if err := rt.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if !rt.Connected() || rt.Armed() || !rt.CancelLatched() {
		t.Fatal("reconnected device must not be armed")
	}
	if got := seen(); len(got) != 3 || !got[2].Connected {
		t.Fatalf("OnDevice calls = %+v", got)
	}
}

func TestRuntimeSample(t *testing.T) {
	ctx := context.Background()
	u := fakeunit.New("id-1", "MASTOGO G-12AB")
	rt := unitRuntime(t, u)
	started := time.Now()
	f := rt.Sample(ctx, started)
	if !f.Skipped || *f.SkipReason != "disconnected" {
		t.Fatalf("disconnected sample = %+v", f)
	}
	if err := rt.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if err := rt.Arm(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := rt.Execute(ctx, estim.Command{Verb: "set_mode", Mode: estim.Int(4)}); err != nil {
		t.Fatal(err)
	}
	f = rt.Sample(ctx, started)
	if f.Skipped || *f.Mode != 4 || f.AtMs == 0 || f.LevelA == nil || f.Outputting == nil || f.TimerRemainingS == nil {
		t.Fatalf("sample = %+v", f)
	}
	raw, _ := json.Marshal(f)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if m["skipped"] != false || m["skipReason"] != nil || m["levelA"] != 0.0 || m["outputting"] != false || m["loadDetected"] != true {
		t.Fatalf("frame json = %s", raw)
	}
	if _, ok := m["sweepPercent"]; ok {
		t.Fatalf("a device without modulation state must not report one: %s", raw)
	}
	// A unit that stops answering its readings: the frame says so.
	u.Mute = map[string]bool{"CSTR": true}
	f = rt.Sample(ctx, started)
	if !f.Skipped || *f.SkipReason != "read_error" || f.Error == nil {
		t.Fatalf("read error sample = %+v", f)
	}
	u.Mute = nil
	frames, gaps, readErrors := rt.Counters()
	if frames != 1 || gaps != 2 || readErrors != 1 {
		t.Fatalf("counters = %d %d %d", frames, gaps, readErrors)
	}

	rt.Telemetry.Offer(f)
	rt.Telemetry.Offer(f)
	if rt.Telemetry.Len() != 2 || len(rt.Telemetry.Drain(1)) != 1 || rt.Telemetry.Len() != 1 {
		t.Fatal("ring")
	}
}

func TestRingDropsOldest(t *testing.T) {
	var r estim.Ring
	for i := 0; i < estim.RingCapacity+5; i++ {
		r.Offer(estim.Frame{AtMs: int64(i)})
	}
	if r.Len() != estim.RingCapacity || r.Dropped() != 5 {
		t.Fatalf("len=%d dropped=%d", r.Len(), r.Dropped())
	}
	if got := r.Drain(1); len(got) != 1 || got[0].AtMs != 5 {
		t.Fatalf("oldest kept = %+v", got)
	}
}

func TestParseCommandAndCaps(t *testing.T) {
	cmd, err := estim.ParseCommand(json.RawMessage(`{"verb":"set_level","channel":"a","level":7,"reason":"x","extra":true}`))
	if err != nil || cmd.Verb != "set_level" || *cmd.Level != 7 || cmd.Reason != "x" {
		t.Fatalf("%+v %v", cmd, err)
	}
	for _, bad := range []string{`{"verb":"set_level","level":7.5}`, `{"verb":"set_level","level":true}`, `{"verb":"set_level","level":"7"}`, `{"verb":"nope"}`, `{"verb":"set_level","channel":"b","level":1}`, `[]`, `null`} {
		if _, err := estim.ParseCommand(json.RawMessage(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	// A device on the full scale with two programs and no tempo control.
	caps := estim.Capabilities{LevelMax: 99, Channels: []string{"a"}, Modes: []int{1, 2}, Tempo: false}
	defaults := estim.DefaultSettings()
	if err := estim.CheckCaps(estim.Command{Verb: "set_level", Level: estim.Int(86)}, caps, defaults); err == nil {
		t.Error("default cap of 85 must hold even when the device allows more")
	}
	if err := estim.CheckCaps(estim.Command{Verb: "set_level", Level: estim.Int(85)}, caps, defaults); err != nil {
		t.Error(err)
	}
	// The session's maximum moves the bound either way, within the device's scale.
	if err := estim.CheckCaps(estim.Command{Verb: "set_level", Level: estim.Int(99)}, caps, estim.Settings{PowerMode: "high", LevelMax: 99}); err != nil {
		t.Error(err)
	}
	if err := estim.CheckCaps(estim.Command{Verb: "set_level", Level: estim.Int(41)}, caps, estim.Settings{PowerMode: "high", LevelMax: 40}); err == nil {
		t.Error("level over the session's maximum accepted")
	}
	// The Mastago's scale: 25 levels, 15 until the session says otherwise.
	small := estim.Capabilities{LevelMax: 25, LevelMaxDefault: 15, Channels: []string{"a"}, Modes: []int{1}, Tempo: false}
	if err := estim.CheckCaps(estim.Command{Verb: "set_level", Level: estim.Int(26)}, small, estim.Settings{PowerMode: "normal", LevelMax: 99}); err == nil {
		t.Error("the device's own scale must hold over the session's maximum")
	}
	if err := estim.CheckCaps(estim.Command{Verb: "set_level", Level: estim.Int(16)}, small, estim.DefaultSettingsFor(small)); err == nil {
		t.Error("the device's default maximum must hold before a session sets one")
	}
	if err := estim.CheckCaps(estim.Command{Verb: "set_level", Level: estim.Int(15)}, small, estim.DefaultSettingsFor(small)); err != nil {
		t.Error(err)
	}
	if got := estim.LevelMaxFor(small, estim.Settings{LevelMax: 99}); got != 25 {
		t.Errorf("LevelMaxFor = %d", got)
	}
	if got := estim.LevelMaxFor(caps, estim.Settings{LevelMax: 120}); got != 99 {
		t.Errorf("LevelMaxFor over the scale = %d", got)
	}
	if err := estim.CheckCaps(estim.Command{Verb: "set_ma", Percent: estim.Int(10)}, caps, defaults); err == nil || !strings.Contains(err.Error(), "tempo") {
		t.Errorf("tempo on a device without one: %v", err)
	}
	if err := estim.CheckCaps(estim.Command{Verb: "set_mode", Mode: estim.Int(3)}, caps, defaults); err == nil {
		t.Error("program outside the device's list")
	}
	if err := estim.CheckCaps(estim.Command{Verb: "set_mode", Mode: estim.Int(2)}, caps, defaults); err != nil {
		t.Error(err)
	}
}

func TestParseSettings(t *testing.T) {
	s, err := estim.ParseSettings(json.RawMessage(`{"type":"device_settings","sessionId":"s","powerMode":"normal","levelMax":15,"extra":1}`))
	if err != nil || s != (estim.Settings{PowerMode: "normal", LevelMax: 15}) {
		t.Fatalf("%+v %v", s, err)
	}
	if s, err := estim.ParseSettings(json.RawMessage(`{"powerMode":"high","levelMax":99}`)); err != nil || s.LevelMax != 99 {
		t.Fatalf("%+v %v", s, err)
	}
	for _, bad := range []string{
		`{"powerMode":"low","levelMax":15}`, `{"powerMode":"NORMAL","levelMax":15}`, `{"powerMode":"normal","levelMax":100}`,
		`{"powerMode":"normal","levelMax":-1}`, `{"powerMode":"normal","levelMax":15.5}`, `{"powerMode":"normal","levelMax":"15"}`,
		`{"powerMode":"normal"}`, `{"levelMax":15}`, `{}`, `[]`, `null`, ``, `{"powerMode":null,"levelMax":15}`,
	} {
		if s, err := estim.ParseSettings(json.RawMessage(bad)); err == nil {
			t.Errorf("%s accepted as %+v", bad, s)
		}
	}
	if err := (estim.Settings{PowerMode: "normal", LevelMax: 0}).Validate(); err != nil {
		t.Error("a maximum of zero is a valid, if useless, setting")
	}
}
