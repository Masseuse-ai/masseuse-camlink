//go:build windows

package ble

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/saltosystems/winrt-go/windows/devices/bluetooth"
	"github.com/saltosystems/winrt-go/windows/devices/bluetooth/advertisement"
	gatt "github.com/saltosystems/winrt-go/windows/devices/bluetooth/genericattributeprofile"
	"github.com/saltosystems/winrt-go/windows/foundation"
	"github.com/saltosystems/winrt-go/windows/foundation/collections"
	"golang.org/x/sys/windows"
)

// The Windows backend drives Windows.Devices.Bluetooth (the Windows
// Runtime) without cgo: winrt-go's generated bindings call the COM vtables
// with syscall.SyscallN, and the few interfaces its set leaves out are
// called the same way from winrt_windows.go. The runtime raises its events
// on thread-pool threads; each handler copies what it needs into Go values
// and hands off through a channel, so nothing runs under a lock inside a
// callback and nothing in a callback waits on the runtime.
//
// A peripheral's identifier is its Bluetooth address (AA:BB:CC:DD:EE:FF,
// the form BlueZ uses on Linux). The runtime has no connect call: a link is
// opened by the first uncached GATT request and held by a GattSession
// with MaintainConnection set; the device's ConnectionStatusChanged event
// says when it drops. A random address must be looked up as such, so the
// address type each advertisement carried is remembered for the run.

const (
	// openWait bounds the adapter and radio queries in Open.
	openWait = 10 * time.Second
	// teardownWait bounds how long Close waits for the runtime objects to
	// be let go of.
	teardownWait = 5 * time.Second
	// unsubscribeWait bounds turning a characteristic's notifications off
	// on the way out.
	unsubscribeWait = 2 * time.Second
)

type central struct {
	log *slog.Logger

	mu      sync.Mutex
	adapter *ole.IUnknown // IBluetoothAdapter
	radio   *ole.IUnknown // IRadio; nil when the adapter reports none
	closed  bool
	// scanning: one advertisement watcher at a time, as on macOS.
	scanning bool
	// seen is what this run's scans and lookups learned per address: the
	// address type, and the name and services merged across a
	// peripheral's advertisement and its scan response, which the runtime
	// reports as separate events.
	seen  map[uint64]*seenPeripheral
	conns map[string]*conn
}

type seenPeripheral struct {
	addrType bluetooth.BluetoothAddressType
	name     string
	services []UUID
}

// minBuild is the first Windows 10 build with the runtime classes the
// backend uses (GattSession, the uncached characteristic discovery, the
// device id): version 1703, the Creators Update.
const minBuild = 15063

// Open finds the computer's Bluetooth adapter and checks its radio. A
// computer without an adapter, or a Windows without the Low Energy
// runtime classes (before Windows 10 version 1703), returns ErrUnsupported;
// a radio that is switched off or disabled, ErrUnavailable with the reason.
func Open(ctx context.Context, log *slog.Logger) (c Central, err error) {
	defer func() {
		if r := recover(); r != nil {
			c, err = nil, fmt.Errorf("%w: the Windows Runtime failed: %v", ErrUnavailable, r)
		}
	}()
	return open(ctx, log)
}

