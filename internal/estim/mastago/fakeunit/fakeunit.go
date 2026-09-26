// Package fakeunit is an in-memory Mastago unit for tests: a ble.Conn that
// answers the AT protocol the way the real unit does (query answers
// followed by a bare OK, named acknowledgments, ERROR:102 without
// electrode contact, stray bytes in front of an unsupported-command
// error), plus the unit's own reports (intensity changed at its buttons,
// contact changes, countdown end, auto-off followed by a dropped link).
// A Central hands out connections to one or more units so a Finder can be
// tested too.
package fakeunit

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/ble"
)

// Unit is one simulated unit.
type Unit struct {
	mu sync.Mutex
	// Identity as the system would report it.
	ID   string
	Name string

	mode      int
	level     int
	timerS    int
	timerAt   time.Time
	started   bool // AT+QPOWS since the last AT+QPOWP
	load      bool
	volts     float64
	off       bool
	fragment  bool
	commands  []string
	conns     []*Conn
	minGap    time.Duration
	lastWrite time.Time
	// GapViolations counts writes that came sooner than MinGap after the
	// previous one.
	GapViolations int
	// Latency delays each reply, to exercise timeouts.
	Latency time.Duration
	// Mute drops replies to commands whose name is in it (queries still
	// fold nothing): a unit that stopped answering.
	Mute map[string]bool
	// NoService makes the Central advertise the peripheral without the
	// unit's service: some other Bluetooth device.
	NoService bool
}

// New is a unit with electrodes on the skin, program 0, intensity 0,
// paused, a full battery and a 60-minute countdown, as it powers on.
func New(id, name string) *Unit {
	return &Unit{ID: id, Name: name, load: true, volts: 4.0, timerS: 3600, timerAt: time.Now(), minGap: 50 * time.Millisecond}
}

// -- inspection and control from the test ------------------------------------------

// Level is the intensity the unit holds.
func (u *Unit) Level() int { u.mu.Lock(); defer u.mu.Unlock(); return u.level }

// Mode is the selected program.
func (u *Unit) Mode() int { u.mu.Lock(); defer u.mu.Unlock(); return u.mode }

// Outputting says whether the unit delivers its program: started, with an
// intensity above zero.
func (u *Unit) Outputting() bool { u.mu.Lock(); defer u.mu.Unlock(); return u.outputting() }

func (u *Unit) outputting() bool { return u.started && u.level > 0 }

// TimerS is the countdown as last set (not counted down).
func (u *Unit) TimerS() int { u.mu.Lock(); defer u.mu.Unlock(); return u.timerS }

// Commands are the command lines received, oldest first.
func (u *Unit) Commands() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.commands...)
}

// ResetCommands forgets the commands received.
func (u *Unit) ResetCommands() { u.mu.Lock(); u.commands = nil; u.mu.Unlock() }

// SetFragment makes each reply arrive in two notifications.
func (u *Unit) SetFragment(on bool) { u.mu.Lock(); u.fragment = on; u.mu.Unlock() }

// SetLoad puts the electrodes on the skin or takes them off, and reports
// the change the way the unit does.
func (u *Unit) SetLoad(on bool) {
	u.mu.Lock()
	u.load = on
	u.mu.Unlock()
	u.push(fmt.Sprintf("+CLOAD:%d", b2i(on)))
}

// SetVolts sets the battery voltage.
func (u *Unit) SetVolts(v float64) { u.mu.Lock(); u.volts = v; u.mu.Unlock() }

// PushLevel changes the intensity at the unit's own buttons.
func (u *Unit) PushLevel(n int) {
	u.mu.Lock()
	u.level = n
	u.mu.Unlock()
	u.push(fmt.Sprintf("+CSTR:1,%d", n))
}

// Heartbeat sends the unit's keepalive.
func (u *Unit) Heartbeat() { u.push("+CHEART:0,0") }

// EndTimer runs the countdown out: output stops, +CLKEND is reported.
func (u *Unit) EndTimer() {
	u.mu.Lock()
	u.timerS = 0
	u.started = false
	u.mu.Unlock()
	u.push("+CLKEND:000000")
}

