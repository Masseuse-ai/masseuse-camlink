package mk312

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/estim"
	"github.com/FemLed/masseuse-camlink/internal/serialport"
)

// Timings, measured on the device.
const (
	// Baud is the link speed.
	Baud = 19200
	// ReadTimeout bounds one reply.
	ReadTimeout = 500 * time.Millisecond
	// HandshakeReplyTimeout bounds the key exchange reply, which arrives
	// buried in a backlog of beacon bytes.
	HandshakeReplyTimeout = 2 * time.Second
	// HandshakeTries is how many sync bytes are sent before giving up on
	// hearing a beacon.
	HandshakeTries = 12
	// PowerSettle follows each step of a power range change.
	PowerSettle = 150 * time.Millisecond
	// MASettle follows a tempo write before it is read back.
	MASettle = 300 * time.Millisecond
	// MAWriteTolerance is how far the device may round a tempo write.
	MAWriteTolerance = 4
	// ModeLoadSettle lets a freshly loaded pattern publish its tempo range.
	ModeLoadSettle = 300 * time.Millisecond
	// RampInterval is one front-panel step per quarter second: a ramp from 0
	// to 85 takes about 21 s on the bus.
	RampInterval = 250 * time.Millisecond
	// cancelSlice is how often a ramp's wait looks at the cancellation latch.
	cancelSlice = 50 * time.Millisecond
)

// Device is one connection to an MK-312BT over a serial port. Methods are
// not safe for concurrent use; the estim.Runtime serializes them.
type Device struct {
	port serialport.Port
	path string
	key  int

	// Sleep waits out a protocol settle delay; tests shorten it. Timing
	// loops (ramps) do not use it.
	Sleep func(ctx context.Context, d time.Duration) error
	// Timeout bounds one reply (ReadTimeout).
	Timeout time.Duration
	// Ramp is the interval between ramp steps (RampInterval).
	Ramp time.Duration
	// SyncTries is how many sync bytes Handshake sends while listening for
	// a beacon (HandshakeTries). A probe that has already listened uses
	// fewer.
	SyncTries int
	// KeyExchangeTimeout bounds the key exchange reply
	// (HandshakeReplyTimeout).
	KeyExchangeTimeout time.Duration

	maCache *maRange
}

type maRange struct{ mode, lo, hi int }