func open(ctx context.Context, log *slog.Logger) (Central, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if v := windows.RtlGetVersion(); v.MajorVersion < 10 || (v.MajorVersion == 10 && v.BuildNumber < minBuild) {
		return nil, fmt.Errorf("%w: Bluetooth Low Energy needs Windows 10 version 1703 or newer (this is build %d)", ErrUnsupported, v.BuildNumber)
	}
	if err := initWinRT(); err != nil {
		return nil, fmt.Errorf("%w: the Windows Runtime could not be initialized: %v", ErrUnsupported, err)
	}
	octx, cancel := context.WithTimeout(ctx, openWait)
	defer cancel()
	c := &central{log: log, seen: map[uint64]*seenPeripheral{}, conns: map[string]*conn{}}

	op, err := bluetoothAdapterGetDefaultAsync()
	if err != nil {
		if hresult(err) == hrClassNotReg {
			return nil, fmt.Errorf("%w: this Windows has no Bluetooth Low Energy runtime (Windows 10 version 1703 or newer is needed)", ErrUnsupported)
		}
		return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	p, err := awaitPointer(octx, op, sigBluetoothAdapter)
	if err != nil {
		return nil, fmt.Errorf("%w: asking for the Bluetooth adapter: %v", ErrUnavailable, err)
	}
	if p == nil {
		return nil, fmt.Errorf("%w: this computer has no Bluetooth", ErrUnsupported)
	}
	result := (*ole.IUnknown)(p)
	c.adapter, err = queryInterface(result, guidBluetoothAdapter)
	result.Release()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	if !adapterSupportsCentral(c.adapter) {
		_ = c.Close()
		return nil, fmt.Errorf("%w: this computer's Bluetooth does not support Low Energy", ErrUnsupported)
	}
	if rop, err := adapterGetRadioAsync(c.adapter); err != nil {
		log.Debug("ble: asking for the radio", "err", err)
	} else if rp, err := awaitPointer(octx, rop, sigRadio); err != nil {
		log.Debug("ble: asking for the radio", "err", err)
	} else if rp != nil {
		c.radio = (*ole.IUnknown)(rp)
	}
	if err := c.radioReady(); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// radioReady is nil while the radio is on, or when the system will not
// say (the scan then reports); ErrUnavailable with the reason otherwise.
func (c *central) radioReady() error {
	c.mu.Lock()
	radio := c.radio
	c.mu.Unlock()
	if radio == nil {
		return nil
	}
	state, err := radioGetState(radio)
	if err != nil {
		return nil
	}
	switch state {
	case radioStateOff:
		return fmt.Errorf("%w: Bluetooth is switched off (Settings > Bluetooth & devices)", ErrUnavailable)
	case radioStateDisabled:
		return fmt.Errorf("%w: Bluetooth is disabled on this computer (Device Manager, or a policy)", ErrUnavailable)
	}
	return nil
}

// guard turns a panic in the runtime bindings (a QueryInterface the
// system refuses, an object already closed) into an error, so a surprise
// from the runtime fails one call rather than the program. It is deferred
// directly, so the recover is its own.
func guard(err *error) {
	if r := recover(); r != nil {
		*err = fmt.Errorf("ble: the Windows Runtime failed: %v", r)
	}
}

// Scan watches advertisements and reports the first one match accepts.
func (c *central) Scan(ctx context.Context, match func(Advertisement) bool) (_ Advertisement, err error) {
	defer guard(&err)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return Advertisement{}, errors.New("ble: central is closed")
	}
	if c.scanning {
		c.mu.Unlock()
		return Advertisement{}, errors.New("ble: a scan is already running")
	}
	c.scanning = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.scanning = false
		c.mu.Unlock()
	}()
	if err := c.radioReady(); err != nil {
		return Advertisement{}, err
	}

	w, err := advertisement.NewBluetoothLEAdvertisementWatcher()
	if err != nil {
		return Advertisement{}, fmt.Errorf("ble: starting a scan: %w", err)
	}
	defer w.Release()
	// Active scanning asks each advertiser for its scan response, which is
	// where a unit's name usually travels.
	if err := w.SetScanningMode(advertisement.BluetoothLEScanningModeActive); err != nil {
		c.log.Debug("ble: active scanning", "err", err)
	}

	found := make(chan Advertisement, 256)
	stopped := make(chan bluetooth.BluetoothError, 1)
	onReceived := typedHandler(c.log, advertisement.SignatureBluetoothLEAdvertisementWatcher, advertisement.SignatureBluetoothLEAdvertisementReceivedEventArgs, func(_, args unsafe.Pointer) {
		a, ok := c.received((*advertisement.BluetoothLEAdvertisementReceivedEventArgs)(args))
		if !ok {
			return
		}
		select {
		case found <- a:
		default:
			c.log.Debug("ble: advertisement dropped; the scan is not keeping up")
		}
	})
	defer onReceived.Release()
	onStopped := typedHandler(c.log, advertisement.SignatureBluetoothLEAdvertisementWatcher, advertisement.SignatureBluetoothLEAdvertisementWatcherStoppedEventArgs, func(_, args unsafe.Pointer) {
		code, err := (*advertisement.BluetoothLEAdvertisementWatcherStoppedEventArgs)(args).GetError()
		if err != nil {
			code = bluetooth.BluetoothErrorOtherError
		}
		select {
		case stopped <- code:
		default:
		}
	})
	defer onStopped.Release()

	receivedToken, err := w.AddReceived(onReceived)
	if err != nil {
		return Advertisement{}, fmt.Errorf("ble: starting a scan: %w", err)
	}
	defer func() { _ = w.RemoveReceived(receivedToken) }()
	stoppedToken, err := w.AddStopped(onStopped)
	if err != nil {
		return Advertisement{}, fmt.Errorf("ble: starting a scan: %w", err)
	}
	defer func() { _ = w.RemoveStopped(stoppedToken) }()
	if err := w.Start(); err != nil {
		return Advertisement{}, fmt.Errorf("ble: starting a scan: %w", err)
	}
	defer func() { _ = w.Stop() }()

	for {
		select {
		case a := <-found:
			if match(a) {
				return a, nil
			}
		case code := <-stopped:
			return Advertisement{}, watcherError(code)
		case <-ctx.Done():
			return Advertisement{}, ctx.Err()
		}
	}
}

