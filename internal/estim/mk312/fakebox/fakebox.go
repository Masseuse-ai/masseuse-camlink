// Package fakebox is a wire-level MK-312BT behind a serialport.Port for
// tests: plaintext until a key exchange, XOR-scrambled host bytes after,
// plaintext replies, a 0x07 beacon while unkeyed and idle, and the power
// range command's side effect on the power register.
package fakebox

import (
	"errors"
	"sync"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/estim/mk312"
)

// Values measured on a device running pattern 0x76: pulse width [50,200],
// frequency [9,128]; the intensity block as pattern 0x84 leaves it.
var defaultRegisters = map[uint16]byte{
	mk312.AddressLevelA:           0,
	mk312.AddressLevelB:           0,
	mk312.AddressR15:              0,
	mk312.AddressCurrentMode:      0x76,
	mk312.AddressMAMin:            1,
	mk312.AddressMAMax:            64,
	mk312.AddressLevelMA:          64,
	mk312.AddressPowerLevel:       mk312.PowerNormal,
	mk312.AddressPowerParam:       0,
	mk312.AddressPulseWidth:       130,
	mk312.AddressBattery:          230,
	mk312.AddressSweepValueA:      125,
	mk312.AddressSweepMinA:        50,
	mk312.AddressSweepMaxA:        200,
	mk312.AddressSweepStepA:       2,
	mk312.AddressFreqValueA:       68,
	mk312.AddressFreqMinA:         9,
	mk312.AddressFreqMaxA:         128,
	mk312.AddressModeRampValueA:   255,
	mk312.AddressGateValueA:       7,
	mk312.AddressRoutineTimerLow:  0x20,
	mk312.AddressRoutineTimerHigh: 0x11,
	mk312.AddressIntensityValueA:  200,
	mk312.AddressIntensityMinA:    176,
	mk312.AddressIntensityMaxA:    224,
	mk312.AddressIntensityStepA:   1,
}

var powerParamToLevel = map[byte]byte{0x6B: mk312.PowerLow, 0x6C: mk312.PowerNormal, 0x6D: mk312.PowerHigh}

// ErrDetached is what a write to an unplugged adapter returns.
var ErrDetached = errors.New("fakebox: no such device")

// Write is one memory write the device received.
type Write struct {
	Addr  uint16
	Value byte
}

// Box is the fake device. Fields are read and written under its lock by
// the methods; tests set the exported knobs between operations.
type Box struct {
	mu sync.Mutex

	// BoxKey is the key the device offers at the key exchange.
	BoxKey byte
	// Beacon enables the idle beacon (on by default).
	Beacon bool
	// BeaconBurst is how many beacon bytes precede the key exchange reply.
	BeaconBurst int
	// BeaconInterval is how often the beacon byte is sent while idle.
	BeaconInterval time.Duration
	// Dead: powered off or link plug out. Bytes vanish, nothing comes back.
	Dead bool
	// Detached: the adapter itself is gone; writes fail.
	Detached bool
	// AnswerKeyExchange off makes the device beacon without ever keying.
	AnswerKeyExchange bool
	// AnswerReads off makes the device NAK reads while writes still land.
	AnswerReads bool
	// IdleWait is how long an idle Read waits before reporting no data.
	IdleWait time.Duration

	registers  map[uint16]byte
	key        int
	pending    []byte
	inbuf      []byte
	writes     []Write
	lastBeacon time.Time
	closed     bool
}

// New is a device at rest: pattern 0x76 loaded, outputs zero, normal power,
// unkeyed and beaconing.
func New() *Box {
	b := &Box{
		BoxKey:            0x42,
		Beacon:            true,
		BeaconBurst:       5,
		BeaconInterval:    20 * time.Millisecond,
		AnswerKeyExchange: true,
		AnswerReads:       true,
		IdleWait:          time.Millisecond,
		registers:         map[uint16]byte{},
		key:               mk312.NoKey,
	}
	for k, v := range defaultRegisters {
		b.registers[k] = v
	}
	b.lastBeacon = time.Now().Add(-b.BeaconInterval)
	return b
}

func (b *Box) beaconDue() bool {
	return b.Beacon && !b.Dead && !b.Detached && b.key == mk312.NoKey && time.Since(b.lastBeacon) >= b.BeaconInterval
}

// Read hands over what the device has sent, or the beacon when it is due.
// With nothing to say it waits IdleWait and returns 0, nil, as a serial
// port whose read timeout ran out does.
func (b *Box) Read(p []byte) (int, error) {
	b.mu.Lock()
	if b.closed || b.Detached {
		b.mu.Unlock()
		return 0, ErrDetached
	}
	if len(b.pending) > 0 {
		n := copy(p, b.pending)
		b.pending = b.pending[n:]
		b.mu.Unlock()
		return n, nil
	}
	if b.beaconDue() && len(p) > 0 {
		b.lastBeacon = time.Now()
		p[0] = mk312.BeaconByte
		b.mu.Unlock()
		return 1, nil
	}
	wait := b.IdleWait
	b.mu.Unlock()
	time.Sleep(wait)
	return 0, nil
}

// Write feeds host bytes to the device.
func (b *Box) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.Detached {
		return 0, ErrDetached
	}
	if b.Dead {
		return len(data), nil
	}
	b.inbuf = append(b.inbuf, data...)
	b.process()
	return len(data), nil
}