// New wraps an open port. Nothing is sent until Handshake or Resume.
func New(port serialport.Port, path string) *Device {
	return &Device{port: port, path: path, key: NoKey, Sleep: sleepCtx, Timeout: ReadTimeout, Ramp: RampInterval, SyncTries: HandshakeTries, KeyExchangeTimeout: HandshakeReplyTimeout}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Path is the serial port the device is on.
func (d *Device) Path() string { return d.path }

// Key is the session key, or NoKey.
func (d *Device) Key() int { return d.key }

// -- raw transport ---------------------------------------------------------

func (d *Device) drain() { _ = d.port.ResetInput() }

func (d *Device) write(ctx context.Context, b []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := d.port.Write(b); err != nil {
		return fmt.Errorf("mk312: write: %w", err)
	}
	return nil
}

// read returns up to n bytes, short when the timeout runs out first, the
// way a desktop serial library does.
func (d *Device) read(ctx context.Context, n int, timeout time.Duration) ([]byte, error) {
	buf := make([]byte, n)
	got := 0
	deadline := time.Now().Add(timeout)
	for got < n {
		if err := ctx.Err(); err != nil {
			return buf[:got], err
		}
		k, err := d.port.Read(buf[got:])
		got += k
		if err != nil {
			return buf[:got], fmt.Errorf("mk312: read: %w", err)
		}
		if got >= n {
			break
		}
		if k == 0 && !time.Now().Before(deadline) {
			break
		}
	}
	return buf[:got], nil
}

// readHandshakeReply reads a plaintext handshake reply out of the beacon.
// No reply opcode is 0x07, so leading beacon bytes are padding and are
// dropped; only safe during the handshake, where nothing is scrambled yet.
func (d *Device) readHandshakeReply(ctx context.Context, n int) ([]byte, error) {
	deadline := time.Now().Add(d.KeyExchangeTimeout)
	var reply []byte
	for len(reply) < n && time.Now().Before(deadline) {
		chunk, err := d.read(ctx, 1, d.Timeout)
		if err != nil {
			return reply, err
		}
		if len(chunk) == 0 {
			continue
		}
		if len(reply) == 0 && chunk[0] == BeaconByte {
			continue
		}
		reply = append(reply, chunk...)
	}
	return reply, nil
}

// -- session -------------------------------------------------------------

// Handshake syncs with a device that is beaconing and agrees a session key.
func (d *Device) Handshake(ctx context.Context) error {
	synced := false
	for i := 0; i < max(1, d.SyncTries) && !synced; i++ {
		if err := d.write(ctx, []byte{0x00}); err != nil {
			return err
		}
		b, err := d.read(ctx, 1, d.Timeout)
		if err != nil {
			return err
		}
		synced = len(b) == 1 && b[0] == BeaconByte
	}
	if !synced {
		return ErrNoBeacon
	}
	// Drop the beacon backlog so the set-key reply is not read as a run of
	// stacked 0x07 bytes.
	d.drain()
	if err := d.write(ctx, EncodeSetKey(0, NoKey)); err != nil {
		return err
	}
	reply, err := d.readHandshakeReply(ctx, 3)
	if err != nil {
		return err
	}
	if len(reply) != 3 {
		return ErrNoKeyExchange
	}
	boxKey, err := DecodeReply(reply, NoKey, ReplySetKey)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrHandshake, err)
	}
	d.key = int(BoxKeyXOR ^ boxKey)
	return nil
}

// Resume adopts the key of a previous run instead of handshaking again: the
// device keeps scrambling with its key until it is reset, so a connector
// that restarted mid-session can only talk to it with the key it had. The
// key is proven by a read that must decode and checksum under it. Silence
// is ErrSilent (the key may still be right); a reply that does not decode
// is a ProtocolError (the device was power-cycled since).
func (d *Device) Resume(ctx context.Context, key int) error {
	if key < 0 || key > 0xFF {
		return errors.New("mk312: session key must be a byte")
	}
	d.drain()
	d.key = key
	err := func() error {
		if err := d.write(ctx, EncodeRead(AddressCurrentMode, key)); err != nil {
			return err
		}
		raw, err := d.read(ctx, 3, d.Timeout)
		if err != nil {
			return err
		}
		if len(raw) == 0 {
			// An unkeyed device would at least be beaconing into this read.
			return ErrSilent
		}
		mode, err := d.decodeRead(raw)
		if err != nil {
			return err
		}
		if !KnownMode(int(mode)) {
			return protocolErr("resumed key did not decode a known pattern")
		}
		return nil
	}()
	if err != nil {
		d.key = NoKey
	}
	return err
}

