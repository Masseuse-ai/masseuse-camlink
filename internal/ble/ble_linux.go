//go:build linux

package ble

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

// The Linux backend speaks to BlueZ (bluetoothd) over the system D-Bus:
// org.bluez.Adapter1 for discovery, org.bluez.Device1 for connections and
// org.bluez.GattCharacteristic1 for writes, reads and notifications
// (delivered as PropertiesChanged signals carrying Value). bluetoothd owns
// the links, so a device another program connected is usable here too.

const (
	bluez           = "org.bluez"
	ifaceAdapter    = "org.bluez.Adapter1"
	ifaceDevice     = "org.bluez.Device1"
	ifaceService    = "org.bluez.GattService1"
	ifaceChar       = "org.bluez.GattCharacteristic1"
	ifaceProperties = "org.freedesktop.DBus.Properties"
	ifaceObjects    = "org.freedesktop.DBus.ObjectManager"
)

type central struct {
	log     *slog.Logger
	bus     *dbus.Conn
	adapter dbus.ObjectPath
	mu      sync.Mutex
	closed  bool
}

// Open connects to the system bus and picks the first powered adapter.
func Open(ctx context.Context, log *slog.Logger) (Central, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	bus, err := dbus.SystemBusPrivate()
	if err != nil {
		return nil, fmt.Errorf("%w: system bus: %v", ErrUnsupported, err)
	}
	if err := bus.Auth(nil); err != nil {
		_ = bus.Close()
		return nil, fmt.Errorf("%w: system bus: %v", ErrUnsupported, err)
	}
	if err := bus.Hello(); err != nil {
		_ = bus.Close()
		return nil, fmt.Errorf("%w: system bus: %v", ErrUnsupported, err)
	}
	c := &central{log: log, bus: bus}
	objects, err := c.managedObjects(ctx)
	if err != nil {
		_ = bus.Close()
		if derr, ok := err.(dbus.Error); ok && strings.Contains(derr.Name, "ServiceUnknown") {
			return nil, fmt.Errorf("%w: bluetoothd is not running", ErrUnsupported)
		}
		return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	var unpowered dbus.ObjectPath
	for path, ifaces := range objects {
		props, ok := ifaces[ifaceAdapter]
		if !ok {
			continue
		}
		if powered, _ := props["Powered"].Value().(bool); powered {
			c.adapter = path
			break
		}
		if unpowered == "" {
			unpowered = path
		}
	}
	if c.adapter == "" {
		_ = bus.Close()
		if unpowered != "" {
			return nil, fmt.Errorf("%w: Bluetooth is switched off", ErrUnavailable)
		}
		return nil, fmt.Errorf("%w: this computer has no Bluetooth adapter", ErrUnsupported)
	}
	return c, nil
}

type objectMap map[dbus.ObjectPath]map[string]map[string]dbus.Variant

func (c *central) managedObjects(ctx context.Context) (objectMap, error) {
	var objects objectMap
	err := c.bus.Object(bluez, "/").CallWithContext(ctx, ifaceObjects+".GetManagedObjects", 0).Store(&objects)
	return objects, err
}

func devicePath(adapter dbus.ObjectPath, address string) dbus.ObjectPath {
	return dbus.ObjectPath(string(adapter) + "/dev_" + strings.ReplaceAll(strings.ToUpper(address), ":", "_"))
}

func advertisementOf(props map[string]dbus.Variant) Advertisement {
	a := Advertisement{}
	a.ID, _ = props["Address"].Value().(string)
	if name, ok := props["Name"].Value().(string); ok {
		a.Name = name
	} else if alias, ok := props["Alias"].Value().(string); ok {
		a.Name = alias
	}
	if uuids, ok := props["UUIDs"].Value().([]string); ok {
		for _, u := range uuids {
			a.Services = append(a.Services, UUID(u))
		}
	}
	if rssi, ok := props["RSSI"].Value().(int16); ok {
		a.RSSI = int(rssi)
	}
	a.Connected, _ = props["Connected"].Value().(bool)
	return a
}

// Scan starts LE discovery and reports the first device match accepts,
// counting devices bluetoothd already knows as well as new ones.
func (c *central) Scan(ctx context.Context, match func(Advertisement) bool) (Advertisement, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return Advertisement{}, errors.New("ble: central is closed")
	}
	c.mu.Unlock()
	signals := make(chan *dbus.Signal, 64)
	c.bus.Signal(signals)
	defer c.bus.RemoveSignal(signals)
	added := dbus.WithMatchInterface(ifaceObjects)
	changed := dbus.WithMatchInterface(ifaceProperties)
	if err := c.bus.AddMatchSignalContext(ctx, added); err != nil {
		return Advertisement{}, fmt.Errorf("ble: %w", err)
	}
	defer func() { _ = c.bus.RemoveMatchSignal(added) }()
	if err := c.bus.AddMatchSignalContext(ctx, changed); err != nil {
		return Advertisement{}, fmt.Errorf("ble: %w", err)
	}
	defer func() { _ = c.bus.RemoveMatchSignal(changed) }()

	adapter := c.bus.Object(bluez, c.adapter)
	filter := map[string]dbus.Variant{"Transport": dbus.MakeVariant("le")}
	if err := adapter.CallWithContext(ctx, ifaceAdapter+".SetDiscoveryFilter", 0, filter).Err; err != nil {
		c.log.Debug("ble: discovery filter", "err", err)
	}
	if err := adapter.CallWithContext(ctx, ifaceAdapter+".StartDiscovery", 0).Err; err != nil {
		if derr, ok := err.(dbus.Error); !ok || !strings.Contains(derr.Name, "InProgress") {
			return Advertisement{}, fmt.Errorf("ble: start discovery: %w", err)
		}
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = adapter.CallWithContext(cctx, ifaceAdapter+".StopDiscovery", 0).Err
	}()

	// Devices bluetoothd already lists (seen a moment ago, or held by
	// another program) come first.
	if objects, err := c.managedObjects(ctx); err == nil {
		for path, ifaces := range objects {
			if props, ok := ifaces[ifaceDevice]; ok && strings.HasPrefix(string(path), string(c.adapter)) {
				if a := advertisementOf(props); a.ID != "" && match(a) {
					return a, nil
				}
			}
		}
	}
	for {
		select {
		case sig := <-signals:
			if sig == nil {
				return Advertisement{}, errors.New("ble: system bus closed")
			}
			var props map[string]dbus.Variant
			switch sig.Name {
			case ifaceObjects + ".InterfacesAdded":
				if len(sig.Body) != 2 {
					continue
				}
				ifaces, _ := sig.Body[1].(map[string]map[string]dbus.Variant)
				props = ifaces[ifaceDevice]
			case ifaceProperties + ".PropertiesChanged":
				if len(sig.Body) < 2 {
					continue
				}
				if iface, _ := sig.Body[0].(string); iface != ifaceDevice {
					continue
				}
				// A change carries only what changed; read the device whole.
				if !strings.HasPrefix(string(sig.Path), string(c.adapter)) {
					continue
				}
				var all map[string]dbus.Variant
				if err := c.bus.Object(bluez, sig.Path).CallWithContext(ctx, ifaceProperties+".GetAll", 0, ifaceDevice).Store(&all); err != nil {
					continue
				}
				props = all
			}
			if props == nil {
				continue
			}
			if a := advertisementOf(props); a.ID != "" && match(a) {
				return a, nil
			}
		case <-ctx.Done():
			return Advertisement{}, ctx.Err()
		}
	}
}