// received turns one Received event into an Advertisement, merged with
// what earlier events said about the same address.
func (c *central) received(args *advertisement.BluetoothLEAdvertisementReceivedEventArgs) (Advertisement, bool) {
	addr, err := args.GetBluetoothAddress()
	if err != nil {
		return Advertisement{}, false
	}
	rssi, _ := args.GetRawSignalStrengthInDBm()
	var name string
	var services []UUID
	if adv, err := args.GetAdvertisement(); err == nil && adv != nil {
		name, _ = adv.GetLocalName()
		if v, err := adv.GetServiceUuids(); err == nil && v != nil {
			services, _ = vectorUUIDs(v)
			v.Release()
		}
		adv.Release()
	}
	addrType, typeKnown := receivedAddressType(args)

	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.remember(addr)
	if typeKnown {
		s.addrType = addrType
	}
	if name != "" {
		s.name = name
	}
	for _, u := range services {
		if !hasUUID(s.services, u) {
			s.services = append(s.services, u)
		}
	}
	return Advertisement{ID: formatAddress(addr), Name: s.name, Services: append([]UUID(nil), s.services...), RSSI: int(rssi)}, true
}

// remember is the record for an address, made when there is none; the
// caller holds c.mu.
func (c *central) remember(addr uint64) *seenPeripheral {
	s := c.seen[addr]
	if s == nil {
		s = &seenPeripheral{addrType: bluetooth.BluetoothAddressTypeUnspecified}
		c.seen[addr] = s
	}
	return s
}

func hasUUID(list []UUID, u UUID) bool {
	for _, x := range list {
		if x.Equal(u) {
			return true
		}
	}
	return false
}

// watcherError is why a watcher stopped on its own.
func watcherError(code bluetooth.BluetoothError) error {
	switch code {
	case bluetooth.BluetoothErrorRadioNotAvailable:
		return fmt.Errorf("%w: Bluetooth is switched off or not available", ErrUnavailable)
	case bluetooth.BluetoothErrorDisabledByPolicy:
		return fmt.Errorf("%w: Bluetooth is disabled by a policy on this computer", ErrUnavailable)
	case bluetooth.BluetoothErrorDisabledByUser, bluetooth.BluetoothErrorConsentRequired:
		return fmt.Errorf("%w: this program is not allowed to use Bluetooth (Settings > Privacy & security)", ErrUnavailable)
	case bluetooth.BluetoothErrorNotSupported, bluetooth.BluetoothErrorTransportNotSupported:
		return fmt.Errorf("%w: this computer's Bluetooth cannot scan for Low Energy devices", ErrUnsupported)
	case bluetooth.BluetoothErrorResourceInUse:
		return errors.New("ble: the Bluetooth radio is in use by another program")
	case bluetooth.BluetoothErrorSuccess:
		return errors.New("ble: the scan was stopped")
	}
	return fmt.Errorf("ble: the scan stopped (Bluetooth error %d)", code)
}

// enumerateWait bounds the enumeration of connected devices, which the
// finder calls without a deadline of its own.
const enumerateWait = 10 * time.Second

// ConnectedWithService lists the Low Energy devices the system holds a
// connection to that offer the service, by the system's own enumeration
// of connected devices. It is best effort: where the enumeration lists
// nothing, or fails, the list is empty and nothing else is affected.
func (c *central) ConnectedWithService(ctx context.Context, service UUID) (_ []Advertisement, err error) {
	defer guard(&err)
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return nil, errors.New("ble: central is closed")
	}
	ectx, cancel := context.WithTimeout(ctx, enumerateWait)
	defer cancel()
	selector, err := connectedDeviceSelector()
	if err != nil || selector == "" {
		c.log.Debug("ble: connected-device selector", "err", err)
		selector = connectedDeviceSelectorFallback
	}
	op, err := deviceInformationFindAllAsync(selector)
	if err != nil {
		c.log.Debug("ble: enumerating connected devices", "err", err)
		return nil, ctx.Err()
	}
	p, err := awaitPointer(ectx, op, sigDeviceInformationCollection)
	if err != nil {
		c.log.Debug("ble: enumerating connected devices", "err", err)
		return nil, ctx.Err()
	}
	if p == nil {
		return nil, nil
	}
	view := (*collections.IVectorView)(p)
	defer view.Release()
	items, err := viewItems(view)
	if err != nil {
		c.log.Debug("ble: enumerating connected devices", "err", err)
	}
	type candidate struct {
		addr uint64
		name string
	}
	var candidates []candidate
	for _, item := range items {
		info := (*ole.IUnknown)(item)
		id, name, err := deviceInformationIDName(info)
		info.Release()
		if err != nil {
			continue
		}
		if addr, ok := addressFromDeviceID(id); ok {
			candidates = append(candidates, candidate{addr, name})
		}
	}
	var out []Advertisement
	for _, cand := range candidates {
		if ectx.Err() != nil {
			break
		}
		if !c.offersService(ectx, cand.addr, service) {
			continue
		}
		name := cand.name
		c.mu.Lock()
		s := c.remember(cand.addr)
		if name != "" && s.name == "" {
			s.name = name
		}
		if name == "" {
			name = s.name
		}
		c.mu.Unlock()
		out = append(out, Advertisement{ID: formatAddress(cand.addr), Name: name, Services: []UUID{service}, Connected: true})
	}
	return out, ctx.Err()
}