// Close leaves the device unkeyed (and, with restore, released) so a fresh
// handshake works next time. The key is forgotten whatever happens.
func (d *Device) Close(ctx context.Context, restore bool) error {
	defer func() { d.key = NoKey }()
	if d.key == NoKey {
		return nil
	}
	var errs []error
	if restore {
		if err := d.Release(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if err := d.Poke(ctx, AddressKey, 0); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Peek reads the byte at addr.
func (d *Device) Peek(ctx context.Context, addr uint16) (byte, error) {
	if err := d.write(ctx, EncodeRead(addr, d.key)); err != nil {
		return 0, err
	}
	raw, err := d.read(ctx, 3, d.Timeout)
	if err != nil {
		return 0, err
	}
	if len(raw) == 0 {
		return 0, ErrSilent
	}
	return d.decodeRead(raw)
}

// decodeRead accepts a read reply in the clear (what the device sends) or
// under the session key.
func (d *Device) decodeRead(raw []byte) (byte, error) {
	v, err := DecodeReply(raw, NoKey, ReplyRead)
	if err == nil || d.key == NoKey {
		return v, err
	}
	return DecodeReply(raw, d.key, ReplyRead)
}

// Poke writes value at addr and waits for the acknowledgment.
func (d *Device) Poke(ctx context.Context, addr uint16, value byte) error {
	if err := d.write(ctx, EncodeWrite(addr, value, d.key)); err != nil {
		return err
	}
	ack, err := d.read(ctx, 1, d.Timeout)
	if err != nil {
		return err
	}
	if len(ack) == 0 {
		return ErrSilent
	}
	if ack[0] != AckWrite && (d.key == NoKey || ack[0] != AckWrite^byte(d.key)) {
		return protocolErr("bad ACK writing 0x%04x: %02x", addr, ack[0])
	}
	return nil
}

// -- register-level helpers ---------------------------------------------

func (d *Device) r15Bit(ctx context.Context, bit byte) (bool, error) {
	r, err := d.Peek(ctx, AddressR15)
	return r&bit != 0, err
}

func (d *Device) setR15Bit(ctx context.Context, bit byte, on bool) error {
	r, err := d.Peek(ctx, AddressR15)
	if err != nil {
		return err
	}
	if on {
		r |= bit
	} else {
		r &^= bit
	}
	return d.Poke(ctx, AddressR15, r)
}

// ADCDisabled says whether the front-panel level knobs are overridden.
func (d *Device) ADCDisabled(ctx context.Context) (bool, error) {
	return d.r15Bit(ctx, R15ADCDisable)
}

// MAPotDisabled says whether the front-panel tempo knob is overridden.
func (d *Device) MAPotDisabled(ctx context.Context) (bool, error) {
	return d.r15Bit(ctx, R15MAPotDisable)
}

// -- bounded controls -----------------------------------------------------

// SetMode loads pattern mode; the caller has checked it is allowed.
func (d *Device) SetMode(ctx context.Context, mode int) error {
	if !KnownMode(mode) {
		return fmt.Errorf("mk312: unknown pattern %d", mode)
	}
	cur, err := d.Peek(ctx, AddressCurrentMode)
	if err != nil {
		return err
	}
	if int(cur) == mode {
		return nil
	}
	steps := []struct {
		addr  uint16
		value byte
	}{{AddressCurrentMode, byte(mode)}, {AddressCommand, CommandExitMenu}, {AddressCommand, CommandNewMode}}
	for _, s := range steps {
		if err := d.Poke(ctx, s.addr, s.value); err != nil {
			return err
		}
		if err := d.Sleep(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
	applied, err := d.Peek(ctx, AddressCurrentMode)
	if err != nil {
		return err
	}
	if int(applied) != mode {
		return protocolErr("pattern write did not stick")
	}
	// Let the freshly loaded pattern publish its tempo range before a
	// following SetLevelMA reads it.
	return d.Sleep(ctx, ModeLoadSettle)
}

// PowerLevel reads the power range (PowerLow, PowerNormal, PowerHigh).
func (d *Device) PowerLevel(ctx context.Context) (int, error) {
	v, err := d.Peek(ctx, AddressPowerLevel)
	return int(v), err
}

// SetPowerLevel selects a power range, zeroing both channels first: the
// same level number is much stronger in the high range.
func (d *Device) SetPowerLevel(ctx context.Context, level int) error {
	param, ok := powerParam[level]
	if !ok {
		return fmt.Errorf("mk312: power range must be low, normal or high")
	}
	cur, err := d.PowerLevel(ctx)
	if err != nil {
		return err
	}
	if cur == level {
		return nil
	}
	for _, w := range []struct {
		addr  uint16
		value byte
	}{{AddressLevelA, 0}, {AddressLevelB, 0}, {AddressPowerParam, param}} {
		if err := d.Poke(ctx, w.addr, w.value); err != nil {
			return err
		}
	}
	if err := d.Sleep(ctx, PowerSettle); err != nil {
		return err
	}
	if err := d.Poke(ctx, AddressCommand, CommandSetPower); err != nil {
		return err
	}
	if err := d.Sleep(ctx, PowerSettle); err != nil {
		return err
	}
	applied, err := d.PowerLevel(ctx)
	if err != nil {
		return err
	}
	if applied != level {
		return protocolErr("power range write did not stick: %s != %s", PowerName(applied), PowerName(level))
	}
	return nil
}

// LevelA reads Channel A's level on the front-panel scale.
func (d *Device) LevelA(ctx context.Context) (int, error) {
	v, err := d.Peek(ctx, AddressLevelA)
	return RAMToLCD(int(v)), err
}

// SetLevelA sets Channel A (0..99), with Channel B pinned at zero and the
// level knobs overridden.
func (d *Device) SetLevelA(ctx context.Context, level int) error {
	if level < 0 || level > LCDMax {
		return fmt.Errorf("mk312: Channel A level must be 0..%d", LCDMax)
	}
	if err := d.Poke(ctx, AddressLevelB, 0); err != nil {
		return err
	}
	if err := d.ensureR15(ctx, R15ADCDisable); err != nil {
		return err
	}
	if err := d.Poke(ctx, AddressLevelA, LCDToRAM(level)); err != nil {
		return err
	}
	applied, err := d.LevelA(ctx)
	if err != nil {
		return err
	}
	if applied != level {
		return protocolErr("Channel A level write did not stick: %d != %d", applied, level)
	}
	return nil
}

func (d *Device) ensureR15(ctx context.Context, bit byte) error {
	set, err := d.r15Bit(ctx, bit)
	if err != nil || set {
		return err
	}
	return d.setR15Bit(ctx, bit, true)
}

// LevelMA reads the tempo byte.
func (d *Device) LevelMA(ctx context.Context) (int, error) {
	v, err := d.Peek(ctx, AddressLevelMA)
	return int(v), err
}

// MARange reads the loaded pattern's tempo bounds.
func (d *Device) MARange(ctx context.Context) (lo, hi int, err error) {
	l, err := d.Peek(ctx, AddressMAMin)
	if err != nil {
		return 0, 0, err
	}
	h, err := d.Peek(ctx, AddressMAMax)
	return int(l), int(h), err
}

// maRangeCached is MARange for the telemetry loop: the bounds are fixed for
// the life of a loaded pattern, so caching saves two round trips a frame.
func (d *Device) maRangeCached(ctx context.Context, mode int) (int, int, error) {
	if c := d.maCache; c != nil && c.mode == mode {
		return c.lo, c.hi, nil
	}
	lo, hi, err := d.MARange(ctx)
	if err != nil {
		return 0, 0, err
	}
	d.maCache = &maRange{mode, lo, hi}
	return lo, hi, nil
}

// SetLevelMA sets the tempo as a percent of the loaded pattern's range,
// overriding the knob, and returns the byte written.
func (d *Device) SetLevelMA(ctx context.Context, percent int) (int, error) {
	if percent < 0 || percent > MAPercentCap {
		return 0, fmt.Errorf("mk312: tempo must be 0..%d", MAPercentCap)
	}
	mode, err := d.Peek(ctx, AddressCurrentMode)
	if err != nil {
		return 0, err
	}
	if tempoInert[int(mode)] {
		return 0, fmt.Errorf("mk312: tempo has no effect in pattern %d", mode)
	}
	lo, hi, err := d.MARange(ctx)
	if err != nil {
		return 0, err
	}
	raw, err := MAPercentToRaw(percent, lo, hi)
	if err != nil {
		return 0, err
	}
	if err := d.ensureR15(ctx, R15ADCDisable); err != nil {
		return 0, err
	}
	if err := d.ensureR15(ctx, R15MAPotDisable); err != nil {
		return 0, err
	}
	if err := d.Poke(ctx, AddressLevelMA, byte(raw)); err != nil {
		return 0, err
	}
	if err := d.Sleep(ctx, MASettle); err != nil {
		return 0, err
	}
	applied, err := d.LevelMA(ctx)
	if err != nil {
		return 0, err
	}
	if diff := applied - raw; diff > MAWriteTolerance || diff < -MAWriteTolerance {
		return 0, protocolErr("tempo write did not stick: %d != %d", applied, raw)
	}
	return raw, nil
}

// RoutineTimer reads the device's free-running 16-bit pattern clock. The
// high byte advances about once a second, so the low byte wraps often
// enough that a naive two-byte read would occasionally splice a pre-wrap
// low onto a post-wrap high; the high byte is re-read and, if it moved, a
// fresh low taken to go with it.
func (d *Device) RoutineTimer(ctx context.Context) (int, error) {
	high, err := d.Peek(ctx, AddressRoutineTimerHigh)
	if err != nil {
		return 0, err
	}
	low, err := d.Peek(ctx, AddressRoutineTimerLow)
	if err != nil {
		return 0, err
	}
	confirm, err := d.Peek(ctx, AddressRoutineTimerHigh)
	if err != nil {
		return 0, err
	}
	if confirm != high {
		high = confirm
		if low, err = d.Peek(ctx, AddressRoutineTimerLow); err != nil {
			return 0, err
		}
	}
	return int(high)<<8 | int(low), nil
}

// Which modulation blocks each characterized pattern drives; reading a
// block a pattern has pinned only costs round trips. Uncharacterized
// patterns read every block.
type profile struct{ width, freq, intensity, gated bool }

var profiles = map[int]profile{
	0x76: {width: true, freq: true},      // pulse width and frequency sweep together
	0x83: {width: true},                  // pulse width sweeps while its floor creeps upward
	0x84: {intensity: true, gated: true}, // intensity ramps in irregular gated bursts
	0x77: {intensity: true, gated: true}, // intensity sweeps on a short gated cycle
	0x7A: {},                             // nothing modulates: constant output
}

var everyBlock = profile{width: true, freq: true, intensity: true}

type block struct{ value, lo, hi, step uint16 }

var (
	widthBlock     = block{AddressSweepValueA, AddressSweepMinA, AddressSweepMaxA, AddressSweepStepA}
	freqBlock      = block{AddressFreqValueA, AddressFreqMinA, AddressFreqMaxA, AddressFreqStepA}
	intensityBlock = block{AddressIntensityValueA, AddressIntensityMinA, AddressIntensityMaxA, AddressIntensityStepA}
)

func (d *Device) readBlock(ctx context.Context, b block) (value, lo, hi, step *int, percent *int, direction *string, err error) {
	var raw [4]byte
	for i, addr := range []uint16{b.value, b.lo, b.hi, b.step} {
		if raw[i], err = d.Peek(ctx, addr); err != nil {
			return nil, nil, nil, nil, nil, nil, err
		}
	}
	v, l, h, s := int(raw[0]), int(raw[1]), int(raw[2]), SignedByte(raw[3])
	if p, ok := PhasePercent(v, l, h); ok {
		percent = estim.Int(p)
	}
	return estim.Int(v), estim.Int(l), estim.Int(h), estim.Int(s), percent, estim.String(SweepDirection(s)), nil
}

// RoutineState reads where the loaded pattern sits in its modulation. The
// bounds are read every time rather than cached: some patterns move their
// own floor while they run, and the tempo retunes bounds in others.
func (d *Device) RoutineState(ctx context.Context, mode int) (*estim.RoutineState, error) {
	p, profiled := profiles[mode]
	if !profiled {
		p = everyBlock
	}
	st := &estim.RoutineState{SequenceProfiled: profiled, SequenceVaries: p.width || p.freq || p.intensity, SequenceGated: p.gated}
	var err error
	var gate, ramp byte
	if gate, err = d.Peek(ctx, AddressGateValueA); err != nil {
		return nil, err
	}
	if ramp, err = d.Peek(ctx, AddressModeRampValueA); err != nil {
		return nil, err
	}
	st.GateValue, st.ModeRampValue = int(gate), int(ramp)
	if st.RoutineTimer, err = d.RoutineTimer(ctx); err != nil {
		return nil, err
	}
	if p.width {
		if st.SweepValue, st.SweepMin, st.SweepMax, st.SweepStep, st.SweepPercent, st.SweepDirection, err = d.readBlock(ctx, widthBlock); err != nil {
			return nil, err
		}
	}
	if p.freq {
		if st.FreqValue, st.FreqMin, st.FreqMax, st.FreqStep, st.FreqPercent, st.FreqDirection, err = d.readBlock(ctx, freqBlock); err != nil {
			return nil, err
		}
	}
	if p.intensity {
		if st.IntensityValue, st.IntensityMin, st.IntensityMax, st.IntensityStep, st.IntensityPercent, st.IntensityDirection, err = d.readBlock(ctx, intensityBlock); err != nil {
			return nil, err
		}
	}
	return st, nil
}

// Telemetry is a compact pattern sample: about a third of Status, skipping
// power, battery and the override bits, none of which move without a
// command that refreshes the status anyway.
func (d *Device) Telemetry(ctx context.Context) (estim.Frame, error) {
	mode, err := d.Peek(ctx, AddressCurrentMode)
	if err != nil {
		return estim.Frame{}, err
	}
	lo, hi, err := d.maRangeCached(ctx, int(mode))
	if err != nil {
		return estim.Frame{}, err
	}
	maRaw, err := d.LevelMA(ctx)
	if err != nil {
		return estim.Frame{}, err
	}
	levelA, err := d.LevelA(ctx)
	if err != nil {
		return estim.Frame{}, err
	}
	f := estim.Frame{Mode: estim.Int(int(mode)), LevelA: estim.Int(levelA), LevelMA: estim.Int(maRaw)}
	if p, ok := MARawToPercent(maRaw, lo, hi); ok {
		f.MAPercent = estim.Int(p)
	}
	if f.RoutineState, err = d.RoutineState(ctx, int(mode)); err != nil {
		return estim.Frame{}, err
	}
	return f, nil
}

// waitOrCancel sleeps in short slices so a release can preempt a ramp step
// promptly; true when cancelled.
func (d *Device) waitOrCancel(ctx context.Context, wait time.Duration, cancelled func() bool) (bool, error) {
	for remaining := wait; remaining > 0; remaining -= cancelSlice {
		if cancelled != nil && cancelled() {
			return true, nil
		}
		if err := sleepCtx(ctx, min(cancelSlice, remaining)); err != nil {
			return false, err
		}
	}
	return cancelled != nil && cancelled(), nil
}

// RampLevelA walks Channel A to target one front-panel step at a time,
// reading each step back, and stops with estim.ErrCancelled when the latch
// is set between steps.
func (d *Device) RampLevelA(ctx context.Context, target int, cancelled func() bool) (int, error) {
	if target < 0 || target > LCDMax {
		return 0, fmt.Errorf("mk312: target must be 0..%d", LCDMax)
	}
	current, err := d.LevelA(ctx)
	if err != nil {
		return 0, err
	}
	if current == target {
		return target, d.SetLevelA(ctx, target)
	}
	dir := 1
	if target < current {
		dir = -1
	}
	for value := current; value != target; {
		if cancelled != nil && cancelled() {
			return value, estim.ErrCancelled
		}
		value += dir
		if err := d.SetLevelA(ctx, value); err != nil {
			return value, err
		}
		if value != target {
			c, err := d.waitOrCancel(ctx, d.Ramp, cancelled)
			if err != nil {
				return value, err
			}
			if c {
				return value, estim.ErrCancelled
			}
		}
	}
	return target, nil
}

// Release fails closed: both channels to zero, the level and tempo knobs
// live again, the power range back to normal, each step attempted whatever
// happened to the ones before, then everything read back.
func (d *Device) Release(ctx context.Context) error {
	var errs []string
	steps := []struct {
		label string
		op    func() error
	}{
		{"Channel A zero", func() error { return d.Poke(ctx, AddressLevelA, 0) }},
		{"Channel B zero", func() error { return d.Poke(ctx, AddressLevelB, 0) }},
		{"ADC enable", func() error { return d.setR15Bit(ctx, R15ADCDisable, false) }},
		{"MA pot enable", func() error { return d.setR15Bit(ctx, R15MAPotDisable, false) }},
		{"Normal power restore", func() error { return d.SetPowerLevel(ctx, PowerNormal) }},
	}
	for _, s := range steps {
		if err := s.op(); err != nil {
			errs = append(errs, s.label+": "+err.Error())
		}
	}
	checks := []struct {
		label string
		ok    func() (bool, error)
	}{
		{"Channel A zero readback", func() (bool, error) { v, err := d.Peek(ctx, AddressLevelA); return v == 0, err }},
		{"Channel B zero readback", func() (bool, error) { v, err := d.Peek(ctx, AddressLevelB); return v == 0, err }},
		{"ADC remained disabled", func() (bool, error) { v, err := d.ADCDisabled(ctx); return !v, err }},
		{"MA pot remained disabled", func() (bool, error) { v, err := d.MAPotDisabled(ctx); return !v, err }},
		{"power not restored to Normal", func() (bool, error) { v, err := d.PowerLevel(ctx); return v == PowerNormal, err }},
	}
	for _, c := range checks {
		ok, err := c.ok()
		switch {
		case err != nil:
			errs = append(errs, c.label+": "+err.Error())
		case !ok:
			errs = append(errs, c.label)
		}
	}
	if len(errs) > 0 {
		return protocolErr("release incomplete: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Status reads everything the service's safety check wants.
func (d *Device) Status(ctx context.Context) (estim.Status, error) {
	var vals [6]byte
	for i, addr := range []uint16{AddressCurrentMode, AddressPowerLevel, AddressBattery, AddressR15, AddressMAMin, AddressMAMax} {
		var err error
		if vals[i], err = d.Peek(ctx, addr); err != nil {
			return estim.Status{}, err
		}
	}
	mode, power, battery, r15, lo, hi := int(vals[0]), int(vals[1]), vals[2], vals[3], int(vals[4]), int(vals[5])
	maRaw, err := d.LevelMA(ctx)
	if err != nil {
		return estim.Status{}, err
	}
	levelA, err := d.LevelA(ctx)
	if err != nil {
		return estim.Status{}, err
	}
	rawB, err := d.Peek(ctx, AddressLevelB)
	if err != nil {
		return estim.Status{}, err
	}
	st := estim.Status{
		Connected:      true,
		Port:           estim.String(d.path),
		Mode:           estim.Int(mode),
		LevelA:         estim.Int(levelA),
		LevelB:         estim.Int(RAMToLCD(int(rawB))),
		Power:          estim.String(PowerName(power)),
		BatteryPercent: estim.Int(BatteryPercent(battery)),
		ADCOverride:    estim.Bool(r15&R15ADCDisable != 0),
		LevelMA:        estim.Int(maRaw),
		MAMin:          estim.Int(lo),
		MAMax:          estim.Int(hi),
		MAPotOverride:  estim.Bool(r15&R15MAPotDisable != 0),
	}
	if p, ok := MARawToPercent(maRaw, lo, hi); ok {
		st.MAPercent = estim.Int(p)
	}
	if st.RoutineState, err = d.RoutineState(ctx, mode); err != nil {
		return estim.Status{}, err
	}
	return st, nil
}