// AutoOff switches the unit off for reason (0 no output, 1 continuous
// output, 2 low battery, 3 button): +QPOWD is reported, delivered before
// every link drops, as the real unit does.
func (u *Unit) AutoOff(reason int) {
	u.mu.Lock()
	u.off = true
	conns := u.conns
	u.conns = nil
	u.mu.Unlock()
	for _, c := range conns {
		c.deliverAndWait(fmt.Sprintf("+QPOWD:%d", reason))
		c.drop()
	}
}

// PowerOn brings a unit back after AutoOff, as it powers on.
func (u *Unit) PowerOn() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.off = false
	u.mode, u.level, u.started, u.timerS = 0, 0, false, 3600
}

// Off says whether the unit is switched off.
func (u *Unit) Off() bool { u.mu.Lock(); defer u.mu.Unlock(); return u.off }

// Connections is how many links are open to the unit.
func (u *Unit) Connections() int { u.mu.Lock(); defer u.mu.Unlock(); return len(u.conns) }

func (u *Unit) push(line string) {
	u.mu.Lock()
	conns := append([]*Conn(nil), u.conns...)
	u.mu.Unlock()
	for _, c := range conns {
		c.deliver(line)
	}
}

func b2i(v bool) int {
	if v {
		return 1
	}
	return 0
}

// -- the protocol ------------------------------------------------------------------

// handle answers one command line; the caller holds no lock.
func (u *Unit) handle(cmd string) [][]byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	now := time.Now()
	if !u.lastWrite.IsZero() && now.Sub(u.lastWrite) < u.minGap {
		u.GapViolations++
	}
	u.lastWrite = now
	u.commands = append(u.commands, cmd)
	name := cmd
	name = strings.TrimPrefix(strings.ToUpper(name), "AT+")
	if i := strings.IndexAny(name, "=?"); i >= 0 {
		name = name[:i]
	}
	if u.Mute[name] {
		return nil
	}
	query := strings.HasSuffix(cmd, "?")
	_, arg, _ := strings.Cut(cmd, "=")
	answer := func(lines ...string) [][]byte {
		out := make([][]byte, 0, len(lines))
		for _, l := range lines {
			out = append(out, []byte(l+"\r\n"))
		}
		return out
	}
	switch name {
	case "CMODE":
		if query {
			return answer(fmt.Sprintf("+CMODE:%d", u.mode), "OK")
		}
		m, ok := lastInt(arg)
		if !ok || m < 0 || m > 31 {
			return answer("+CMODE ERROR:1")
		}
		u.mode = m
		return answer("+CMODE:OK")
	case "CSTR":
		if query {
			return answer(fmt.Sprintf("+CSTR:%d", u.level), "OK")
		}
		n, ok := lastInt(arg)
		if !ok || n < 0 || n > 25 {
			return answer("+CSTR ERROR:1")
		}
		if !u.load {
			return answer("+CSTR ERROR:102")
		}
		u.level = n
		return answer("+CSTR:OK")
	case "CDCLK":
		if query {
			return answer("+CDCLK:"+hhmmss(u.remaining(now)), "OK")
		}
		s, ok := parseHHMMSS(arg)
		if !ok {
			return answer("+CDCLK ERROR:1")
		}
		u.timerS, u.timerAt = s, now
		return answer("+CDCLK:OK")
	case "QPOWP":
		if query {
			return answer(fmt.Sprintf("+QPOWP:%d", b2i(!u.outputting())), "OK")
		}
		u.started = false
		return answer("+QPOWP:OK")
	case "QPOWS":
		if query {
			return answer(fmt.Sprintf("+QPOWS:%d", b2i(u.outputting())), "OK")
		}
		u.started = true
		return answer("+QPOWS:OK")
	case "CBC":
		return answer(fmt.Sprintf("+CBC:0,%.2f", u.volts), "OK")
	case "CLOAD":
		return answer(fmt.Sprintf("+CLOAD:%d", b2i(u.load)), "OK")
	case "QPOWD":
		// Firmware 3.00: not supported, with two stray bytes in front.
		return [][]byte{append([]byte{0x80, 0x1b}, []byte(" ERROR:104\r\n")...)}
	}
	return answer("ERROR:1")
}