// offersService says whether the system's cache of a device's services,
// read without connecting, names the service.
func (c *central) offersService(ctx context.Context, addr uint64, service UUID) bool {
	op, err := bluetooth.BluetoothLEDeviceFromBluetoothAddressAsync(addr)
	if err != nil {
		return false
	}
	p, err := awaitPointer(ctx, op, bluetooth.SignatureBluetoothLEDevice)
	if err != nil || p == nil {
		return false
	}
	dev := (*bluetooth.BluetoothLEDevice)(p)
	defer dev.Release()
	sop, err := dev.GetGattServicesWithCacheModeAsync(bluetooth.BluetoothCacheModeCached)
	if err != nil {
		return false
	}
	sp, err := awaitPointer(ctx, sop, gatt.SignatureGattDeviceServicesResult)
	if err != nil || sp == nil {
		return false
	}
	res := (*gatt.GattDeviceServicesResult)(sp)
	defer res.Release()
	if status, err := res.GetStatus(); err != nil || status != gatt.GattCommunicationStatusSuccess {
		return false
	}
	view, err := res.GetServices()
	if err != nil || view == nil {
		return false
	}
	defer view.Release()
	items, _ := viewItems(view)
	offers := false
	for _, item := range items {
		svc := (*gatt.GattDeviceService)(item)
		if g, err := svc.GetUuid(); err == nil && guidToUUID(g).Equal(service) {
			offers = true
		}
		svc.Release()
	}
	return offers
}

// Connect opens a link to the peripheral with the address id, holds it
// with a GattSession, and discovers its services and characteristics.
func (c *central) Connect(ctx context.Context, id string) (Conn, error) {
	var cn *conn
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("ble: connect %s: the Windows Runtime failed: %v", id, r)
				if cn != nil {
					_ = cn.fail(err)
				}
			}
		}()
		err = c.connect(ctx, id, &cn)
	}()
	if err != nil {
		return nil, err
	}
	return cn, nil
}

// connect is Connect; the connection is written to out as soon as it
// exists, so that the recover above can release it after a panic.
func (c *central) connect(ctx context.Context, id string, out **conn) error {
	addr, ok := parseAddress(id)
	if !ok {
		return fmt.Errorf("%w: %q is not a Bluetooth address", ErrNotFound, id)
	}
	id = formatAddress(addr)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("ble: central is closed")
	}
	if prev, exists := c.conns[id]; exists && prev.isOpen() {
		c.mu.Unlock()
		return fmt.Errorf("ble: %s is already connected", id)
	}
	addrType, advertisedName := bluetooth.BluetoothAddressTypeUnspecified, ""
	if s := c.seen[addr]; s != nil {
		addrType, advertisedName = s.addrType, s.name
	}
	c.mu.Unlock()

	dev, err := c.deviceFor(ctx, addr, addrType)
	if err != nil {
		return err
	}
	cn := &conn{
		c: c, id: id, device: dev,
		disconnected: make(chan struct{}), settled: make(chan struct{}), torn: make(chan struct{}),
		events: make(chan event, 256), chars: map[string]*gatt.GattCharacteristic{}, subs: map[string]*subscription{},
	}
	*out = cn
	if name, err := deviceName(dev); err == nil && name != "" {
		cn.name = name
	} else {
		cn.name = advertisedName
	}
	c.mu.Lock()
	c.conns[id] = cn
	c.mu.Unlock()

	// The status event first, so a link that drops while it is being
	// opened is not missed.
	cn.statusHandler = typedHandler(c.log, bluetooth.SignatureBluetoothLEDevice, sigInspectable, func(sender, _ unsafe.Pointer) {
		status, err := (*bluetooth.BluetoothLEDevice)(sender).GetConnectionStatus()
		if err == nil && status == bluetooth.BluetoothConnectionStatusDisconnected {
			cn.dropped(errors.New("device disconnected"))
		}
	})
	if cn.statusToken, err = dev.AddConnectionStatusChanged(cn.statusHandler); err != nil {
		cn.statusHandler.Release()
		cn.statusHandler = nil
		return cn.fail(fmt.Errorf("ble: connect %s: %w", id, err))
	}

	// A GattSession that maintains the connection is what makes the
	// runtime open the link and keep it; without one the link lives only
	// as long as a request is in flight or a notification is subscribed.
	if did, err := dev.GetBluetoothDeviceId(); err != nil {
		c.log.Debug("ble: device id", "peripheral", id, "err", err)
	} else {
		sop, err := gatt.GattSessionFromDeviceIdAsync(did)
		did.Release()
		if err != nil {
			c.log.Debug("ble: opening a session", "peripheral", id, "err", err)
		} else if sp, err := awaitPointer(ctx, sop, gatt.SignatureGattSession); err != nil {
			c.log.Debug("ble: opening a session", "peripheral", id, "err", err)
		} else if sp != nil {
			cn.session = (*gatt.GattSession)(sp)
			if can, err := cn.session.GetCanMaintainConnection(); err == nil && !can {
				c.log.Debug("ble: the session cannot maintain the connection", "peripheral", id)
			} else if err := cn.session.SetMaintainConnection(true); err != nil {
				c.log.Debug("ble: maintaining the connection", "peripheral", id, "err", err)
			}
		}
	}

	if err := cn.discover(ctx); err != nil {
		return cn.fail(err)
	}
	cn.mu.Lock()
	select {
	case <-cn.disconnected:
		// Dropped while the services were being read; the release of the
		// runtime objects follows settled.
		cn.mu.Unlock()
		cn.settle()
		return ErrDisconnected
	default:
	}
	cn.open = true
	cn.mu.Unlock()
	cn.settle()
	go cn.dispatch()
	return nil
}

