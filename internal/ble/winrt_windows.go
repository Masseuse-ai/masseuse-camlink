//go:build windows

package ble

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/saltosystems/winrt-go"
	"github.com/saltosystems/winrt-go/windows/devices/bluetooth"
	"github.com/saltosystems/winrt-go/windows/devices/bluetooth/advertisement"
	"github.com/saltosystems/winrt-go/windows/foundation"
	"github.com/saltosystems/winrt-go/windows/foundation/collections"
	"github.com/saltosystems/winrt-go/windows/storage/streams"
)

// The glue between Go and the Windows Runtime: the apartment, waiting on
// asynchronous operations, event handlers, buffers, and the handful of
// WinRT interfaces winrt-go's generated set leaves out, called by their
// vtable slot the way the generated code calls the rest. The slot orders
// below are the interfaces' declared method orders from the Windows
// metadata (winrt-go's generator, run on the same metadata, produces the
// same structs).

// HRESULTs the backend tells apart.
const (
	hrSFalse             uintptr = 0x1
	hrRPCChangedMode     uintptr = 0x80010106 // RPC_E_CHANGED_MODE: the thread is in another apartment already
	hrClassNotReg        uintptr = 0x80040154 // REGDB_E_CLASSNOTREG: this Windows has no such class
	hrNoInterface        uintptr = 0x80004002 // E_NOINTERFACE
	hrDeviceNotConnected uintptr = 0x8007048F // ERROR_DEVICE_NOT_CONNECTED
	hrNotFound           uintptr = 0x80070490 // ERROR_NOT_FOUND: what GATT calls return once the device is gone
	hrDeviceNotAvailable uintptr = 0x800710DF // ERROR_DEVICE_NOT_AVAILABLE
)

// The WinRT type-system signature of Object (IInspectable), the argument
// type of events that carry no arguments.
const sigInspectable = "cinterface(IInspectable)"

var (
	winrtOnce sync.Once
	winrtErr  error
)

// initWinRT joins the process to the multithreaded COM apartment once,
// from an OS thread parked for the life of the process: an apartment lives
// as long as a thread that entered it, and goroutines run on whichever
// thread the scheduler has, all of them implicitly in the multithreaded
// apartment once it exists.
func initWinRT() error {
	winrtOnce.Do(func() {
		done := make(chan error, 1)
		go func() {
			runtime.LockOSThread()
			err := ole.RoInitialize(1) // RO_INIT_MULTITHREADED
			if err != nil {
				switch hresult(err) {
				case hrSFalse, hrRPCChangedMode:
					err = nil
				}
			}
			done <- err
			if err != nil {
				return
			}
			select {} // parked: the apartment stays open
		}()
		winrtErr = <-done
	})
	return winrtErr
}

// hresult is the HRESULT behind a COM error, 0 for any other error.
func hresult(err error) uintptr {
	var oe *ole.OleError
	if errors.As(err, &oe) {
		return oe.Code()
	}
	return 0
}

// call invokes a COM method by its vtable slot; a failing HRESULT is an
// *ole.OleError.
func call(method uintptr, args ...uintptr) error {
	hr, _, _ := syscall.SyscallN(method, args...)
	if hr != 0 {
		return ole.NewError(hr)
	}
	return nil
}

// vtable reads an object's vtable as the struct T laid out for its
// interface.
func vtable[T any](obj *ole.IUnknown) *T {
	return (*T)(unsafe.Pointer(obj.RawVTable))
}

// queryInterface asks obj for the interface iid; the caller releases the
// result.
func queryInterface(obj *ole.IUnknown, iid string) (*ole.IUnknown, error) {
	var out *ole.IUnknown
	if err := obj.PutQueryInterface(ole.NewGUID(iid), &out); err != nil {
		return nil, err
	}
	if out == nil {
		return nil, ole.NewError(hrNoInterface)
	}
	return out, nil
}