// ConnectedWithService lists devices bluetoothd holds connected that offer
// the service.
func (c *central) ConnectedWithService(ctx context.Context, service UUID) ([]Advertisement, error) {
	objects, err := c.managedObjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("ble: %w", err)
	}
	var out []Advertisement
	for path, ifaces := range objects {
		props, ok := ifaces[ifaceDevice]
		if !ok || !strings.HasPrefix(string(path), string(c.adapter)) {
			continue
		}
		a := advertisementOf(props)
		if a.Connected && a.HasService(service) {
			out = append(out, a)
		}
	}
	return out, nil
}

// Connect connects the device with the address id and waits for its
// services to resolve.
func (c *central) Connect(ctx context.Context, id string) (Conn, error) {
	path := devicePath(c.adapter, id)
	dev := c.bus.Object(bluez, path)
	var props map[string]dbus.Variant
	if err := dev.CallWithContext(ctx, ifaceProperties+".GetAll", 0, ifaceDevice).Store(&props); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	cn := &conn{c: c, path: path, id: id, disconnected: make(chan struct{}), events: make(chan event, 256), chars: map[string]dbus.ObjectPath{}}
	cn.name, _ = props["Name"].Value().(string)
	cn.signals = make(chan *dbus.Signal, 256)
	c.bus.Signal(cn.signals)
	cn.match = dbus.WithMatchInterface(ifaceProperties)
	if err := c.bus.AddMatchSignalContext(ctx, cn.match); err != nil {
		c.bus.RemoveSignal(cn.signals)
		return nil, fmt.Errorf("ble: %w", err)
	}
	go cn.pump()
	if connected, _ := props["Connected"].Value().(bool); !connected {
		if err := dev.CallWithContext(ctx, ifaceDevice+".Connect", 0).Err; err != nil {
			cn.dropped(nil)
			return nil, fmt.Errorf("ble: connect %s: %w", id, err)
		}
	}
	cn.mu.Lock()
	cn.open = true
	cn.mu.Unlock()
	if err := cn.waitResolved(ctx); err != nil {
		_ = cn.Close()
		return nil, err
	}
	if err := cn.discover(ctx); err != nil {
		_ = cn.Close()
		return nil, err
	}
	return cn, nil
}

