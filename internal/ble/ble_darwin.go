//go:build darwin

package ble

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

// The macOS backend drives CoreBluetooth through the Objective-C runtime
// without cgo: the frameworks are dlopen'ed, messages are sent with
// objc_msgSend, and the delegate is a class registered at run time whose
// methods are Go functions. The central manager runs on a private serial
// dispatch queue, so callbacks arrive on a libdispatch thread and never
// need a run loop; each one copies what it needs into Go values and hands
// off to a waiting goroutine.
//
// Every Go-initiated message runs inside an autorelease pool on a locked
// OS thread (withPool), and the objects the backend keeps across calls
// (peripherals, characteristics, the manager, the delegate) are retained
// explicitly and released on Close.

// CBManagerState values.
const (
	stateUnknown      = 0
	stateResetting    = 1
	stateUnsupported  = 2
	stateUnauthorized = 3
	statePoweredOff   = 4
	statePoweredOn    = 5
)

// stateWait bounds how long Open waits for CoreBluetooth to report a
// state; the first use on a computer shows the Bluetooth permission
// prompt, and the state stays unknown until the person answers it.
const stateWait = 45 * time.Second

var (
	loadOnce sync.Once
	loadErr  error

	dispatchQueueCreate func(label string, attr uintptr) uintptr
	dispatchRelease     func(obj uintptr)
	dispatchAttrMake    func(attr uintptr, frequency uintptr) uintptr

	delegateClass objc.Class

	sel struct {
		alloc, init, retain, release, drain                                  objc.SEL
		initWithDelegateQueueOptions, state, authorization                   objc.SEL
		scanForPeripherals, stopScan                                         objc.SEL
		retrieveConnectedWithServices, retrieveWithIdentifiers               objc.SEL
		connectPeripheral, cancelConnection, setDelegate                     objc.SEL
		discoverServices, services, discoverCharacteristics, characteristics objc.SEL
		uuid, uuidString, identifier, name                                   objc.SEL
		writeValue, setNotifyValue, readValue, value                         objc.SEL
		bytes, length, dataWithBytes, utf8String, stringWithUTF8String       objc.SEL
		objectForKey, count, objectAtIndex, arrayWithObject                  objc.SEL
		initWithUUIDString, uuidWithString, localizedDescription             objc.SEL
		integerValue                                                         objc.SEL
	}
)

// registry maps the delegate object each central owns to the central, and
// connected peripherals (by identifier) to their connections. Callbacks
// pass the delegate as self, so the lookup needs nothing else.
var registry = struct {
	mu       sync.Mutex
	centrals map[objc.ID]*central
}{centrals: map[objc.ID]*central{}}