// stringOut calls a method whose only output is an HSTRING.
func stringOut(method uintptr, this *ole.IUnknown, in ...uintptr) (string, error) {
	var h ole.HString
	args := append([]uintptr{uintptr(unsafe.Pointer(this))}, in...)
	args = append(args, uintptr(unsafe.Pointer(&h)))
	if err := call(method, args...); err != nil {
		return "", err
	}
	s := h.String()
	_ = ole.DeleteHString(h)
	return s, nil
}

// boolOut calls a method whose only output is a boolean.
func boolOut(method uintptr, this *ole.IUnknown) (bool, error) {
	var out bool
	err := call(method, uintptr(unsafe.Pointer(this)), uintptr(unsafe.Pointer(&out)))
	return out, err
}

// int32Out calls a method whose only output is a 32-bit value (an enum).
func int32Out(method uintptr, this *ole.IUnknown, in ...uintptr) (int32, error) {
	var out int32
	args := append([]uintptr{uintptr(unsafe.Pointer(this))}, in...)
	args = append(args, uintptr(unsafe.Pointer(&out)))
	err := call(method, args...)
	return out, err
}

// asyncOut calls a method whose only output is an asynchronous operation.
func asyncOut(method uintptr, this *ole.IUnknown, in ...uintptr) (*foundation.IAsyncOperation, error) {
	var out *foundation.IAsyncOperation
	args := append([]uintptr{uintptr(unsafe.Pointer(this))}, in...)
	args = append(args, uintptr(unsafe.Pointer(&out)))
	if err := call(method, args...); err != nil {
		return nil, err
	}
	return out, nil
}

// guidToUUID writes a GUID in the canonical UUID form.
func guidToUUID(g syscall.GUID) UUID {
	return UUID(fmt.Sprintf("%08x-%04x-%04x-%02x%02x-%02x%02x%02x%02x%02x%02x",
		g.Data1, g.Data2, g.Data3, g.Data4[0], g.Data4[1], g.Data4[2], g.Data4[3], g.Data4[4], g.Data4[5], g.Data4[6], g.Data4[7]))
}

// -- asynchronous operations ------------------------------------------------

// asyncInfo is the operation's IAsyncInfo, released by the caller.
func asyncInfo(op *foundation.IAsyncOperation) (*foundation.IAsyncInfo, error) {
	itf, err := queryInterface(&op.IUnknown, foundation.GUIDIAsyncInfo)
	if err != nil {
		return nil, err
	}
	return (*foundation.IAsyncInfo)(unsafe.Pointer(itf)), nil
}

// await waits for a WinRT asynchronous operation to complete. signature is
// the operation's result type in WinRT's signature language: it names the
// completed-handler interface (AsyncOperationCompletedHandler<T>) the
// operation accepts. When ctx ends first the operation is cancelled and
// ctx's error returned. The operation itself is not released.
func await(ctx context.Context, op *foundation.IAsyncOperation, signature string) error {
	if op == nil {
		return errors.New("ble: no operation to wait for")
	}
	done := make(chan foundation.AsyncStatus, 1)
	iid := ole.NewGUID(winrt.ParameterizedInstanceGUID(foundation.GUIDAsyncOperationCompletedHandler, signature))
	handler := foundation.NewAsyncOperationCompletedHandler(iid, func(_ *foundation.AsyncOperationCompletedHandler, _ *foundation.IAsyncOperation, status foundation.AsyncStatus) {
		select {
		case done <- status:
		default:
		}
	})
	defer handler.Release()
	if err := op.SetCompleted(handler); err != nil {
		return fmt.Errorf("ble: waiting on the operation: %w", err)
	}
	var status foundation.AsyncStatus
	select {
	case status = <-done:
	case <-ctx.Done():
		if info, err := asyncInfo(op); err == nil {
			_ = info.Cancel()
			info.Release()
		}
		return ctx.Err()
	}
	switch status {
	case foundation.AsyncStatusCompleted:
		return nil
	case foundation.AsyncStatusCanceled:
		return errors.New("ble: the operation was cancelled")
	}
	return asyncError(op)
}

