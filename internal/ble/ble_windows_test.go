//go:build windows

package ble

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-ole/go-ole"
	"github.com/saltosystems/winrt-go"
	"github.com/saltosystems/winrt-go/windows/devices/bluetooth"
	"github.com/saltosystems/winrt-go/windows/devices/bluetooth/advertisement"
	gatt "github.com/saltosystems/winrt-go/windows/devices/bluetooth/genericattributeprofile"
	"github.com/saltosystems/winrt-go/windows/foundation"
)

func TestGUIDToUUID(t *testing.T) {
	g := syscall.GUID{Data1: 0x0000fff0, Data2: 0x0000, Data3: 0x1000, Data4: [8]byte{0x80, 0x00, 0x00, 0x80, 0x5f, 0x9b, 0x34, 0xfb}}
	if got := guidToUUID(g); got.Canonical() != "0000fff0-0000-1000-8000-00805f9b34fb" || got.Short() != "fff0" {
		t.Errorf("guidToUUID = %q (short %q)", got, got.Short())
	}
	if !guidToUUID(g).Equal("FFF0") {
		t.Error("the service UUID should match its 16-bit form")
	}
	custom := syscall.GUID{Data1: 0x6e400001, Data2: 0xb5a3, Data3: 0xf393, Data4: [8]byte{0xe0, 0xa9, 0xe5, 0x0e, 0x24, 0xdc, 0xca, 0x9e}}
	if got := guidToUUID(custom); got.Canonical() != "6e400001-b5a3-f393-e0a9-e50e24dcca9e" {
		t.Errorf("guidToUUID(custom) = %q", got)
	}
}

// The handler interface ids are derived from type signatures; the
// signatures written by hand here (the classes winrt-go does not generate)
// must produce the ids the Windows SDK publishes for the same handlers.
func TestHandlerIIDs(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{
			"TypedEventHandler<BluetoothLEAdvertisementWatcher, BluetoothLEAdvertisementReceivedEventArgs>",
			winrt.ParameterizedInstanceGUID(foundation.GUIDTypedEventHandler, advertisement.SignatureBluetoothLEAdvertisementWatcher, advertisement.SignatureBluetoothLEAdvertisementReceivedEventArgs),
			"90eb4eca-d465-5ea0-a61c-033c8c5ecef2",
		},
		{
			"AsyncOperationCompletedHandler<DeviceInformationCollection>",
			winrt.ParameterizedInstanceGUID(foundation.GUIDAsyncOperationCompletedHandler, sigDeviceInformationCollection),
			"4a458732-527e-5c73-9a68-a73da370f782",
		},
	}
	for _, c := range cases {
		got := strings.ToLower(strings.Trim(c.got, "{}"))
		if got != c.want {
			t.Errorf("%s: iid %s, want %s", c.name, got, c.want)
		}
	}
	// The rest must at least be well-formed GUIDs.
	for _, sig := range []string{sigBluetoothAdapter, sigRadio, sigInspectable, bluetooth.SignatureBluetoothLEDevice, gatt.SignatureGattSession, gatt.SignatureGattCommunicationStatus} {
		if id := winrt.ParameterizedInstanceGUID(foundation.GUIDAsyncOperationCompletedHandler, sig); ole.NewGUID(id) == nil {
			t.Errorf("signature %q: no GUID", sig)
		}
	}
}

func TestWatcherError(t *testing.T) {
	if err := watcherError(bluetooth.BluetoothErrorRadioNotAvailable); !errors.Is(err, ErrUnavailable) {
		t.Errorf("RadioNotAvailable: %v", err)
	}
	if err := watcherError(bluetooth.BluetoothErrorDisabledByUser); !errors.Is(err, ErrUnavailable) {
		t.Errorf("DisabledByUser: %v", err)
	}
	if err := watcherError(bluetooth.BluetoothErrorNotSupported); !errors.Is(err, ErrUnsupported) {
		t.Errorf("NotSupported: %v", err)
	}
	if err := watcherError(bluetooth.BluetoothErrorOtherError); err == nil || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrUnsupported) {
		t.Errorf("OtherError: %v", err)
	}
}

func TestGattStatusError(t *testing.T) {
	if err := gattStatusError("write", gatt.GattCommunicationStatusSuccess); err != nil {
		t.Errorf("Success: %v", err)
	}
	if err := gattStatusError("write", gatt.GattCommunicationStatusUnreachable); !errors.Is(err, ErrDisconnected) {
		t.Errorf("Unreachable: %v", err)
	}
	if err := gattStatusError("write", gatt.GattCommunicationStatusProtocolError); err == nil || errors.Is(err, ErrDisconnected) {
		t.Errorf("ProtocolError: %v", err)
	}
}

func TestHResult(t *testing.T) {
	if got := hresult(ole.NewError(hrNotFound)); got != hrNotFound {
		t.Errorf("hresult = %#x", got)
	}
	if got := hresult(errors.New("plain")); got != 0 {
		t.Errorf("hresult(plain) = %#x", got)
	}
	cn := &conn{disconnected: make(chan struct{})}
	if err := cn.failure("write", ole.NewError(hrDeviceNotConnected)); !errors.Is(err, ErrDisconnected) {
		t.Errorf("device not connected: %v", err)
	}
	if err := cn.failure("write", ole.NewError(0x80004005)); errors.Is(err, ErrDisconnected) {
		t.Errorf("E_FAIL read as a drop: %v", err)
	}
	close(cn.disconnected)
	if err := cn.failure("write", errors.New("anything")); !errors.Is(err, ErrDisconnected) {
		t.Errorf("after the drop: %v", err)
	}
}

// Open on the machine at hand: a computer without a radio (GitHub's
// runners) says ErrUnsupported, one with Bluetooth off says
// ErrUnavailable, one with Bluetooth on opens; none of them hangs. This is
// the Windows Runtime plumbing (the apartment, the activation factories,
// an asynchronous operation awaited) exercised without a device.
func TestOpenWithoutBluetooth(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		start := time.Now()
		c, err := Open(ctx, log)
		elapsed := time.Since(start)
		cancel()
		switch {
		case err == nil:
			t.Logf("open %d: Bluetooth is available on this machine (%s)", i, elapsed)
			if err := c.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		case errors.Is(err, ErrUnsupported):
			t.Logf("open %d: no Bluetooth: %v (%s)", i, err, elapsed)
		case errors.Is(err, ErrUnavailable):
			t.Logf("open %d: Bluetooth not available: %v (%s)", i, err, elapsed)
		default:
			t.Fatalf("open %d: %v", i, err)
		}
		if elapsed > 20*time.Second {
			t.Fatalf("open %d took %s", i, elapsed)
		}
	}
}

// A scan on a machine without Bluetooth never starts: Open refuses first.
// With Bluetooth on, a short scan runs and ends on its deadline.
func TestScanEndsOnDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Open(ctx, nil)
	if err != nil {
		t.Skipf("no Bluetooth here: %v", err)
	}
	defer c.Close()
	sctx, scancel := context.WithTimeout(ctx, 2*time.Second)
	defer scancel()
	start := time.Now()
	_, err = c.Scan(sctx, func(Advertisement) bool { return false })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Scan: %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("Scan took %s to stop", time.Since(start))
	}
	held, err := c.ConnectedWithService(ctx, "fff0")
	if err != nil {
		t.Fatalf("ConnectedWithService: %v", err)
	}
	t.Logf("held peripherals offering fff0: %d", len(held))
}