// deviceFor is the runtime's object for the device at addr. A random
// address has to be named as such; when the type is not known (an
// identifier remembered from an earlier run) the plain lookup is tried,
// then each type in turn.
func (c *central) deviceFor(ctx context.Context, addr uint64, addrType bluetooth.BluetoothAddressType) (*bluetooth.BluetoothLEDevice, error) {
	lookup := func(typed bool, t bluetooth.BluetoothAddressType) (*bluetooth.BluetoothLEDevice, error) {
		var op *foundation.IAsyncOperation
		var err error
		if typed {
			op, err = bluetooth.BluetoothLEDeviceFromBluetoothAddressWithBluetoothAddressTypeAsync(addr, t)
		} else {
			op, err = bluetooth.BluetoothLEDeviceFromBluetoothAddressAsync(addr)
		}
		if err != nil {
			return nil, fmt.Errorf("ble: looking up %s: %w", formatAddress(addr), err)
		}
		p, err := awaitPointer(ctx, op, bluetooth.SignatureBluetoothLEDevice)
		if err != nil {
			return nil, fmt.Errorf("ble: looking up %s: %w", formatAddress(addr), err)
		}
		return (*bluetooth.BluetoothLEDevice)(p), nil
	}
	type attempt struct {
		typed bool
		t     bluetooth.BluetoothAddressType
	}
	attempts := []attempt{{true, addrType}}
	if addrType == bluetooth.BluetoothAddressTypeUnspecified {
		attempts = []attempt{{false, 0}, {true, bluetooth.BluetoothAddressTypePublic}, {true, bluetooth.BluetoothAddressTypeRandom}}
	}
	for _, a := range attempts {
		dev, err := lookup(a.typed, a.t)
		if err != nil {
			return nil, err
		}
		if dev != nil {
			return dev, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNotFound, formatAddress(addr))
}

func (c *central) forget(cn *conn) {
	c.mu.Lock()
	if c.conns[cn.id] == cn {
		delete(c.conns, cn.id)
	}
	c.mu.Unlock()
}

// Close lets go of the adapter and radio; connections stay usable until
// closed themselves.
func (c *central) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	adapter, radio := c.adapter, c.radio
	c.adapter, c.radio = nil, nil
	c.mu.Unlock()
	if radio != nil {
		radio.Release()
	}
	if adapter != nil {
		adapter.Release()
	}
	return nil
}

// -- connection --------------------------------------------------------------

type event struct {
	key  string // canonical characteristic UUID
	data []byte
}

// subscription is one characteristic's notifications: the runtime handler
// and its registration, and the function the values go to.
type subscription struct {
	char    *gatt.GattCharacteristic
	handler *foundation.TypedEventHandler
	token   foundation.EventRegistrationToken
	fn      func([]byte)
}