// asyncError is the failure a completed operation reports.
func asyncError(op *foundation.IAsyncOperation) error {
	info, err := asyncInfo(op)
	if err != nil {
		return errors.New("ble: the operation failed")
	}
	defer info.Release()
	code, err := info.GetErrorCode()
	if err != nil || code.Value == 0 {
		return errors.New("ble: the operation failed")
	}
	return ole.NewError(uintptr(uint32(code.Value)))
}

// releaseAsync closes and releases a finished operation.
func releaseAsync(op *foundation.IAsyncOperation) {
	if op == nil {
		return
	}
	if info, err := asyncInfo(op); err == nil {
		_ = info.Close()
		info.Release()
	}
	op.Release()
}

// awaitPointer waits for an operation whose result is an object and hands
// the object back (nil when the operation completed with none); the
// operation is released.
func awaitPointer(ctx context.Context, op *foundation.IAsyncOperation, signature string) (unsafe.Pointer, error) {
	if op == nil {
		return nil, errors.New("ble: no operation to wait for")
	}
	defer releaseAsync(op)
	if err := await(ctx, op, signature); err != nil {
		return nil, err
	}
	p, err := op.GetResults()
	if err != nil {
		return nil, fmt.Errorf("ble: reading the result: %w", err)
	}
	return p, nil
}

// awaitValue waits for an operation whose result is a value (an enum) and
// hands the value back; the operation is released.
func awaitValue(ctx context.Context, op *foundation.IAsyncOperation, signature string) (uintptr, error) {
	p, err := awaitPointer(ctx, op, signature)
	return uintptr(p), err
}

// -- events -------------------------------------------------------------------

// typedHandler is a TypedEventHandler<TSender, TArgs> whose two type
// arguments are named by their signatures. fn runs on a Windows thread-pool
// thread: it copies what it needs and hands off. A panic in it is logged
// and swallowed rather than unwound into the runtime's frames.
func typedHandler(log *slog.Logger, senderSig, argsSig string, fn func(sender, args unsafe.Pointer)) *foundation.TypedEventHandler {
	iid := ole.NewGUID(winrt.ParameterizedInstanceGUID(foundation.GUIDTypedEventHandler, senderSig, argsSig))
	return foundation.NewTypedEventHandler(iid, func(_ *foundation.TypedEventHandler, sender, args unsafe.Pointer) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("ble: panic in a Bluetooth event handler", "panic", r)
			}
		}()
		fn(sender, args)
	})
}

// -- buffers --------------------------------------------------------------------

// bufferBytes copies a WinRT buffer's contents out.
func bufferBytes(buf *streams.IBuffer) ([]byte, error) {
	if buf == nil {
		return nil, nil
	}
	n, err := buf.GetLength()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return []byte{}, nil
	}
	reader, err := streams.DataReaderFromBuffer(buf)
	if err != nil {
		return nil, err
	}
	defer reader.Release()
	return reader.ReadBytes(n)
}

// bytesBuffer copies data into a WinRT buffer the caller releases.
func bytesBuffer(data []byte) (*streams.IBuffer, error) {
	w, err := streams.NewDataWriter()
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = w.Close()
		w.Release()
	}()
	if len(data) > 0 {
		if err := w.WriteBytes(uint32(len(data)), data); err != nil {
			return nil, err
		}
	}
	return w.DetachBuffer()
}

// -- collections ---------------------------------------------------------------

// vectorUUIDs reads an IVector<Guid> (the advertised service UUIDs). The
// generic GetAt of the generated binding writes the element into a
// pointer-sized slot, too small for a GUID, so the slot is called here
// with a GUID to write into.
func vectorUUIDs(v *collections.IVector) ([]UUID, error) {
	n, err := v.GetSize()
	if err != nil {
		return nil, err
	}
	out := make([]UUID, 0, n)
	for i := uint32(0); i < n; i++ {
		var g syscall.GUID
		if err := call(v.VTable().GetAt, uintptr(unsafe.Pointer(v)), uintptr(i), uintptr(unsafe.Pointer(&g))); err != nil {
			return out, err
		}
		out = append(out, guidToUUID(g))
	}
	return out, nil
}