// Close forgets the central; the bus connection is closed.
func (c *central) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.bus.Close()
}

// -- connection --------------------------------------------------------------

type event struct {
	char dbus.ObjectPath
	data []byte
}

type conn struct {
	c       *central
	path    dbus.ObjectPath
	id      string
	name    string
	signals chan *dbus.Signal
	match   dbus.MatchOption

	disconnected chan struct{}
	events       chan event
	closeOnce    sync.Once

	mu       sync.Mutex
	open     bool
	resolved chan struct{}
	chars    map[string]dbus.ObjectPath // by canonical characteristic UUID
	subs     map[dbus.ObjectPath]func([]byte)
	io       sync.Mutex
}

func (cn *conn) ID() string                    { return cn.id }
func (cn *conn) Name() string                  { return cn.name }
func (cn *conn) Disconnected() <-chan struct{} { return cn.disconnected }

// pump routes PropertiesChanged signals: the device's Connected and
// ServicesResolved flags, and characteristic values to subscribers.
func (cn *conn) pump() {
	for sig := range cn.signals {
		if sig == nil || sig.Name != ifaceProperties+".PropertiesChanged" || len(sig.Body) < 2 {
			continue
		}
		iface, _ := sig.Body[0].(string)
		changed, _ := sig.Body[1].(map[string]dbus.Variant)
		switch {
		case sig.Path == cn.path && iface == ifaceDevice:
			if v, ok := changed["Connected"].Value().(bool); ok && !v {
				cn.dropped(errors.New("device disconnected"))
				return
			}
			if v, ok := changed["ServicesResolved"].Value().(bool); ok && v {
				cn.mu.Lock()
				if cn.resolved != nil {
					select {
					case <-cn.resolved:
					default:
						close(cn.resolved)
					}
				}
				cn.mu.Unlock()
			}
		case iface == ifaceChar && strings.HasPrefix(string(sig.Path), string(cn.path)):
			value, ok := changed["Value"].Value().([]byte)
			if !ok {
				continue
			}
			cn.mu.Lock()
			fn := cn.subs[sig.Path]
			cn.mu.Unlock()
			if fn != nil {
				fn(append([]byte(nil), value...))
			}
		}
	}
}