func load() error {
	loadOnce.Do(func() {
		loadErr = func() error {
			for _, path := range []string{
				"/System/Library/Frameworks/Foundation.framework/Foundation",
				"/System/Library/Frameworks/CoreBluetooth.framework/CoreBluetooth",
			} {
				if _, err := purego.Dlopen(path, purego.RTLD_GLOBAL|purego.RTLD_NOW); err != nil {
					return fmt.Errorf("ble: %w", err)
				}
			}
			libsystem, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_GLOBAL|purego.RTLD_NOW)
			if err != nil {
				return fmt.Errorf("ble: %w", err)
			}
			purego.RegisterLibFunc(&dispatchQueueCreate, libsystem, "dispatch_queue_create")
			purego.RegisterLibFunc(&dispatchRelease, libsystem, "dispatch_release")
			purego.RegisterLibFunc(&dispatchAttrMake, libsystem, "dispatch_queue_attr_make_with_autorelease_frequency")

			s := &sel
			s.alloc, s.init, s.retain, s.release, s.drain = n("alloc"), n("init"), n("retain"), n("release"), n("drain")
			s.initWithDelegateQueueOptions = n("initWithDelegate:queue:options:")
			s.state, s.authorization = n("state"), n("authorization")
			s.scanForPeripherals, s.stopScan = n("scanForPeripheralsWithServices:options:"), n("stopScan")
			s.retrieveConnectedWithServices = n("retrieveConnectedPeripheralsWithServices:")
			s.retrieveWithIdentifiers = n("retrievePeripheralsWithIdentifiers:")
			s.connectPeripheral, s.cancelConnection, s.setDelegate = n("connectPeripheral:options:"), n("cancelPeripheralConnection:"), n("setDelegate:")
			s.discoverServices, s.services = n("discoverServices:"), n("services")
			s.discoverCharacteristics, s.characteristics = n("discoverCharacteristics:forService:"), n("characteristics")
			s.uuid, s.uuidString, s.identifier, s.name = n("UUID"), n("UUIDString"), n("identifier"), n("name")
			s.writeValue, s.setNotifyValue, s.readValue, s.value = n("writeValue:forCharacteristic:type:"), n("setNotifyValue:forCharacteristic:"), n("readValueForCharacteristic:"), n("value")
			s.bytes, s.length, s.dataWithBytes = n("bytes"), n("length"), n("dataWithBytes:length:")
			s.utf8String, s.stringWithUTF8String = n("UTF8String"), n("stringWithUTF8String:")
			s.objectForKey, s.count, s.objectAtIndex, s.arrayWithObject = n("objectForKey:"), n("count"), n("objectAtIndex:"), n("arrayWithObject:")
			s.initWithUUIDString, s.uuidWithString = n("initWithUUIDString:"), n("UUIDWithString:")
			s.localizedDescription, s.integerValue = n("localizedDescription"), n("integerValue")

			cls, err := objc.RegisterClass("MasseuseCamlinkBLEDelegate", objc.GetClass("NSObject"), nil, nil, []objc.MethodDef{
				{Cmd: n("centralManagerDidUpdateState:"), Fn: cbDidUpdateState},
				{Cmd: n("centralManager:didDiscoverPeripheral:advertisementData:RSSI:"), Fn: cbDidDiscover},
				{Cmd: n("centralManager:didConnectPeripheral:"), Fn: cbDidConnect},
				{Cmd: n("centralManager:didFailToConnectPeripheral:error:"), Fn: cbDidFailToConnect},
				{Cmd: n("centralManager:didDisconnectPeripheral:error:"), Fn: cbDidDisconnect},
				{Cmd: n("peripheral:didDiscoverServices:"), Fn: cbDidDiscoverServices},
				{Cmd: n("peripheral:didDiscoverCharacteristicsForService:error:"), Fn: cbDidDiscoverCharacteristics},
				{Cmd: n("peripheral:didUpdateValueForCharacteristic:error:"), Fn: cbDidUpdateValue},
				{Cmd: n("peripheral:didWriteValueForCharacteristic:error:"), Fn: cbDidWriteValue},
				{Cmd: n("peripheral:didUpdateNotificationStateForCharacteristic:error:"), Fn: cbDidUpdateNotificationState},
			})
			if err != nil {
				return fmt.Errorf("ble: registering the delegate class: %w", err)
			}
			delegateClass = cls
			return nil
		}()
	})
	return loadErr
}

func n(name string) objc.SEL { return objc.RegisterName(name) }

// withPool runs f inside an autorelease pool on a locked OS thread, so the
// temporaries CoreBluetooth and Foundation hand back are freed when f
// returns rather than leaking on a thread that has no pool.
func withPool(f func()) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	pool := objc.ID(objc.GetClass("NSAutoreleasePool")).Send(sel.alloc).Send(sel.init)
	defer pool.Send(sel.drain)
	f()
}

func nsString(s string) objc.ID {
	return objc.ID(objc.GetClass("NSString")).Send(sel.stringWithUTF8String, s)
}