// viewItems reads an IVectorView of objects; each item is the caller's to
// release.
func viewItems(v *collections.IVectorView) ([]unsafe.Pointer, error) {
	n, err := v.GetSize()
	if err != nil {
		return nil, err
	}
	out := make([]unsafe.Pointer, 0, n)
	for i := uint32(0); i < n; i++ {
		p, err := v.GetAt(i)
		if err != nil {
			return out, err
		}
		if p != nil {
			out = append(out, p)
		}
	}
	return out, nil
}

// -- Windows.Devices.Bluetooth.BluetoothAdapter ------------------------------------

const (
	classBluetoothAdapter       = "Windows.Devices.Bluetooth.BluetoothAdapter"
	guidBluetoothAdapterStatics = "8b02fb6a-ac4c-4741-8661-8eab7d17ea9f"
	guidBluetoothAdapter        = "7974f04c-5f7a-4a34-9225-a855f84b1a8b"
	sigBluetoothAdapter         = "rc(Windows.Devices.Bluetooth.BluetoothAdapter;{7974f04c-5f7a-4a34-9225-a855f84b1a8b})"
)

type iBluetoothAdapterStaticsVtbl struct {
	ole.IInspectableVtbl
	GetDeviceSelector uintptr
	FromIdAsync       uintptr
	GetDefaultAsync   uintptr
}

type iBluetoothAdapterVtbl struct {
	ole.IInspectableVtbl
	GetDeviceId                        uintptr
	GetBluetoothAddress                uintptr
	GetIsClassicSupported              uintptr
	GetIsLowEnergySupported            uintptr
	GetIsPeripheralRoleSupported       uintptr
	GetIsCentralRoleSupported          uintptr
	GetIsAdvertisementOffloadSupported uintptr
	GetRadioAsync                      uintptr
}

// bluetoothAdapterGetDefaultAsync starts the lookup of the computer's
// Bluetooth adapter: the result is the adapter, or none.
func bluetoothAdapterGetDefaultAsync() (*foundation.IAsyncOperation, error) {
	f, err := ole.RoGetActivationFactory(classBluetoothAdapter, ole.NewGUID(guidBluetoothAdapterStatics))
	if err != nil {
		return nil, err
	}
	defer f.Release()
	return asyncOut(vtable[iBluetoothAdapterStaticsVtbl](&f.IUnknown).GetDefaultAsync, &f.IUnknown)
}

// adapterSupportsCentral says whether the adapter can act as a Low Energy
// central. An adapter that cannot say is taken to.
func adapterSupportsCentral(adapter *ole.IUnknown) bool {
	vt := vtable[iBluetoothAdapterVtbl](adapter)
	if le, err := boolOut(vt.GetIsLowEnergySupported, adapter); err == nil && !le {
		return false
	}
	if central, err := boolOut(vt.GetIsCentralRoleSupported, adapter); err == nil && !central {
		return false
	}
	return true
}

// adapterGetRadioAsync starts the lookup of the adapter's radio.
func adapterGetRadioAsync(adapter *ole.IUnknown) (*foundation.IAsyncOperation, error) {
	return asyncOut(vtable[iBluetoothAdapterVtbl](adapter).GetRadioAsync, adapter)
}

// -- Windows.Devices.Radios.Radio ------------------------------------------------------

const sigRadio = "rc(Windows.Devices.Radios.Radio;{252118df-b33e-416a-875f-1cf38ae2d83e})"

type iRadioVtbl struct {
	ole.IInspectableVtbl
	SetStateAsync      uintptr
	AddStateChanged    uintptr
	RemoveStateChanged uintptr
	GetState           uintptr
	GetName            uintptr
	GetKind            uintptr
}