type conn struct {
	c    *central
	id   string
	name string

	device        *bluetooth.BluetoothLEDevice
	session       *gatt.GattSession
	statusHandler *foundation.TypedEventHandler
	statusToken   foundation.EventRegistrationToken

	// disconnected is closed when the link is gone; settled when Connect
	// is done with the runtime objects, one way or the other, so their
	// release (teardown) never races the opening; torn when they have
	// been released.
	disconnected chan struct{}
	settled      chan struct{}
	settleOnce   sync.Once
	torn         chan struct{}
	events       chan event
	closeOnce    sync.Once

	mu       sync.Mutex
	open     bool
	linkLost bool
	services []*gatt.GattDeviceService
	chars    map[string]*gatt.GattCharacteristic // by canonical characteristic UUID
	subs     map[string]*subscription            // by canonical characteristic UUID
	// io serializes writes, reads and subscriptions, as the other backends
	// do: the unit answers one request at a time.
	io sync.Mutex
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

// settle says Connect is done with the runtime objects, one way or the
// other; the release of a dropped connection waits for it.
func (cn *conn) settle() {
	cn.settleOnce.Do(func() { close(cn.settled) })
}

// fail ends a Connect that did not complete: the link is marked dropped,
// the runtime objects released, and err returned.
func (cn *conn) fail(err error) error {
	cn.dropped(nil)
	cn.settle()
	select {
	case <-cn.torn:
	case <-time.After(teardownWait):
	}
	return err
}

// discover reads the services and their characteristics, fresh from the
// device, which is also what opens the link.
func (cn *conn) discover(ctx context.Context) error {
	op, err := cn.device.GetGattServicesWithCacheModeAsync(bluetooth.BluetoothCacheModeUncached)
	if err != nil {
		return fmt.Errorf("ble: discovering services: %w", err)
	}
	p, err := awaitPointer(ctx, op, gatt.SignatureGattDeviceServicesResult)
	if err != nil {
		return cn.failure("discovering services", err)
	}
	if p == nil {
		return errors.New("ble: discovering services: no result")
	}
	res := (*gatt.GattDeviceServicesResult)(p)
	defer res.Release()
	status, err := res.GetStatus()
	if err != nil {
		return fmt.Errorf("ble: discovering services: %w", err)
	}
	if err := gattStatusError("discovering services", status); err != nil {
		return err
	}
	view, err := res.GetServices()
	if err != nil {
		return fmt.Errorf("ble: discovering services: %w", err)
	}
	if view == nil {
		return nil
	}
	items, err := viewItems(view)
	view.Release()
	if err != nil {
		return fmt.Errorf("ble: discovering services: %w", err)
	}
	for _, item := range items {
		svc := (*gatt.GattDeviceService)(item)
		cn.mu.Lock()
		cn.services = append(cn.services, svc)
		cn.mu.Unlock()
		if err := cn.discoverCharacteristics(ctx, svc); err != nil {
			return err
		}
	}
	return nil
}

func (cn *conn) discoverCharacteristics(ctx context.Context, svc *gatt.GattDeviceService) error {
	op, err := svc.GetCharacteristicsWithCacheModeAsync(bluetooth.BluetoothCacheModeUncached)
	if err != nil {
		return fmt.Errorf("ble: discovering characteristics: %w", err)
	}
	p, err := awaitPointer(ctx, op, gatt.SignatureGattCharacteristicsResult)
	if err != nil {
		return cn.failure("discovering characteristics", err)
	}
	if p == nil {
		return nil
	}
	res := (*gatt.GattCharacteristicsResult)(p)
	defer res.Release()
	status, err := res.GetStatus()
	if err != nil {
		return fmt.Errorf("ble: discovering characteristics: %w", err)
	}
	if err := gattStatusError("discovering characteristics", status); err != nil {
		return err
	}
	view, err := res.GetCharacteristics()
	if err != nil {
		return fmt.Errorf("ble: discovering characteristics: %w", err)
	}
	if view == nil {
		return nil
	}
	items, err := viewItems(view)
	view.Release()
	if err != nil {
		return fmt.Errorf("ble: discovering characteristics: %w", err)
	}
	cn.mu.Lock()
	defer cn.mu.Unlock()
	for _, item := range items {
		ch := (*gatt.GattCharacteristic)(item)
		g, err := ch.GetUuid()
		if err != nil {
			ch.Release()
			continue
		}
		key := guidToUUID(g).Canonical()
		if _, dup := cn.chars[key]; dup {
			ch.Release()
			continue
		}
		cn.chars[key] = ch
	}
	return nil
}

// gattStatusError is the failure a GATT result's status reports, nil for
// success. An unreachable device is a dropped link.
func gattStatusError(what string, status gatt.GattCommunicationStatus) error {
	switch status {
	case gatt.GattCommunicationStatusSuccess:
		return nil
	case gatt.GattCommunicationStatusUnreachable:
		return fmt.Errorf("%w: %s: the device is unreachable", ErrDisconnected, what)
	case gatt.GattCommunicationStatusProtocolError:
		return fmt.Errorf("ble: %s: the device refused (protocol error)", what)
	case gatt.GattCommunicationStatusAccessDenied:
		return fmt.Errorf("ble: %s: access denied", what)
	}
	return fmt.Errorf("ble: %s: status %d", what, status)
}

// failure is an error from the runtime, read as a dropped link when the
// link is known to be down or the error says the device is gone.
func (cn *conn) failure(what string, err error) error {
	select {
	case <-cn.disconnected:
		return ErrDisconnected
	default:
	}
	switch hresult(err) {
	case hrDeviceNotConnected, hrDeviceNotAvailable, hrNotFound:
		return fmt.Errorf("%w: %s: %v", ErrDisconnected, what, err)
	}
	return fmt.Errorf("ble: %s: %w", what, err)
}

func (cn *conn) characteristic(char UUID) (*gatt.GattCharacteristic, error) {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	if !cn.open {
		return nil, ErrDisconnected
	}
	ch, ok := cn.chars[char.Canonical()]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoCharacteristic, char.Short())
	}
	return ch, nil
}