func goString(ns objc.ID) string {
	if ns == 0 {
		return ""
	}
	p := objc.Send[*byte](ns, sel.utf8String)
	if p == nil {
		return ""
	}
	n := 0
	for *(*byte)(unsafe.Add(unsafe.Pointer(p), n)) != 0 {
		n++
	}
	return string(unsafe.Slice(p, n))
}

func goBytes(data objc.ID) []byte {
	if data == 0 {
		return nil
	}
	length := objc.Send[uintptr](data, sel.length)
	if length == 0 {
		return []byte{}
	}
	p := objc.Send[*byte](data, sel.bytes)
	return append([]byte(nil), unsafe.Slice(p, length)...)
}

func nsData(b []byte) objc.ID {
	if len(b) == 0 {
		return objc.ID(objc.GetClass("NSData")).Send(sel.dataWithBytes, uintptr(0), uintptr(0))
	}
	d := objc.ID(objc.GetClass("NSData")).Send(sel.dataWithBytes, unsafe.Pointer(&b[0]), uintptr(len(b)))
	runtime.KeepAlive(b)
	return d
}

func cbUUID(u UUID) objc.ID {
	return objc.ID(objc.GetClass("CBUUID")).Send(sel.uuidWithString, nsString(u.Canonical()))
}

func nsArrayOf(obj objc.ID) objc.ID {
	return objc.ID(objc.GetClass("NSArray")).Send(sel.arrayWithObject, obj)
}

func arrayItems(arr objc.ID) []objc.ID {
	if arr == 0 {
		return nil
	}
	count := objc.Send[uintptr](arr, sel.count)
	out := make([]objc.ID, 0, count)
	for i := uintptr(0); i < count; i++ {
		out = append(out, arr.Send(sel.objectAtIndex, i))
	}
	return out
}

func errorString(err objc.ID) string {
	if err == 0 {
		return ""
	}
	return goString(err.Send(sel.localizedDescription))
}

func attributeUUID(attr objc.ID) UUID {
	return UUID(goString(attr.Send(sel.uuid).Send(sel.uuidString)))
}

func peripheralID(p objc.ID) string {
	return goString(p.Send(sel.identifier).Send(sel.uuidString))
}

// -- central ---------------------------------------------------------------

type central struct {
	log      *slog.Logger
	delegate objc.ID
	mgr      objc.ID
	queue    uintptr

	mu       sync.Mutex
	state    int
	stateCh  chan struct{} // closed and replaced on every state change
	scanSink func(Advertisement)
	// peripherals are retained CBPeripheral objects by identifier: those
	// seen in a scan, retrieved as connected, or looked up by identifier.
	peripherals map[string]objc.ID
	conns       map[string]*conn
	closed      bool
}