// Close marks the port closed.
func (b *Box) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return nil
}

// Closed says whether Close was called.
func (b *Box) Closed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

// Reopen undoes Close, as opening the serial port again would.
func (b *Box) Reopen() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = false
}

// ResetInput drops what the device has sent and not yet been read.
func (b *Box) ResetInput() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending = nil
	return nil
}

var packetLength = map[byte]int{0x00: 1, mk312.CmdSetKey: 3, mk312.CmdRead: 4, mk312.CmdWrite: 5}

func (b *Box) unscramble(v byte) byte {
	if b.key == mk312.NoKey {
		return v
	}
	return v ^ byte(b.key)
}

func (b *Box) process() {
	for len(b.inbuf) > 0 {
		opcode := b.unscramble(b.inbuf[0])
		length, ok := packetLength[opcode]
		if !ok {
			b.inbuf = b.inbuf[1:]
			b.pending = append(b.pending, mk312.BeaconByte)
			continue
		}
		if len(b.inbuf) < length {
			return
		}
		packet := make([]byte, length)
		for i := range packet {
			packet[i] = b.unscramble(b.inbuf[i])
		}
		b.inbuf = b.inbuf[length:]
		if opcode == 0x00 {
			continue
		}
		if mk312.Checksum(packet[:length-1]) != packet[length-1] {
			b.pending = append(b.pending, mk312.BeaconByte)
			continue
		}
		switch opcode {
		case mk312.CmdSetKey:
			if !b.AnswerKeyExchange {
				continue
			}
			for i := 0; i < b.BeaconBurst; i++ {
				b.pending = append(b.pending, mk312.BeaconByte)
			}
			reply := []byte{mk312.ReplySetKey, b.BoxKey}
			reply = append(reply, mk312.Checksum(reply))
			b.pending = append(b.pending, reply...)
			b.key = int(packet[1] ^ b.BoxKey ^ mk312.BoxKeyXOR)
		case mk312.CmdRead:
			if !b.AnswerReads {
				b.pending = append(b.pending, mk312.BeaconByte)
				continue
			}
			addr := uint16(packet[1])<<8 | uint16(packet[2])
			reply := []byte{mk312.ReplyRead, b.registers[addr]}
			reply = append(reply, mk312.Checksum(reply))
			b.pending = append(b.pending, reply...)
		case mk312.CmdWrite:
			addr := uint16(packet[1])<<8 | uint16(packet[2])
			b.applyWrite(addr, packet[3])
			b.pending = append(b.pending, mk312.AckWrite)
		}
	}
}

func (b *Box) applyWrite(addr uint16, value byte) {
	b.writes = append(b.writes, Write{addr, value})
	switch {
	case addr == mk312.AddressKey && value == 0:
		b.key = mk312.NoKey
	case addr == mk312.AddressCommand:
		if value == mk312.CommandSetPower {
			if level, ok := powerParamToLevel[b.registers[mk312.AddressPowerParam]]; ok {
				b.registers[mk312.AddressPowerLevel] = level
			}
		}
	default:
		b.registers[addr] = value
	}
}

// -- inspection for assertions ----------------------------------------------

// Register reads a memory byte directly.
func (b *Box) Register(addr uint16) byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.registers[addr]
}

// Set writes a memory byte directly, as the device's own firmware would.
func (b *Box) Set(addr uint16, value byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.registers[addr] = value
}

// Key is the device's current session key, or mk312.NoKey.
func (b *Box) Key() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.key
}

// Writes lists every memory write received so far.
func (b *Box) Writes() []Write {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Write(nil), b.writes...)
}

// LevelA is Channel A on the front-panel scale.
func (b *Box) LevelA() int { return mk312.RAMToLCD(int(b.Register(mk312.AddressLevelA))) }

// LevelB is Channel B on the front-panel scale.
func (b *Box) LevelB() int { return mk312.RAMToLCD(int(b.Register(mk312.AddressLevelB))) }

// Power is the power range register.
func (b *Box) Power() int { return int(b.Register(mk312.AddressPowerLevel)) }

// Mode is the loaded pattern number.
func (b *Box) Mode() int { return int(b.Register(mk312.AddressCurrentMode)) }

// ADCDisabled says whether the level knobs are overridden.
func (b *Box) ADCDisabled() bool { return b.Register(mk312.AddressR15)&mk312.R15ADCDisable != 0 }

// MAPotDisabled says whether the tempo knob is overridden.
func (b *Box) MAPotDisabled() bool { return b.Register(mk312.AddressR15)&mk312.R15MAPotDisable != 0 }

// SetDead switches the device off (or pulls its link plug).
func (b *Box) SetDead(dead bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.Dead = dead
}

// SetDetached unplugs (or replugs) the adapter.
func (b *Box) SetDetached(detached bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.Detached = detached
}

// PowerCycle forgets the session key and puts the outputs at rest, as a
// device switched off and on does.
func (b *Box) PowerCycle() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.key = mk312.NoKey
	b.pending = nil
	b.inbuf = nil
	b.registers[mk312.AddressLevelA] = 0
	b.registers[mk312.AddressLevelB] = 0
	b.registers[mk312.AddressR15] = 0
	b.registers[mk312.AddressPowerLevel] = mk312.PowerNormal
}