// Write sends data to the characteristic. The service is implied by the
// characteristic on this backend.
func (cn *conn) Write(ctx context.Context, _ UUID, char UUID, data []byte, withResponse bool) (err error) {
	defer guard(&err)
	ch, err := cn.characteristic(char)
	if err != nil {
		return err
	}
	cn.io.Lock()
	defer cn.io.Unlock()
	buf, err := bytesBuffer(data)
	if err != nil {
		return fmt.Errorf("ble: write: %w", err)
	}
	defer buf.Release()
	option := gatt.GattWriteOptionWriteWithResponse
	if !withResponse {
		option = gatt.GattWriteOptionWriteWithoutResponse
	}
	op, err := ch.WriteValueWithOptionAsync(buf, option)
	if err != nil {
		return cn.failure("write", err)
	}
	status, err := awaitValue(ctx, op, gatt.SignatureGattCommunicationStatus)
	if err != nil {
		return cn.failure("write", err)
	}
	return gattStatusError("write", gatt.GattCommunicationStatus(status))
}

// Subscribe turns the characteristic's notifications on and routes each
// value to fn, in order, from one goroutine. The handler is in place
// before the descriptor is written, so no value is missed.
func (cn *conn) Subscribe(ctx context.Context, _ UUID, char UUID, fn func([]byte)) (err error) {
	defer guard(&err)
	ch, err := cn.characteristic(char)
	if err != nil {
		return err
	}
	cn.io.Lock()
	defer cn.io.Unlock()
	key := char.Canonical()
	cn.mu.Lock()
	if prev := cn.subs[key]; prev != nil {
		prev.fn = fn
		cn.mu.Unlock()
		return nil
	}
	cn.mu.Unlock()

	handler := typedHandler(cn.c.log, gatt.SignatureGattCharacteristic, gatt.SignatureGattValueChangedEventArgs, func(_, args unsafe.Pointer) {
		buf, err := (*gatt.GattValueChangedEventArgs)(args).GetCharacteristicValue()
		if err != nil {
			return
		}
		data, err := bufferBytes(buf)
		if buf != nil {
			buf.Release()
		}
		if err != nil {
			return
		}
		select {
		case cn.events <- event{key: key, data: data}:
		default:
			cn.c.log.Warn("ble: notification dropped; the reader is not keeping up", "peripheral", cn.id)
		}
	})
	token, err := ch.AddValueChanged(handler)
	if err != nil {
		handler.Release()
		return cn.failure("subscribe", err)
	}
	sub := &subscription{char: ch, handler: handler, token: token, fn: fn}
	cn.mu.Lock()
	cn.subs[key] = sub
	cn.mu.Unlock()

	value := gatt.GattClientCharacteristicConfigurationDescriptorValueNotify
	if props, err := ch.GetCharacteristicProperties(); err == nil &&
		props&gatt.GattCharacteristicPropertiesNotify == 0 && props&gatt.GattCharacteristicPropertiesIndicate != 0 {
		value = gatt.GattClientCharacteristicConfigurationDescriptorValueIndicate
	}
	var werr error
	if op, err := ch.WriteClientCharacteristicConfigurationDescriptorAsync(value); err != nil {
		werr = cn.failure("subscribe", err)
	} else if status, err := awaitValue(ctx, op, gatt.SignatureGattCommunicationStatus); err != nil {
		werr = cn.failure("subscribe", err)
	} else {
		werr = gattStatusError("subscribe", gatt.GattCommunicationStatus(status))
	}
	if werr != nil {
		cn.mu.Lock()
		delete(cn.subs, key)
		cn.mu.Unlock()
		_ = ch.RemoveValueChanged(token)
		handler.Release()
		return werr
	}
	return nil
}