func (cn *conn) waitResolved(ctx context.Context) error {
	cn.mu.Lock()
	cn.resolved = make(chan struct{})
	resolved := cn.resolved
	cn.mu.Unlock()
	var already bool
	if err := cn.c.bus.Object(bluez, cn.path).CallWithContext(ctx, ifaceProperties+".Get", 0, ifaceDevice, "ServicesResolved").Store(&already); err == nil && already {
		return nil
	}
	select {
	case <-resolved:
		return nil
	case <-cn.disconnected:
		return ErrDisconnected
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (cn *conn) discover(ctx context.Context) error {
	objects, err := cn.c.managedObjects(ctx)
	if err != nil {
		return fmt.Errorf("ble: %w", err)
	}
	cn.mu.Lock()
	defer cn.mu.Unlock()
	for path, ifaces := range objects {
		props, ok := ifaces[ifaceChar]
		if !ok || !strings.HasPrefix(string(path), string(cn.path)+"/") {
			continue
		}
		u, _ := props["UUID"].Value().(string)
		if u == "" {
			continue
		}
		if _, dup := cn.chars[UUID(u).Canonical()]; !dup {
			cn.chars[UUID(u).Canonical()] = path
		}
	}
	return nil
}

func (cn *conn) characteristic(char UUID) (dbus.BusObject, error) {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	if !cn.open {
		return nil, ErrDisconnected
	}
	path, ok := cn.chars[char.Canonical()]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoCharacteristic, char.Short())
	}
	return cn.c.bus.Object(bluez, path), nil
}

func (cn *conn) checkDropped(err error) error {
	if err == nil {
		return nil
	}
	select {
	case <-cn.disconnected:
		return ErrDisconnected
	default:
	}
	if derr, ok := err.(dbus.Error); ok && strings.Contains(derr.Name, "NotConnected") {
		cn.dropped(err)
		return ErrDisconnected
	}
	return fmt.Errorf("ble: %w", err)
}

// Write sends data to the characteristic.
func (cn *conn) Write(ctx context.Context, _ UUID, char UUID, data []byte, withResponse bool) error {
	obj, err := cn.characteristic(char)
	if err != nil {
		return err
	}
	cn.io.Lock()
	defer cn.io.Unlock()
	kind := "command"
	if withResponse {
		kind = "request"
	}
	opts := map[string]dbus.Variant{"type": dbus.MakeVariant(kind)}
	return cn.checkDropped(obj.CallWithContext(ctx, ifaceChar+".WriteValue", 0, data, opts).Err)
}

// Subscribe turns notifications on and routes each value to fn.
func (cn *conn) Subscribe(ctx context.Context, _ UUID, char UUID, fn func([]byte)) error {
	obj, err := cn.characteristic(char)
	if err != nil {
		return err
	}
	cn.io.Lock()
	defer cn.io.Unlock()
	cn.mu.Lock()
	if cn.subs == nil {
		cn.subs = map[dbus.ObjectPath]func([]byte){}
	}
	cn.subs[obj.Path()] = fn
	cn.mu.Unlock()
	if err := obj.CallWithContext(ctx, ifaceChar+".StartNotify", 0).Err; err != nil {
		if derr, ok := err.(dbus.Error); !ok || !strings.Contains(derr.Name, "InProgress") {
			cn.mu.Lock()
			delete(cn.subs, obj.Path())
			cn.mu.Unlock()
			return cn.checkDropped(err)
		}
	}
	return nil
}

// Read reads the characteristic's value.
func (cn *conn) Read(ctx context.Context, _ UUID, char UUID) ([]byte, error) {
	obj, err := cn.characteristic(char)
	if err != nil {
		return nil, err
	}
	cn.io.Lock()
	defer cn.io.Unlock()
	var value []byte
	if err := obj.CallWithContext(ctx, ifaceChar+".ReadValue", 0, map[string]dbus.Variant{}).Store(&value); err != nil {
		return nil, cn.checkDropped(err)
	}
	return value, nil
}

// Close disconnects the device.
func (cn *conn) Close() error {
	cn.mu.Lock()
	wasOpen := cn.open
	cn.open = false
	cn.mu.Unlock()
	if wasOpen {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = cn.c.bus.Object(bluez, cn.path).CallWithContext(ctx, ifaceDevice+".Disconnect", 0).Err
	}
	cn.dropped(nil)
	return nil
}

func (cn *conn) dropped(err error) {
	cn.closeOnce.Do(func() {
		cn.mu.Lock()
		cn.open = false
		cn.mu.Unlock()
		close(cn.disconnected)
		_ = cn.c.bus.RemoveMatchSignal(cn.match)
		// RemoveSignal returns once no delivery is in flight, so closing
		// the channel afterwards is safe and ends pump.
		cn.c.bus.RemoveSignal(cn.signals)
		close(cn.signals)
		if err != nil {
			cn.c.log.Debug("ble: disconnected", "peripheral", cn.id, "err", err)
		}
	})
}