// radioState is Windows.Devices.Radios.RadioState.
type radioState int32

const (
	radioStateUnknown  radioState = 0
	radioStateOn       radioState = 1
	radioStateOff      radioState = 2
	radioStateDisabled radioState = 3
)

func radioGetState(radio *ole.IUnknown) (radioState, error) {
	v, err := int32Out(vtable[iRadioVtbl](radio).GetState, radio)
	return radioState(v), err
}

// -- Windows.Devices.Enumeration.DeviceInformation ------------------------------------

const (
	classDeviceInformation       = "Windows.Devices.Enumeration.DeviceInformation"
	guidDeviceInformationStatics = "c17f100e-3a46-4a78-8013-769dc9b97390"
	guidDeviceInformation        = "aba0fb95-4398-489d-8e44-e6130927011f"
	// The signature of DeviceInformationCollection, whose default interface
	// is IVectorView<DeviceInformation>.
	sigDeviceInformationCollection = "rc(Windows.Devices.Enumeration.DeviceInformationCollection;pinterface({bbe1fa4c-b0e3-4583-baef-1f1b2e483e56};rc(Windows.Devices.Enumeration.DeviceInformation;{aba0fb95-4398-489d-8e44-e6130927011f})))"
)

type iDeviceInformationStaticsVtbl struct {
	ole.IInspectableVtbl
	CreateFromIdAsync                             uintptr
	CreateFromIdAsyncAdditionalProperties         uintptr
	FindAllAsync                                  uintptr
	FindAllAsyncDeviceClass                       uintptr
	FindAllAsyncAqsFilter                         uintptr
	FindAllAsyncAqsFilterAndAdditionalProperties  uintptr
	CreateWatcher                                 uintptr
	CreateWatcherDeviceClass                      uintptr
	CreateWatcherAqsFilter                        uintptr
	CreateWatcherAqsFilterAndAdditionalProperties uintptr
}

type iDeviceInformationVtbl struct {
	ole.IInspectableVtbl
	GetId                  uintptr
	GetName                uintptr
	GetIsEnabled           uintptr
	GetIsDefault           uintptr
	GetEnclosureLocation   uintptr
	GetProperties          uintptr
	Update                 uintptr
	GetThumbnailAsync      uintptr
	GetGlyphThumbnailAsync uintptr
}

// deviceInformationFindAllAsync starts an enumeration of the devices an
// Advanced Query Syntax selector matches; the result is a
// DeviceInformationCollection.
func deviceInformationFindAllAsync(selector string) (*foundation.IAsyncOperation, error) {
	f, err := ole.RoGetActivationFactory(classDeviceInformation, ole.NewGUID(guidDeviceInformationStatics))
	if err != nil {
		return nil, err
	}
	defer f.Release()
	h, err := ole.NewHString(selector)
	if err != nil {
		return nil, err
	}
	defer func() { _ = ole.DeleteHString(h) }()
	return asyncOut(vtable[iDeviceInformationStaticsVtbl](&f.IUnknown).FindAllAsyncAqsFilter, &f.IUnknown, uintptr(h))
}

// deviceInformationIDName reads a DeviceInformation's identifier and name.
func deviceInformationIDName(item *ole.IUnknown) (id, name string, err error) {
	itf, err := queryInterface(item, guidDeviceInformation)
	if err != nil {
		return "", "", err
	}
	defer itf.Release()
	vt := vtable[iDeviceInformationVtbl](itf)
	if id, err = stringOut(vt.GetId, itf); err != nil {
		return "", "", err
	}
	name, _ = stringOut(vt.GetName, itf)
	return id, name, nil
}

// -- Windows.Devices.Bluetooth.BluetoothLEDevice, beyond the generated set -------------