// Read reads the characteristic's value from the device.
func (cn *conn) Read(ctx context.Context, _ UUID, char UUID) (_ []byte, err error) {
	defer guard(&err)
	ch, err := cn.characteristic(char)
	if err != nil {
		return nil, err
	}
	cn.io.Lock()
	defer cn.io.Unlock()
	op, err := ch.ReadValueWithCacheModeAsync(bluetooth.BluetoothCacheModeUncached)
	if err != nil {
		return nil, cn.failure("read", err)
	}
	p, err := awaitPointer(ctx, op, gatt.SignatureGattReadResult)
	if err != nil {
		return nil, cn.failure("read", err)
	}
	if p == nil {
		return nil, errors.New("ble: read: no result")
	}
	res := (*gatt.GattReadResult)(p)
	defer res.Release()
	status, err := res.GetStatus()
	if err != nil {
		return nil, fmt.Errorf("ble: read: %w", err)
	}
	if err := gattStatusError("read", status); err != nil {
		return nil, err
	}
	buf, err := res.GetValue()
	if err != nil {
		return nil, fmt.Errorf("ble: read: %w", err)
	}
	if buf == nil {
		return []byte{}, nil
	}
	defer buf.Release()
	return bufferBytes(buf)
}

// dispatch delivers notifications to subscribers in order, off the
// runtime's threads.
func (cn *conn) dispatch() {
	deliver := func(ev event) {
		cn.mu.Lock()
		sub := cn.subs[ev.key]
		cn.mu.Unlock()
		if sub != nil && sub.fn != nil {
			sub.fn(ev.data)
		}
	}
	for {
		select {
		case ev := <-cn.events:
			deliver(ev)
		case <-cn.disconnected:
			// Drain what arrived before the drop, then stop.
			for {
				select {
				case ev := <-cn.events:
					deliver(ev)
				default:
					return
				}
			}
		}
	}
}

// Close disconnects: the runtime objects are released, which ends the
// link when nothing else on the computer holds it.
func (cn *conn) Close() error {
	cn.mu.Lock()
	wasOpen := cn.open
	cn.open = false
	cn.mu.Unlock()
	cn.dropped(nil)
	if wasOpen {
		select {
		case <-cn.torn:
		case <-time.After(teardownWait):
			cn.c.log.Debug("ble: disconnect not confirmed", "peripheral", cn.id)
		}
	}
	return nil
}

// dropped marks the link gone: waiters are released, the disconnected
// channel closed, and the runtime objects released once Connect is done
// with them. err is the runtime's word when the device went; nil when
// this side is closing.
func (cn *conn) dropped(err error) {
	cn.closeOnce.Do(func() {
		cn.mu.Lock()
		cn.open = false
		cn.linkLost = err != nil
		cn.mu.Unlock()
		close(cn.disconnected)
		cn.c.forget(cn)
		if err != nil {
			cn.c.log.Debug("ble: disconnected", "peripheral", cn.id, "err", err)
		}
		go func() {
			<-cn.settled
			cn.teardown()
		}()
	})
}

// teardown releases everything the connection holds in the runtime:
// notifications off (when the link is still there to say so), handlers
// removed, characteristics and services released, the session no longer
// maintaining the connection and closed, the device closed.
func (cn *conn) teardown() {
	defer close(cn.torn)
	defer func() {
		if r := recover(); r != nil {
			cn.c.log.Debug("ble: the Windows Runtime failed while the connection was released", "peripheral", cn.id, "panic", r)
		}
	}()
	cn.mu.Lock()
	subs, chars, services, linkLost := cn.subs, cn.chars, cn.services, cn.linkLost
	cn.subs, cn.chars, cn.services = map[string]*subscription{}, map[string]*gatt.GattCharacteristic{}, nil
	cn.mu.Unlock()

	if cn.session != nil {
		_ = cn.session.SetMaintainConnection(false)
	}
	ctx, cancel := context.WithTimeout(context.Background(), unsubscribeWait)
	defer cancel()
	for _, s := range subs {
		_ = s.char.RemoveValueChanged(s.token)
		if !linkLost {
			if op, err := s.char.WriteClientCharacteristicConfigurationDescriptorAsync(gatt.GattClientCharacteristicConfigurationDescriptorValueNone); err == nil {
				_, _ = awaitValue(ctx, op, gatt.SignatureGattCommunicationStatus)
			}
		}
		s.handler.Release()
	}
	for _, ch := range chars {
		ch.Release()
	}
	for _, svc := range services {
		_ = svc.Close()
		svc.Release()
	}
	if cn.statusHandler != nil {
		_ = cn.device.RemoveConnectionStatusChanged(cn.statusToken)
		cn.statusHandler.Release()
		cn.statusHandler = nil
	}
	if cn.session != nil {
		_ = cn.session.Close()
		cn.session.Release()
		cn.session = nil
	}
	if cn.device != nil {
		_ = cn.device.Close()
		cn.device.Release()
		cn.device = nil
	}
}