// Open creates the CoreBluetooth central and waits for it to report a
// usable state. A computer without Bluetooth returns ErrUnsupported; one
// with Bluetooth switched off or this program not allowed to use it,
// ErrUnavailable (with the reason).
func Open(ctx context.Context, log *slog.Logger) (Central, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if err := load(); err != nil {
		return nil, err
	}
	c := &central{log: log, stateCh: make(chan struct{}), peripherals: map[string]objc.ID{}, conns: map[string]*conn{}}
	withPool(func() {
		c.delegate = objc.ID(delegateClass).Send(sel.alloc).Send(sel.init)
		registry.mu.Lock()
		registry.centrals[c.delegate] = c
		registry.mu.Unlock()
		attr := dispatchAttrMake(0, 1) // DISPATCH_AUTORELEASE_FREQUENCY_WORK_ITEM
		c.queue = dispatchQueueCreate("ai.masseuse.camlink.ble", attr)
		c.mgr = objc.ID(objc.GetClass("CBCentralManager")).Send(sel.alloc).Send(sel.initWithDelegateQueueOptions, c.delegate, c.queue, objc.ID(0))
	})
	if err := c.waitPoweredOn(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

func (c *central) waitPoweredOn(ctx context.Context) error {
	deadline := time.NewTimer(stateWait)
	defer deadline.Stop()
	warned := false
	for {
		c.mu.Lock()
		state, ch := c.state, c.stateCh
		c.mu.Unlock()
		switch state {
		case statePoweredOn:
			return nil
		case stateUnsupported:
			return fmt.Errorf("%w: this computer has no Bluetooth", ErrUnsupported)
		case stateUnauthorized:
			return fmt.Errorf("%w: this program is not allowed to use Bluetooth (System Settings > Privacy & Security > Bluetooth)", ErrUnavailable)
		case statePoweredOff:
			return fmt.Errorf("%w: Bluetooth is switched off", ErrUnavailable)
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("%w: Bluetooth did not report its state", ErrUnavailable)
		case <-time.After(2 * time.Second):
			if !warned {
				warned = true
				c.log.Info("ble: waiting for Bluetooth permission; answer the system prompt if one is showing")
			}
		}
	}
}

func (c *central) setState(state int) {
	c.mu.Lock()
	c.state = state
	close(c.stateCh)
	c.stateCh = make(chan struct{})
	c.mu.Unlock()
	c.log.Debug("ble: central state", "state", state)
}

// Scan reports the first advertisement match accepts.
func (c *central) Scan(ctx context.Context, match func(Advertisement) bool) (Advertisement, error) {
	found := make(chan Advertisement, 1)
	var once sync.Once
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return Advertisement{}, errors.New("ble: central is closed")
	}
	if c.scanSink != nil {
		c.mu.Unlock()
		return Advertisement{}, errors.New("ble: a scan is already running")
	}
	c.scanSink = func(a Advertisement) {
		if match(a) {
			once.Do(func() { found <- a })
		}
	}
	c.mu.Unlock()
	withPool(func() { c.mgr.Send(sel.scanForPeripherals, objc.ID(0), objc.ID(0)) })
	defer func() {
		withPool(func() { c.mgr.Send(sel.stopScan) })
		c.mu.Lock()
		c.scanSink = nil
		c.mu.Unlock()
	}()
	select {
	case a := <-found:
		return a, nil
	case <-ctx.Done():
		return Advertisement{}, ctx.Err()
	}
}

// ConnectedWithService lists peripherals the system already holds a
// connection to (another program's) that offer the service.
func (c *central) ConnectedWithService(ctx context.Context, service UUID) ([]Advertisement, error) {
	var out []Advertisement
	withPool(func() {
		arr := c.mgr.Send(sel.retrieveConnectedWithServices, nsArrayOf(cbUUID(service)))
		for _, p := range arrayItems(arr) {
			id := c.keep(p)
			out = append(out, Advertisement{ID: id, Name: goString(p.Send(sel.name)), Services: []UUID{service}, Connected: true})
		}
	})
	return out, ctx.Err()
}

// keep retains a peripheral and remembers it by identifier; the caller is
// inside a pool. Returns the identifier.
func (c *central) keep(p objc.ID) string {
	id := peripheralID(p)
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.peripherals[id]; !ok {
		p.Send(sel.retain)
		c.peripherals[id] = p
	}
	return id
}

func (c *central) peripheral(id string) (objc.ID, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.peripherals[id]
	return p, ok
}

// Connect opens a connection to the peripheral with ID and discovers its
// services and characteristics.
func (c *central) Connect(ctx context.Context, id string) (Conn, error) {
	p, ok := c.peripheral(id)
	if !ok {
		withPool(func() {
			nsid := objc.ID(objc.GetClass("NSUUID")).Send(sel.alloc).Send(sel.initWithUUIDString, nsString(id))
			if nsid == 0 {
				return
			}
			defer nsid.Send(sel.release)
			for _, cand := range arrayItems(c.mgr.Send(sel.retrieveWithIdentifiers, nsArrayOf(nsid))) {
				c.keep(cand)
			}
		})
		if p, ok = c.peripheral(id); !ok {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
	}
	cn := &conn{c: c, p: p, id: id, connected: make(chan error, 1), disconnected: make(chan struct{}), events: make(chan event, 256), chars: map[string]objc.ID{}}
	c.mu.Lock()
	if prev, exists := c.conns[id]; exists {
		c.mu.Unlock()
		if prev.isOpen() {
			return nil, fmt.Errorf("ble: %s is already connected", id)
		}
		c.mu.Lock()
	}
	c.conns[id] = cn
	c.mu.Unlock()
	withPool(func() {
		cn.name = goString(p.Send(sel.name))
		p.Send(sel.setDelegate, c.delegate)
		c.mgr.Send(sel.connectPeripheral, p, objc.ID(0))
	})
	select {
	case err := <-cn.connected:
		if err != nil {
			c.forget(cn)
			return nil, err
		}
	case <-ctx.Done():
		withPool(func() { c.mgr.Send(sel.cancelConnection, p) })
		c.forget(cn)
		return nil, ctx.Err()
	}
	go cn.dispatch()
	if err := cn.discover(ctx); err != nil {
		_ = cn.Close()
		return nil, err
	}
	return cn, nil
}

func (c *central) forget(cn *conn) {
	c.mu.Lock()
	if c.conns[cn.id] == cn {
		delete(c.conns, cn.id)
	}
	c.mu.Unlock()
}

func (c *central) conn(p objc.ID) *conn {
	id := peripheralID(p)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conns[id]
}

// Close stops any scan, disconnects nothing (connections close
// themselves) and releases the central's objects.
func (c *central) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	peripherals := c.peripherals
	c.peripherals = map[string]objc.ID{}
	c.mu.Unlock()
	registry.mu.Lock()
	delete(registry.centrals, c.delegate)
	registry.mu.Unlock()
	withPool(func() {
		if c.mgr != 0 {
			c.mgr.Send(sel.stopScan)
			c.mgr.Send(sel.setDelegate, objc.ID(0))
			c.mgr.Send(sel.release)
			c.mgr = 0
		}
		for _, p := range peripherals {
			p.Send(sel.setDelegate, objc.ID(0))
			p.Send(sel.release)
		}
		if c.delegate != 0 {
			c.delegate.Send(sel.release)
			c.delegate = 0
		}
		if c.queue != 0 {
			dispatchRelease(c.queue)
			c.queue = 0
		}
	})
	return nil
}

// -- connection --------------------------------------------------------------

type event struct {
	char objc.ID
	data []byte
}

type conn struct {
	c    *central
	p    objc.ID
	id   string
	name string

	connected    chan error
	disconnected chan struct{}
	events       chan event
	closeOnce    sync.Once

	mu           sync.Mutex
	open         bool
	chars        map[string]objc.ID // by canonical characteristic UUID; retained
	servicesDone chan error
	charsLeft    int
	charsDone    chan error
	writeDone    chan error
	notifyDone   chan error
	readDone     chan readResult
	readChar     objc.ID
	subs         map[objc.ID]func([]byte)
	// io serializes writes, reads and subscriptions: CoreBluetooth reports
	// their completion without saying which request it was for.
	io sync.Mutex
}

type readResult struct {
	data []byte
	err  error
}

func (cn *conn) ID() string   { return cn.id }
func (cn *conn) Name() string { return cn.name }

func (cn *conn) isOpen() bool {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	return cn.open
}

// Disconnected is closed when the link drops.
func (cn *conn) Disconnected() <-chan struct{} { return cn.disconnected }

// discover walks services and characteristics once connected.
func (cn *conn) discover(ctx context.Context) error {
	cn.mu.Lock()
	cn.servicesDone = make(chan error, 1)
	cn.mu.Unlock()
	withPool(func() { cn.p.Send(sel.discoverServices, objc.ID(0)) })
	if err := cn.wait(ctx, cn.servicesDone); err != nil {
		return fmt.Errorf("ble: discovering services: %w", err)
	}
	var services []objc.ID
	withPool(func() { services = arrayItems(cn.p.Send(sel.services)) })
	if len(services) == 0 {
		return nil
	}
	cn.mu.Lock()
	cn.charsLeft = len(services)
	cn.charsDone = make(chan error, 1)
	cn.mu.Unlock()
	withPool(func() {
		for _, s := range services {
			cn.p.Send(sel.discoverCharacteristics, objc.ID(0), s)
		}
	})
	if err := cn.wait(ctx, cn.charsDone); err != nil {
		return fmt.Errorf("ble: discovering characteristics: %w", err)
	}
	withPool(func() {
		cn.mu.Lock()
		defer cn.mu.Unlock()
		for _, s := range services {
			for _, ch := range arrayItems(s.Send(sel.characteristics)) {
				key := attributeUUID(ch).Canonical()
				if _, dup := cn.chars[key]; dup {
					continue
				}
				ch.Send(sel.retain)
				cn.chars[key] = ch
			}
		}
	})
	return nil
}

// wait blocks for a completion, the link dropping, or ctx.
func (cn *conn) wait(ctx context.Context, done chan error) error {
	select {
	case err := <-done:
		return err
	case <-cn.disconnected:
		return ErrDisconnected
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (cn *conn) characteristic(char UUID) (objc.ID, error) {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	if !cn.open {
		return 0, ErrDisconnected
	}
	ch, ok := cn.chars[char.Canonical()]
	if !ok {
		return 0, fmt.Errorf("%w: %s", ErrNoCharacteristic, char.Short())
	}
	return ch, nil
}

// Write sends data to the characteristic. The service is implied by the
// characteristic on this backend.
func (cn *conn) Write(ctx context.Context, _ UUID, char UUID, data []byte, withResponse bool) error {
	ch, err := cn.characteristic(char)
	if err != nil {
		return err
	}
	cn.io.Lock()
	defer cn.io.Unlock()
	var done chan error
	if withResponse {
		done = make(chan error, 1)
		cn.mu.Lock()
		cn.writeDone = done
		cn.mu.Unlock()
	}
	withPool(func() {
		kind := uintptr(1) // CBCharacteristicWriteWithoutResponse
		if withResponse {
			kind = 0 // CBCharacteristicWriteWithResponse
		}
		cn.p.Send(sel.writeValue, nsData(data), ch, kind)
	})
	if !withResponse {
		return nil
	}
	err = cn.wait(ctx, done)
	cn.mu.Lock()
	cn.writeDone = nil
	cn.mu.Unlock()
	return err
}

// Subscribe turns notifications on and routes each value to fn.
func (cn *conn) Subscribe(ctx context.Context, _ UUID, char UUID, fn func([]byte)) error {
	ch, err := cn.characteristic(char)
	if err != nil {
		return err
	}
	cn.io.Lock()
	defer cn.io.Unlock()
	done := make(chan error, 1)
	cn.mu.Lock()
	if cn.subs == nil {
		cn.subs = map[objc.ID]func([]byte){}
	}
	cn.subs[ch] = fn
	cn.notifyDone = done
	cn.mu.Unlock()
	withPool(func() { cn.p.Send(sel.setNotifyValue, true, ch) })
	err = cn.wait(ctx, done)
	cn.mu.Lock()
	cn.notifyDone = nil
	if err != nil {
		delete(cn.subs, ch)
	}
	cn.mu.Unlock()
	return err
}

// Read reads the characteristic's value.
func (cn *conn) Read(ctx context.Context, _ UUID, char UUID) ([]byte, error) {
	ch, err := cn.characteristic(char)
	if err != nil {
		return nil, err
	}
	cn.io.Lock()
	defer cn.io.Unlock()
	done := make(chan readResult, 1)
	cn.mu.Lock()
	cn.readDone, cn.readChar = done, ch
	cn.mu.Unlock()
	withPool(func() { cn.p.Send(sel.readValue, ch) })
	var res readResult
	select {
	case res = <-done:
	case <-cn.disconnected:
		res.err = ErrDisconnected
	case <-ctx.Done():
		res.err = ctx.Err()
	}
	cn.mu.Lock()
	cn.readDone, cn.readChar = nil, 0
	cn.mu.Unlock()
	return res.data, res.err
}

// dispatch delivers notifications to subscribers in order, off the
// CoreBluetooth queue.
func (cn *conn) dispatch() {
	for {
		select {
		case ev := <-cn.events:
			cn.mu.Lock()
			fn := cn.subs[ev.char]
			cn.mu.Unlock()
			if fn != nil {
				fn(ev.data)
			}
		case <-cn.disconnected:
			// Drain what arrived before the drop, then stop.
			for {
				select {
				case ev := <-cn.events:
					cn.mu.Lock()
					fn := cn.subs[ev.char]
					cn.mu.Unlock()
					if fn != nil {
						fn(ev.data)
					}
				default:
					return
				}
			}
		}
	}
}

// Close disconnects and releases what the connection retained.
func (cn *conn) Close() error {
	cn.mu.Lock()
	wasOpen := cn.open
	cn.open = false
	cn.mu.Unlock()
	if wasOpen {
		withPool(func() { cn.c.mgr.Send(sel.cancelConnection, cn.p) })
		select {
		case <-cn.disconnected:
		case <-time.After(5 * time.Second):
			cn.c.log.Debug("ble: disconnect not confirmed", "peripheral", cn.id)
			cn.dropped(nil)
		}
	} else {
		cn.dropped(nil)
	}
	return nil
}

// dropped marks the link gone: waiters are released, the disconnected
// channel closed, retained characteristics let go.
func (cn *conn) dropped(err error) {
	cn.closeOnce.Do(func() {
		cn.mu.Lock()
		cn.open = false
		chars := cn.chars
		cn.chars = map[string]objc.ID{}
		cn.mu.Unlock()
		close(cn.disconnected)
		cn.c.forget(cn)
		if err != nil {
			cn.c.log.Debug("ble: disconnected", "peripheral", cn.id, "err", err)
		}
		if len(chars) > 0 {
			withPool(func() {
				for _, ch := range chars {
					ch.Send(sel.release)
				}
			})
		}
	})
}

// -- delegate callbacks (on the dispatch queue) ------------------------------

func centralFor(self objc.ID) *central {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	return registry.centrals[self]
}

func cbDidUpdateState(self objc.ID, _ objc.SEL, mgr objc.ID) {
	c := centralFor(self)
	if c == nil {
		return
	}
	c.setState(objc.Send[int](mgr, sel.state))
}

func cbDidDiscover(self objc.ID, _ objc.SEL, _ objc.ID, p objc.ID, advData objc.ID, rssi objc.ID) {
	c := centralFor(self)
	if c == nil {
		return
	}
	c.mu.Lock()
	sink := c.scanSink
	c.mu.Unlock()
	if sink == nil {
		return
	}
	adv := Advertisement{ID: c.keep(p), Name: goString(p.Send(sel.name))}
	if advData != 0 {
		if local := advData.Send(sel.objectForKey, nsString("kCBAdvDataLocalName")); local != 0 {
			adv.Name = goString(local)
		}
		for _, u := range arrayItems(advData.Send(sel.objectForKey, nsString("kCBAdvDataServiceUUIDs"))) {
			adv.Services = append(adv.Services, UUID(goString(u.Send(sel.uuidString))))
		}
	}
	if rssi != 0 {
		adv.RSSI = objc.Send[int](rssi, sel.integerValue)
	}
	sink(adv)
}

func cbDidConnect(self objc.ID, _ objc.SEL, _ objc.ID, p objc.ID) {
	c := centralFor(self)
	if c == nil {
		return
	}
	if cn := c.conn(p); cn != nil {
		cn.mu.Lock()
		cn.open = true
		cn.mu.Unlock()
		select {
		case cn.connected <- nil:
		default:
		}
	}
}

func cbDidFailToConnect(self objc.ID, _ objc.SEL, _ objc.ID, p objc.ID, err objc.ID) {
	c := centralFor(self)
	if c == nil {
		return
	}
	if cn := c.conn(p); cn != nil {
		msg := errorString(err)
		if msg == "" {
			msg = "connection failed"
		}
		select {
		case cn.connected <- errors.New("ble: " + msg):
		default:
		}
	}
}

func cbDidDisconnect(self objc.ID, _ objc.SEL, _ objc.ID, p objc.ID, err objc.ID) {
	c := centralFor(self)
	if c == nil {
		return
	}
	if cn := c.conn(p); cn != nil {
		var e error
		if msg := errorString(err); msg != "" {
			e = errors.New(msg)
		}
		cn.dropped(e)
	}
}

func completion(err objc.ID) error {
	if msg := errorString(err); msg != "" {
		return errors.New("ble: " + msg)
	}
	return nil
}

func cbDidDiscoverServices(self objc.ID, _ objc.SEL, p objc.ID, err objc.ID) {
	c := centralFor(self)
	if c == nil {
		return
	}
	if cn := c.conn(p); cn != nil {
		cn.mu.Lock()
		done := cn.servicesDone
		cn.mu.Unlock()
		if done != nil {
			select {
			case done <- completion(err):
			default:
			}
		}
	}
}

func cbDidDiscoverCharacteristics(self objc.ID, _ objc.SEL, p objc.ID, _ objc.ID, err objc.ID) {
	c := centralFor(self)
	if c == nil {
		return
	}
	if cn := c.conn(p); cn != nil {
		cn.mu.Lock()
		cn.charsLeft--
		done := cn.charsDone
		left := cn.charsLeft
		cn.mu.Unlock()
		if done == nil {
			return
		}
		if e := completion(err); e != nil {
			select {
			case done <- e:
			default:
			}
			return
		}
		if left <= 0 {
			select {
			case done <- nil:
			default:
			}
		}
	}
}

func cbDidUpdateValue(self objc.ID, _ objc.SEL, p objc.ID, ch objc.ID, err objc.ID) {
	c := centralFor(self)
	if c == nil {
		return
	}
	cn := c.conn(p)
	if cn == nil {
		return
	}
	cn.mu.Lock()
	read, readChar := cn.readDone, cn.readChar
	cn.mu.Unlock()
	if read != nil && readChar == ch {
		res := readResult{err: completion(err)}
		if res.err == nil {
			res.data = goBytes(ch.Send(sel.value))
		}
		select {
		case read <- res:
		default:
		}
		return
	}
	if err != 0 {
		c.log.Debug("ble: notification error", "peripheral", cn.id, "err", errorString(err))
		return
	}
	select {
	case cn.events <- event{char: ch, data: goBytes(ch.Send(sel.value))}:
	default:
		c.log.Warn("ble: notification dropped; the reader is not keeping up", "peripheral", cn.id)
	}
}

func cbDidWriteValue(self objc.ID, _ objc.SEL, p objc.ID, _ objc.ID, err objc.ID) {
	c := centralFor(self)
	if c == nil {
		return
	}
	if cn := c.conn(p); cn != nil {
		cn.mu.Lock()
		done := cn.writeDone
		cn.mu.Unlock()
		if done != nil {
			select {
			case done <- completion(err):
			default:
			}
		}
	}
}

func cbDidUpdateNotificationState(self objc.ID, _ objc.SEL, p objc.ID, _ objc.ID, err objc.ID) {
	c := centralFor(self)
	if c == nil {
		return
	}
	if cn := c.conn(p); cn != nil {
		cn.mu.Lock()
		done := cn.notifyDone
		cn.mu.Unlock()
		if done != nil {
			select {
			case done <- completion(err):
			default:
			}
		}
	}
}
