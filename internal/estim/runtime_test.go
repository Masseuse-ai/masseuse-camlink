package estim_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/estim"
	"github.com/FemLed/masseuse-camlink/internal/estim/mk312"
	"github.com/FemLed/masseuse-camlink/internal/estim/mk312/fakebox"
)

// boxRuntime is a Runtime whose Connect handshakes the fake box, with the
// device's settle delays and timeouts shortened.
func boxRuntime(t *testing.T, box *fakebox.Box) (*estim.Runtime, *[]estim.Descriptor) {
	t.Helper()
	var mu sync.Mutex
	var seen []estim.Descriptor
	rt := &estim.Runtime{
		Connect: func(ctx context.Context) (estim.Driver, error) {
			box.Reopen() // a real Connect opens the serial port afresh
			dev := mk312.New(box, "fake")
			dev.Sleep = func(context.Context, time.Duration) error { return nil }
			dev.Timeout = 30 * time.Millisecond
			dev.KeyExchangeTimeout = 100 * time.Millisecond
			dev.Ramp = 0
			dev.SyncTries = 2
			return mk312.Connect(ctx, dev, nil, nil)
		},
		OnDevice: func(ctx context.Context, d estim.Descriptor) {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, d)
		},
	}
	return rt, &seen
}

func TestRuntimeOpenArmExecuteRelease(t *testing.T) {
	ctx := context.Background()
	box := fakebox.New()
	box.Set(mk312.AddressLevelA, mk312.LCDToRAM(30)) // left running by someone
	rt, seen := boxRuntime(t, box)
	if rt.Connected() || rt.Armed() {
		t.Fatal("fresh runtime should hold nothing")
	}
	if st := rt.LastStatus(); st.Connected || *st.LevelA != 0 || st.Mode != nil {
		t.Fatalf("disconnected status = %+v", st)
	}
	if err := rt.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if !rt.Connected() || box.LevelA() != 0 || box.Power() != mk312.PowerNormal {
		t.Fatal("open must release the device before trusting it")
	}
	if d := rt.Descriptor(); d.Kind != estim.KindMK312BT || !d.Connected || d.Capabilities.LevelMax != 85 {
		t.Fatalf("descriptor = %+v", d)
	}
	if len(*seen) != 1 || !(*seen)[0].Connected {
		t.Fatalf("OnDevice calls = %+v", *seen)
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
	if !rt.Armed() || rt.CancelLatched() || box.Power() != mk312.PowerHigh {
		t.Fatalf("armed=%v latched=%v power=%d", rt.Armed(), rt.CancelLatched(), box.Power())
	}
	until, ok := rt.ArmedUntil()
	if !ok || time.Until(until) > estim.MaxArmWindow || time.Until(until) < 29*time.Minute {
		t.Fatalf("arm window = %v", time.Until(until))
	}
	if !rt.ExtendArm() {
		t.Fatal("extend while armed")
	}
	res, st, err := rt.Execute(ctx, estim.Command{Verb: "set_level", Level: estim.Int(12)})
	if err != nil || *res.Level != 12 || *st.LevelA != 12 || box.LevelA() != 12 {
		t.Fatalf("set_level: %+v %+v %v", res, st, err)
	}
	if _, _, err := rt.Execute(ctx, estim.Command{Verb: "set_level", Level: estim.Int(90)}); err == nil {
		t.Fatal("level over the cap accepted")
	}
	if _, _, err := rt.Execute(ctx, estim.Command{Verb: "set_mode", Mode: estim.Int(0x7C)}); err == nil {
		t.Fatal("pattern outside the allow-list accepted")
	}

	// Release: outputs zero, latch set, arm revoked; a renewal is refused.
	st, err = rt.Release(ctx, "test")
	if err != nil || *st.LevelA != 0 || box.LevelA() != 0 || box.Power() != mk312.PowerNormal {
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
	if rt.Connected() || box.Key() != mk312.NoKey || box.Power() != mk312.PowerNormal || !box.Closed() {
		t.Fatal("close must release, unkey and close the port")
	}
	if st := rt.LastStatus(); st.Connected || *st.LevelA != 0 {
		t.Fatalf("status after close = %+v", st)
	}
	if n := len(*seen); n != 2 || (*seen)[1].Connected {
		t.Fatalf("OnDevice calls = %+v", *seen)
	}
}

func TestRuntimeArmExpiry(t *testing.T) {
	ctx := context.Background()
	box := fakebox.New()
	rt, _ := boxRuntime(t, box)
	now := time.Now()
	rt.Now = func() time.Time { return now }
	rt.ArmWindow = time.Minute
	if err := rt.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if err := rt.Arm(ctx); err != nil {
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
	if !released || err != nil || box.Power() != mk312.PowerNormal || !rt.CancelLatched() {
		t.Fatalf("expiry: %v %v", released, err)
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
	box := fakebox.New()
	rt, seen := boxRuntime(t, box)
	if err := rt.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if err := rt.Arm(ctx); err != nil {
		t.Fatal(err)
	}
	if !rt.HealthCheck(ctx) {
		t.Fatal("healthy device failed its check")
	}
	box.SetDead(true)
	if rt.HealthCheck(ctx) {
		t.Fatal("dead device passed its check")
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
	if n := len(*seen); n != 2 || (*seen)[1].Connected || (*seen)[1].Kind != estim.KindMK312BT {
		t.Fatalf("OnDevice calls = %+v", *seen)
	}
	// While the device is dead, reconnecting fails quietly.
	if err := rt.Open(ctx); err == nil {
		t.Fatal("open succeeded against a dead device")
	}
	// The device comes back (power-cycled: unkeyed, at rest) and is picked
	// up released and unarmed.
	box.SetDead(false)
	box.PowerCycle()
	if err := rt.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if !rt.Connected() || rt.Armed() || !rt.CancelLatched() {
		t.Fatal("reconnected device must not be armed")
	}
	if n := len(*seen); n != 3 || !(*seen)[2].Connected {
		t.Fatalf("OnDevice calls = %+v", *seen)
	}
}

func TestRuntimeSample(t *testing.T) {
	ctx := context.Background()
	box := fakebox.New()
	rt, _ := boxRuntime(t, box)
	started := time.Now()
	f := rt.Sample(ctx, started)
	if !f.Skipped || *f.SkipReason != "disconnected" {
		t.Fatalf("disconnected sample = %+v", f)
	}
	if err := rt.Open(ctx); err != nil {
		t.Fatal(err)
	}
	f = rt.Sample(ctx, started)
	if f.Skipped || *f.Mode != 0x76 || f.AtMs == 0 || f.RoutineState == nil {
		t.Fatalf("sample = %+v", f)
	}
	raw, _ := json.Marshal(f)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if m["skipped"] != false || m["skipReason"] != nil || m["levelA"] != 0.0 || m["sweepPercent"] != 50.0 {
		t.Fatalf("frame json = %s", raw)
	}
	box.AnswerReads = false
	f = rt.Sample(ctx, started)
	if !f.Skipped || *f.SkipReason != "read_error" || f.Error == nil {
		t.Fatalf("read error sample = %+v", f)
	}
	box.AnswerReads = true
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
	caps := estim.Capabilities{LevelMax: 99, Channels: []string{"a"}, Modes: []int{1, 2}, Tempo: false}
	if err := estim.CheckCaps(estim.Command{Verb: "set_level", Level: estim.Int(86)}, caps); err == nil {
		t.Error("connector cap of 85 must hold even when the device allows more")
	}
	if err := estim.CheckCaps(estim.Command{Verb: "set_ma", Percent: estim.Int(10)}, caps); err == nil {
		t.Error("tempo on a device without one")
	}
	if err := estim.CheckCaps(estim.Command{Verb: "set_mode", Mode: estim.Int(3)}, caps); err == nil {
		t.Error("pattern outside the device's list")
	}
	if err := estim.CheckCaps(estim.Command{Verb: "set_mode", Mode: estim.Int(2)}, caps); err != nil {
		t.Error(err)
	}
}