func (u *Unit) remaining(now time.Time) int {
	if u.timerAt.IsZero() {
		return u.timerS
	}
	return max(0, u.timerS-int(now.Sub(u.timerAt).Seconds()))
}

func lastInt(s string) (int, bool) {
	parts := strings.Split(s, ",")
	n, err := strconv.Atoi(strings.TrimSpace(parts[len(parts)-1]))
	return n, err == nil
}

func hhmmss(s int) string {
	return fmt.Sprintf("%02d%02d%02d", s/3600, s%3600/60, s%60)
}

func parseHHMMSS(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if len(s) != 6 {
		return 0, false
	}
	h, e1 := strconv.Atoi(s[0:2])
	m, e2 := strconv.Atoi(s[2:4])
	sec, e3 := strconv.Atoi(s[4:6])
	if e1 != nil || e2 != nil || e3 != nil {
		return 0, false
	}
	return h*3600 + m*60 + sec, true
}

// -- connection --------------------------------------------------------------------

// Conn is one link to a Unit, a ble.Conn.
type Conn struct {
	u            *Unit
	mu           sync.Mutex
	notify       func([]byte)
	queue        chan item
	disconnected chan struct{}
	once         sync.Once
	rx           []byte
}

type item struct {
	b    []byte
	done chan struct{}
}

// Connect opens a link to the unit. A unit that is off refuses.
func (u *Unit) Connect() (*Conn, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.off {
		return nil, errors.New("fakeunit: the unit is off")
	}
	c := &Conn{u: u, queue: make(chan item, 256), disconnected: make(chan struct{})}
	u.conns = append(u.conns, c)
	go c.run()
	return c, nil
}

// run delivers notifications in order, off the writer's goroutine, with
// the unit's latency before each.
func (c *Conn) run() {
	for {
		select {
		case it := <-c.queue:
			c.u.mu.Lock()
			latency := c.u.Latency
			c.u.mu.Unlock()
			if latency > 0 {
				select {
				case <-time.After(latency):
				case <-c.disconnected:
					return
				}
			}
			c.mu.Lock()
			fn := c.notify
			c.mu.Unlock()
			if fn != nil {
				fn(it.b)
			}
			if it.done != nil {
				close(it.done)
			}
		case <-c.disconnected:
			return
		}
	}
}

func (c *Conn) deliver(line string) {
	c.enqueue([]byte(line+"\r\n"), nil)
}

// deliverAndWait delivers one line and returns once the listener has had
// it (or the link is gone).
func (c *Conn) deliverAndWait(line string) {
	done := make(chan struct{})
	c.enqueue([]byte(line+"\r\n"), done)
	select {
	case <-done:
	case <-c.disconnected:
	case <-time.After(2 * time.Second):
	}
}

// enqueue queues one notification, in two pieces when the unit fragments;
// done, when set, is closed once the last piece is delivered.
func (c *Conn) enqueue(b []byte, done chan struct{}) {
	c.u.mu.Lock()
	fragment := c.u.fragment
	c.u.mu.Unlock()
	send := func(b []byte, done chan struct{}) {
		select {
		case c.queue <- item{b, done}:
		case <-c.disconnected:
			if done != nil {
				close(done)
			}
		}
	}
	if fragment && len(b) > 3 {
		send(b[:3], nil)
		send(b[3:], done)
		return
	}
	send(b, done)
}

func (c *Conn) drop() {
	c.once.Do(func() { close(c.disconnected) })
	c.u.mu.Lock()
	for i, x := range c.u.conns {
		if x == c {
			c.u.conns = append(c.u.conns[:i], c.u.conns[i+1:]...)
			break
		}
	}
	c.u.mu.Unlock()
}

// ID is the unit's identifier.
func (c *Conn) ID() string { return c.u.ID }

// Name is the unit's advertised name.
func (c *Conn) Name() string { return c.u.Name }

