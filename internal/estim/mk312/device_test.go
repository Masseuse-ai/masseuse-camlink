package mk312_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/estim"
	"github.com/FemLed/masseuse-camlink/internal/estim/mk312"
	"github.com/FemLed/masseuse-camlink/internal/estim/mk312/fakebox"
	"github.com/FemLed/masseuse-camlink/internal/serialport"
)

func fast(d *mk312.Device) *mk312.Device {
	d.Sleep = func(context.Context, time.Duration) error { return nil }
	d.Timeout = 50 * time.Millisecond
	d.KeyExchangeTimeout = 100 * time.Millisecond
	d.Ramp = 0
	return d
}

func handshaken(t *testing.T) (*fakebox.Box, *mk312.Device) {
	t.Helper()
	box := fakebox.New()
	dev := fast(mk312.New(box, "fake"))
	if err := dev.Handshake(context.Background()); err != nil {
		t.Fatal(err)
	}
	return box, dev
}

func TestHandshakeAgreesOnTheKey(t *testing.T) {
	box, dev := handshaken(t)
	if dev.Key() == mk312.NoKey || dev.Key() != box.Key() {
		t.Fatalf("keys: device %d, box %d", dev.Key(), box.Key())
	}
	// With host key 0 the session key is the box key XOR 0x55.
	if dev.Key() != int(box.BoxKey^mk312.BoxKeyXOR) {
		t.Fatalf("key = %#x", dev.Key())
	}
	mode, err := dev.Peek(context.Background(), mk312.AddressCurrentMode)
	if err != nil || mode != 0x76 {
		t.Fatalf("peek under key: %d, %v", mode, err)
	}
	if err := dev.Poke(context.Background(), mk312.AddressLevelA, 10); err != nil || box.Register(mk312.AddressLevelA) != 10 {
		t.Fatalf("poke under key: %v", err)
	}
}

func TestHandshakeFailures(t *testing.T) {
	ctx := context.Background()
	dead := fakebox.New()
	dead.SetDead(true)
	dev := fast(mk312.New(dead, "fake"))
	dev.SyncTries = 2
	if err := dev.Handshake(ctx); !errors.Is(err, mk312.ErrNoBeacon) || !errors.Is(err, mk312.ErrHandshake) {
		t.Fatalf("dead box: %v", err)
	}
	mute := fakebox.New()
	mute.AnswerKeyExchange = false
	dev = fast(mk312.New(mute, "fake"))
	if err := dev.Handshake(ctx); !errors.Is(err, mk312.ErrNoKeyExchange) {
		t.Fatalf("box that never keys: %v", err)
	}
	if dev.Key() != mk312.NoKey {
		t.Fatal("key set after a failed handshake")
	}
}

func TestResume(t *testing.T) {
	ctx := context.Background()
	box, dev := handshaken(t)
	key := dev.Key()

	// A new connection with the right key reads straight away.
	again := fast(mk312.New(box, "fake"))
	if err := again.Resume(ctx, key); err != nil {
		t.Fatalf("resume: %v", err)
	}
	// A power-cycled box answers the wrong key with a NAK: a protocol error,
	// which tells the caller the key is stale.
	box.PowerCycle()
	box.Beacon = false
	stale := fast(mk312.New(box, "fake"))
	err := stale.Resume(ctx, key)
	if !mk312.IsProtocolError(err) || stale.Key() != mk312.NoKey {
		t.Fatalf("stale key: %v (key %d)", err, stale.Key())
	}
	// Silence says nothing about the key.
	box.SetDead(true)
	silent := fast(mk312.New(box, "fake"))
	if err := silent.Resume(ctx, key); !errors.Is(err, mk312.ErrSilent) || mk312.IsProtocolError(err) {
		t.Fatalf("silent box: %v", err)
	}
	if err := silent.Resume(ctx, 300); err == nil {
		t.Fatal("out-of-range key accepted")
	}
}