type iBluetoothLEDeviceVtbl struct {
	ole.IInspectableVtbl
	GetDeviceId                   uintptr
	GetName                       uintptr
	GetGattServices               uintptr
	GetConnectionStatus           uintptr
	GetBluetoothAddress           uintptr
	GetGattService                uintptr
	AddNameChanged                uintptr
	RemoveNameChanged             uintptr
	AddGattServicesChanged        uintptr
	RemoveGattServicesChanged     uintptr
	AddConnectionStatusChanged    uintptr
	RemoveConnectionStatusChanged uintptr
}

type iBluetoothLEDeviceStatics2Vtbl struct {
	ole.IInspectableVtbl
	GetDeviceSelectorFromPairingState                             uintptr
	GetDeviceSelectorFromConnectionStatus                         uintptr
	GetDeviceSelectorFromDeviceName                               uintptr
	GetDeviceSelectorFromBluetoothAddress                         uintptr
	GetDeviceSelectorFromBluetoothAddressWithBluetoothAddressType uintptr
	GetDeviceSelectorFromAppearance                               uintptr
	FromBluetoothAddressWithBluetoothAddressTypeAsync             uintptr
}

// deviceName is the system's name for the device (its cached GAP name, or
// the name it advertised).
func deviceName(dev *bluetooth.BluetoothLEDevice) (string, error) {
	itf, err := queryInterface(&dev.IUnknown, bluetooth.GUIDiBluetoothLEDevice)
	if err != nil {
		return "", err
	}
	defer itf.Release()
	return stringOut(vtable[iBluetoothLEDeviceVtbl](itf).GetName, itf)
}

// connectedDeviceSelector is the system's own selector for the Low Energy
// devices it holds a connection to.
func connectedDeviceSelector() (string, error) {
	f, err := ole.RoGetActivationFactory("Windows.Devices.Bluetooth.BluetoothLEDevice", ole.NewGUID(bluetooth.GUIDiBluetoothLEDeviceStatics2))
	if err != nil {
		return "", err
	}
	defer f.Release()
	return stringOut(vtable[iBluetoothLEDeviceStatics2Vtbl](&f.IUnknown).GetDeviceSelectorFromConnectionStatus, &f.IUnknown, uintptr(bluetooth.BluetoothConnectionStatusConnected))
}

// connectedDeviceSelectorFallback is what connectedDeviceSelector returns
// on the systems seen so far, for when the call itself fails.
const connectedDeviceSelectorFallback = `System.Devices.DevObjectType:=5 AND System.Devices.Aep.ProtocolId:="{BB7BB05E-5972-42B5-94FC-76EAA7084D49}" AND System.Devices.Aep.IsConnected:=System.StructuredQueryType.Boolean#True`

// -- BluetoothLEAdvertisementReceivedEventArgs, beyond the generated set -----------------

type iBluetoothLEAdvertisementReceivedEventArgs2Vtbl struct {
	ole.IInspectableVtbl
	GetBluetoothAddressType    uintptr
	GetTransmitPowerLevelInDBm uintptr
	GetIsAnonymous             uintptr
	GetIsConnectable           uintptr
	GetIsScannable             uintptr
	GetIsDirected              uintptr
	GetIsScanResponse          uintptr
}

// receivedAddressType is the address type of an advertisement's sender
// (Windows 10 1809 and newer; older systems do not say, and ok is false).
func receivedAddressType(args *advertisement.BluetoothLEAdvertisementReceivedEventArgs) (bluetooth.BluetoothAddressType, bool) {
	itf, err := queryInterface(&args.IUnknown, advertisement.GUIDiBluetoothLEAdvertisementReceivedEventArgs2)
	if err != nil {
		return bluetooth.BluetoothAddressTypeUnspecified, false
	}
	defer itf.Release()
	v, err := int32Out(vtable[iBluetoothLEAdvertisementReceivedEventArgs2Vtbl](itf).GetBluetoothAddressType, itf)
	if err != nil {
		return bluetooth.BluetoothAddressTypeUnspecified, false
	}
	return bluetooth.BluetoothAddressType(v), true
}