// Write takes command lines on the write characteristic.
func (c *Conn) Write(ctx context.Context, service, char ble.UUID, data []byte, withResponse bool) error {
	select {
	case <-c.disconnected:
		return ble.ErrDisconnected
	default:
	}
	if !service.Equal("fff0") || !char.Equal("fff5") {
		return ble.ErrNoCharacteristic
	}
	c.mu.Lock()
	c.rx = append(c.rx, data...)
	var lines []string
	for {
		i := strings.IndexByte(string(c.rx), '\n')
		if i < 0 {
			break
		}
		lines = append(lines, strings.TrimRight(string(c.rx[:i]), "\r"))
		c.rx = c.rx[i+1:]
	}
	c.mu.Unlock()
	for _, l := range lines {
		for _, reply := range c.u.handle(strings.TrimSpace(l)) {
			c.enqueue(reply, nil)
		}
	}
	return nil
}

// Subscribe registers the notification sink for the notify
// characteristic.
func (c *Conn) Subscribe(ctx context.Context, service, char ble.UUID, fn func([]byte)) error {
	if !service.Equal("fff0") || !char.Equal("fff4") {
		return ble.ErrNoCharacteristic
	}
	c.mu.Lock()
	c.notify = fn
	c.mu.Unlock()
	return nil
}

// Read is not part of the protocol the unit is driven with.
func (c *Conn) Read(ctx context.Context, service, char ble.UUID) ([]byte, error) {
	return nil, ble.ErrNoCharacteristic
}

// Disconnected is closed when the link drops.
func (c *Conn) Disconnected() <-chan struct{} { return c.disconnected }

// Close drops the link.
func (c *Conn) Close() error {
	c.drop()
	return nil
}

// -- central -----------------------------------------------------------------------

// Central is a ble.Central over a set of units: those in Units advertise
// when on; those in Held are connected to another program and do not
// advertise, but are listed by ConnectedWithService.
type Central struct {
	mu    sync.Mutex
	Units []*Unit
	Held  []*Unit
	// ScanDelay is how long a scan takes to report an advertising unit.
	ScanDelay time.Duration
	scans     int
	closed    bool
}

// Scans is how many scans were run.
func (c *Central) Scans() int { c.mu.Lock(); defer c.mu.Unlock(); return c.scans }

// Scan reports the first advertising unit match accepts.
func (c *Central) Scan(ctx context.Context, match func(ble.Advertisement) bool) (ble.Advertisement, error) {
	c.mu.Lock()
	c.scans++
	units := append([]*Unit(nil), c.Units...)
	delay := c.ScanDelay
	c.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ble.Advertisement{}, ctx.Err()
		}
	}
	for _, u := range units {
		if u.Off() {
			continue
		}
		a := ble.Advertisement{ID: u.ID, Name: u.Name, RSSI: -60}
		if !u.NoService {
			a.Services = []ble.UUID{"fff0"}
		}
		if match(a) {
			return a, nil
		}
	}
	<-ctx.Done()
	return ble.Advertisement{}, ctx.Err()
}

// ConnectedWithService lists the held units.
func (c *Central) ConnectedWithService(ctx context.Context, service ble.UUID) ([]ble.Advertisement, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []ble.Advertisement
	for _, u := range c.Held {
		if u.Off() {
			continue
		}
		out = append(out, ble.Advertisement{ID: u.ID, Name: u.Name, Services: []ble.UUID{service}, Connected: true})
	}
	return out, nil
}

// Connect opens a link to the unit with id.
func (c *Central) Connect(ctx context.Context, id string) (ble.Conn, error) {
	c.mu.Lock()
	units := append(append([]*Unit(nil), c.Units...), c.Held...)
	c.mu.Unlock()
	for _, u := range units {
		if u.ID == id {
			conn, err := u.Connect()
			if err != nil {
				return nil, err
			}
			return conn, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ble.ErrNotFound, id)
}

// Close marks the central closed.
func (c *Central) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

// Closed says whether Close was called.
func (c *Central) Closed() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.closed }