func TestCloseReleasesAndUnkeys(t *testing.T) {
	ctx := context.Background()
	box, dev := handshaken(t)
	if err := dev.SetLevelA(ctx, 20); err != nil {
		t.Fatal(err)
	}
	if err := dev.SetPowerLevel(ctx, mk312.PowerHigh); err != nil {
		t.Fatal(err)
	}
	if err := dev.Close(ctx, true); err != nil {
		t.Fatal(err)
	}
	if box.LevelA() != 0 || box.LevelB() != 0 || box.Power() != mk312.PowerNormal || box.ADCDisabled() {
		t.Fatalf("box not released: A=%d B=%d power=%d adc=%v", box.LevelA(), box.LevelB(), box.Power(), box.ADCDisabled())
	}
	if box.Key() != mk312.NoKey || dev.Key() != mk312.NoKey {
		t.Fatal("box still keyed after close")
	}
	// Closing again is a no-op; a fresh handshake works.
	if err := dev.Close(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := fast(mk312.New(box, "fake")).Handshake(ctx); err != nil {
		t.Fatalf("handshake after close: %v", err)
	}
}

func TestSetLevelAPinsChannelBAndOverridesTheKnob(t *testing.T) {
	ctx := context.Background()
	box, dev := handshaken(t)
	box.Set(mk312.AddressLevelB, 40)
	if err := dev.SetLevelA(ctx, 30); err != nil {
		t.Fatal(err)
	}
	if box.LevelA() != 30 || box.LevelB() != 0 || !box.ADCDisabled() {
		t.Fatalf("A=%d B=%d adc=%v", box.LevelA(), box.LevelB(), box.ADCDisabled())
	}
	if err := dev.SetLevelA(ctx, 100); err == nil {
		t.Fatal("level above the panel scale accepted")
	}
	if l, err := dev.LevelA(ctx); err != nil || l != 30 {
		t.Fatalf("LevelA = %d, %v", l, err)
	}
}

func TestRampStepsAndCancels(t *testing.T) {
	ctx := context.Background()
	box, dev := handshaken(t)
	applied, err := dev.RampLevelA(ctx, 5, nil)
	if err != nil || applied != 5 || box.LevelA() != 5 {
		t.Fatalf("ramp: %d, %v (box %d)", applied, err, box.LevelA())
	}
	var levels []int
	for _, w := range box.Writes() {
		if w.Addr == mk312.AddressLevelA {
			levels = append(levels, mk312.RAMToLCD(int(w.Value)))
		}
	}
	// Never a jump: one panel step per write.
	for i := 1; i < len(levels); i++ {
		if d := levels[i] - levels[i-1]; d > 1 || d < 0 {
			t.Fatalf("ramp wrote %v", levels)
		}
	}

	// A latch set mid-ramp stops it where it is.
	dev.Ramp = 5 * time.Millisecond
	var steps atomic.Int32
	cancelled := func() bool { return steps.Add(1) > 3 }
	applied, err = dev.RampLevelA(ctx, 40, cancelled)
	if !errors.Is(err, estim.ErrCancelled) {
		t.Fatalf("cancelled ramp: %d, %v", applied, err)
	}
	if box.LevelA() >= 40 || box.LevelA() < 5 {
		t.Fatalf("box level after cancel = %d", box.LevelA())
	}
	// Ramping down works the same way.
	dev.Ramp = 0
	if applied, err = dev.RampLevelA(ctx, 0, nil); err != nil || applied != 0 || box.LevelA() != 0 {
		t.Fatalf("ramp down: %d, %v", applied, err)
	}
}

func TestSetModeAndPower(t *testing.T) {
	ctx := context.Background()
	box, dev := handshaken(t)
	if err := dev.SetMode(ctx, 0x77); err != nil || box.Mode() != 0x77 {
		t.Fatalf("set pattern: %v (box %#x)", err, box.Mode())
	}
	var cmds []byte
	for _, w := range box.Writes() {
		if w.Addr == mk312.AddressCommand {
			cmds = append(cmds, w.Value)
		}
	}
	if len(cmds) != 2 || cmds[0] != mk312.CommandExitMenu || cmds[1] != mk312.CommandNewMode {
		t.Fatalf("pattern load commands = % x", cmds)
	}
	if err := dev.SetMode(ctx, 0x77); err != nil {
		t.Fatal("re-selecting the loaded pattern should be a no-op")
	}
	if err := dev.SetMode(ctx, 0x20); err == nil {
		t.Fatal("unknown pattern accepted")
	}
	box.Set(mk312.AddressLevelA, 100)
	if err := dev.SetPowerLevel(ctx, mk312.PowerHigh); err != nil {
		t.Fatal(err)
	}
	if box.Power() != mk312.PowerHigh || box.LevelA() != 0 {
		t.Fatalf("power=%d A=%d: outputs must be zeroed before a range change", box.Power(), box.LevelA())
	}
	if err := dev.SetPowerLevel(ctx, 7); err == nil {
		t.Fatal("bad power range accepted")
	}
}

func TestSetLevelMA(t *testing.T) {
	ctx := context.Background()
	box, dev := handshaken(t)
	raw, err := dev.SetLevelMA(ctx, 50)
	if err != nil || raw != 33 || box.Register(mk312.AddressLevelMA) != 33 || !box.MAPotDisabled() || !box.ADCDisabled() {
		t.Fatalf("tempo: %d, %v (pot override %v)", raw, err, box.MAPotDisabled())
	}
	if _, err := dev.SetLevelMA(ctx, 101); err == nil {
		t.Fatal("tempo over 100 accepted")
	}
	box.Set(mk312.AddressCurrentMode, 0x80)
	if _, err := dev.SetLevelMA(ctx, 50); err == nil {
		t.Fatal("tempo accepted in a pattern where it has no effect")
	}
}

func TestStatusAndTelemetryShape(t *testing.T) {
	ctx := context.Background()
	box, dev := handshaken(t)
	st, err := dev.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(st)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"connected": true, "port": "fake", "mode": 118.0, "levelA": 0.0, "levelB": 0.0, "power": "normal", "batteryPercent": 90.0, "adcOverride": false, "levelMA": 64.0, "maMin": 1.0, "maMax": 64.0, "maPercent": 100.0, "maPotOverride": false, "gateValue": 7.0, "modeRampValue": 255.0, "routineTimer": 4384.0, "sequenceProfiled": true, "sequenceVaries": true, "sequenceGated": false, "sweepValue": 125.0, "sweepMin": 50.0, "sweepMax": 200.0, "sweepStep": 2.0, "sweepPercent": 50.0, "sweepDirection": "rising", "freqValue": 68.0, "freqPercent": 50.0, "intensityValue": nil, "intensityDirection": nil}
	for k, v := range want {
		if got, ok := m[k]; !ok || got != v {
			t.Errorf("status[%q] = %v (%v), want %v", k, got, ok, v)
		}
	}
	if _, ok := m["error"]; ok {
		t.Error("error key present on a healthy status")
	}

	f, err := dev.Telemetry(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if *f.Mode != 0x76 || *f.LevelA != 0 || *f.LevelMA != 64 || *f.MAPercent != 100 || f.RoutineState == nil || *f.SweepPercent != 50 || f.IntensityValue != nil {
		t.Fatalf("frame = %+v", f)
	}
	// An uncharacterized pattern reads every block.
	box.Set(mk312.AddressCurrentMode, 0x88)
	st, err = dev.Status(ctx)
	if err != nil || st.SequenceProfiled || st.IntensityValue == nil || *st.IntensityValue != 200 || *st.IntensityPercent != 50 {
		t.Fatalf("unprofiled status = %+v, %v", st, err)
	}
	// Pattern 0x7A modulates nothing.
	box.Set(mk312.AddressCurrentMode, 0x7A)
	if st, err = dev.Status(ctx); err != nil || st.SequenceVaries || st.SweepValue != nil {
		t.Fatalf("constant pattern status = %+v, %v", st, err)
	}
}

func TestRoutineTimerRereadsAcrossAWrap(t *testing.T) {
	ctx := context.Background()
	box, dev := handshaken(t)
	box.Set(mk312.AddressRoutineTimerHigh, 0x11)
	box.Set(mk312.AddressRoutineTimerLow, 0xFF)
	v, err := dev.RoutineTimer(ctx)
	if err != nil || v != 0x11FF {
		t.Fatalf("timer = %#x, %v", v, err)
	}
}

func TestReleaseReportsEveryFailure(t *testing.T) {
	ctx := context.Background()
	box, dev := handshaken(t)
	if err := dev.SetLevelA(ctx, 20); err != nil {
		t.Fatal(err)
	}
	if err := dev.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if box.LevelA() != 0 || box.ADCDisabled() || box.MAPotDisabled() || box.Power() != mk312.PowerNormal {
		t.Fatal("release did not restore the panel")
	}
	// A device that stops answering mid-release: every step is still tried
	// and the error names them all.
	box.SetDead(true)
	err := dev.Release(ctx)
	if err == nil || !mk312.IsProtocolError(err) {
		t.Fatalf("release on a dead box: %v", err)
	}
	for _, want := range []string{"Channel A zero", "Channel B zero", "power"} {
		if !containsStr(err.Error(), want) {
			t.Errorf("release error lacks %q: %v", want, err)
		}
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestFileKeyStore(t *testing.T) {
	s := mk312.FileKeyStore{Path: filepath.Join(t.TempDir(), "key")}
	if _, ok, err := s.Load(); ok || err != nil {
		t.Fatalf("empty store: %v %v", ok, err)
	}
	if err := s.Save(0x17); err != nil {
		t.Fatal(err)
	}
	k, ok, err := s.Load()
	if !ok || err != nil || k != 0x17 {
		t.Fatalf("load = %d %v %v", k, ok, err)
	}
	if err := s.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Load(); ok {
		t.Fatal("key survived clear")
	}
	if err := s.Clear(); err != nil {
		t.Fatal("clearing twice should be fine")
	}
}

type memStore struct {
	key    int
	ok     bool
	saves  int
	clears int
}

func (m *memStore) Load() (int, bool, error) { return m.key, m.ok, nil }
func (m *memStore) Save(k int) error         { m.key, m.ok = k, true; m.saves++; return nil }
func (m *memStore) Clear() error             { m.ok = false; m.clears++; return nil }

func TestConnectResumesOrHandshakes(t *testing.T) {
	ctx := context.Background()
	box := fakebox.New()
	store := &memStore{}
	drv, err := mk312.Connect(ctx, fast(mk312.New(box, "fake")), store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if store.saves != 1 || !store.ok || store.key != drv.Key() {
		t.Fatalf("store after handshake = %+v", store)
	}
	// A restart resumes with the stored key and handshakes nothing.
	writesBefore := len(box.Writes())
	drv2, err := mk312.Connect(ctx, fast(mk312.New(box, "fake")), store, nil)
	if err != nil || drv2.Key() != drv.Key() || store.saves != 1 {
		t.Fatalf("resume: %v (saves %d)", err, store.saves)
	}
	if len(box.Writes()) != writesBefore {
		t.Fatal("resume wrote to the box")
	}
	// After a power cycle the stored key is rejected, cleared, and a fresh
	// handshake follows.
	box.PowerCycle()
	drv3, err := mk312.Connect(ctx, fast(mk312.New(box, "fake")), store, nil)
	if err != nil || store.clears != 1 || store.saves != 2 || drv3.Key() != box.Key() {
		t.Fatalf("after power cycle: %v store=%+v", err, store)
	}
	// Closing a keyed driver unkeys the box and clears the store.
	if err := drv3.Close(ctx, true); err != nil {
		t.Fatal(err)
	}
	if store.ok || box.Key() != mk312.NoKey || !box.Closed() {
		t.Fatalf("after close: store=%+v boxKey=%d closed=%v", store, box.Key(), box.Closed())
	}
}

func TestDriverExecuteAndCaps(t *testing.T) {
	ctx := context.Background()
	box := fakebox.New()
	drv, err := mk312.Connect(ctx, fast(mk312.New(box, "fake")), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if drv.Kind() != estim.KindMK312BT || drv.Label() != mk312.Label || drv.Port() != "fake" {
		t.Fatal("driver identity")
	}
	caps := drv.Capabilities()
	if caps.LevelMax != 85 || len(caps.Modes) != 11 || !caps.Tempo || len(caps.Channels) != 1 || caps.Channels[0] != "a" {
		t.Fatalf("caps = %+v", caps)
	}
	if err := drv.Arm(ctx); err != nil || box.Power() != mk312.PowerHigh {
		t.Fatalf("arm: %v power=%d", err, box.Power())
	}
	box.Set(mk312.AddressLevelB, 30)
	res, err := drv.Execute(ctx, estim.Command{Verb: "set_level", Level: estim.Int(10)}, nil)
	if err != nil || *res.Level != 10 || box.LevelA() != 10 || box.LevelB() != 0 {
		t.Fatalf("set_level: %+v, %v (A=%d B=%d)", res, err, box.LevelA(), box.LevelB())
	}
	res, err = drv.Execute(ctx, estim.Command{Verb: "adjust_level", Delta: estim.Int(-3)}, nil)
	if err != nil || *res.PreviousLevel != 10 || *res.Level != 7 {
		t.Fatalf("adjust_level: %+v, %v", res, err)
	}
	res, err = drv.Execute(ctx, estim.Command{Verb: "set_ma", Percent: estim.Int(0)}, nil)
	if err != nil || *res.LevelMA != 1 {
		t.Fatalf("set_ma: %+v, %v", res, err)
	}
	res, err = drv.Execute(ctx, estim.Command{Verb: "adjust_ma", Delta: estim.Int(10)}, nil)
	if err != nil || *res.PreviousPercent != 0 || *res.Percent != 10 {
		t.Fatalf("adjust_ma: %+v, %v", res, err)
	}
	res, err = drv.Execute(ctx, estim.Command{Verb: "set_mode", Mode: estim.Int(0x7B)}, nil)
	if err != nil || *res.Mode != 0x7B || box.Mode() != 0x7B {
		t.Fatalf("set_mode: %+v, %v", res, err)
	}
	for _, bad := range []estim.Command{
		{Verb: "set_level", Level: estim.Int(86)},
		{Verb: "set_level"},
		{Verb: "adjust_level", Delta: estim.Int(6)},
		{Verb: "adjust_level", Delta: estim.Int(0)},
		{Verb: "set_ma", Percent: estim.Int(101)},
		{Verb: "adjust_ma", Delta: estim.Int(11)},
		{Verb: "set_mode", Mode: estim.Int(0x7C)},
		{Verb: "set_mode"},
		{Verb: "status"},
		{Verb: "release"},
		{Verb: "bogus"},
	} {
		if _, err := drv.Execute(ctx, bad, nil); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	// adjust_level clamps at the cap, never above it.
	box.Set(mk312.AddressLevelA, mk312.LCDToRAM(84))
	if res, err := drv.Execute(ctx, estim.Command{Verb: "adjust_level", Delta: estim.Int(5)}, nil); err != nil || *res.Level != 85 {
		t.Fatalf("adjust to cap: %+v, %v", res, err)
	}
}

func TestProbePort(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	ftdi := serialport.Candidate{Path: "fake", VendorID: serialport.FTDIVendor, ProductID: 0x6001}
	opts := mk312.ProbeOptions{Listen: 30 * time.Millisecond, Tune: func(d *mk312.Device) { fast(d) }}

	// A beaconing box is found without a stored key, and a stale stored key
	// is forgotten first.
	box := fakebox.New()
	store := &memStore{key: 0x33, ok: true}
	drv, err := mk312.ProbePort(ctx, box, ftdi, store, log, opts)
	if err != nil || drv == nil || store.clears != 1 || drv.Key() != box.Key() {
		t.Fatalf("beaconing box: %v store=%+v", err, store)
	}

	// A keyed box that is still ours resumes without a beacon.
	quiet := fakebox.New()
	first, err := mk312.Connect(ctx, fast(mk312.New(quiet, "fake")), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	store = &memStore{key: first.Key(), ok: true}
	drv, err = mk312.ProbePort(ctx, quiet, ftdi, store, log, opts)
	if err != nil || drv.Key() != first.Key() {
		t.Fatalf("resume through probe: %v", err)
	}

	// A keyed box nobody has the key to is diagnosed, not mistaken for an
	// empty port.
	lost := fakebox.New()
	if _, err := mk312.Connect(ctx, fast(mk312.New(lost, "fake")), nil, nil); err != nil {
		t.Fatal(err)
	}
	_, err = mk312.ProbePort(ctx, lost, ftdi, &memStore{}, log, opts)
	if !errors.Is(err, mk312.ErrKeyed) || !lost.Closed() {
		t.Fatalf("keyed box: %v (closed %v)", err, lost.Closed())
	}

	// An adapter with nothing on it: no device, and nothing was written to a
	// port that is not one the device ships with.
	other := fakebox.New()
	other.SetDead(true)
	_, err = mk312.ProbePort(ctx, other, serialport.Candidate{Path: "/dev/cu.Bluetooth", VendorID: 0x1234}, &memStore{}, log, opts)
	if !errors.Is(err, estim.ErrNoDevice) || len(other.Writes()) != 0 {
		t.Fatalf("foreign port: %v", err)
	}
	empty := fakebox.New()
	empty.SetDead(true)
	_, err = mk312.ProbePort(ctx, empty, ftdi, &memStore{}, log, opts)
	if !errors.Is(err, estim.ErrNoDevice) || !empty.Closed() {
		t.Fatalf("empty ftdi port: %v", err)
	}
}
