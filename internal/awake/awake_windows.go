//go:build windows

package awake

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows: a power request of type PowerRequestSystemRequired
// (PowerCreateRequest, PowerSetRequest), which keeps the system from
// idle-sleeping while the display may still turn off. It belongs to the
// process and is cleared when the handle is closed or the process ends;
// `powercfg /requests` lists it with its reason. This is the modern form
// of SetThreadExecutionState, which is per thread and would tie the hold to
// one locked OS thread.

const (
	// POWER_REQUEST_CONTEXT_VERSION (DIAGNOSTIC_REASON_VERSION).
	powerRequestContextVersion uint32 = 0
	// POWER_REQUEST_CONTEXT_SIMPLE_STRING.
	powerRequestContextSimpleString uint32 = 0x1
	// PowerRequestSystemRequired (POWER_REQUEST_TYPE).
	powerRequestSystemRequired uintptr = 1
)

// reasonContext is REASON_CONTEXT with the union's SimpleReasonString
// member; the padding is the rest of the union's larger member.
type reasonContext struct {
	version            uint32
	flags              uint32
	simpleReasonString *uint16
	_                  [2]uintptr
}

var (
	kernel32           = windows.NewLazySystemDLL("kernel32.dll")
	powerCreateRequest = kernel32.NewProc("PowerCreateRequest")
	powerSetRequest    = kernel32.NewProc("PowerSetRequest")
	powerClearRequest  = kernel32.NewProc("PowerClearRequest")
)

func take(name, reason string) (func() error, error) {
	for _, p := range []*windows.LazyProc{powerCreateRequest, powerSetRequest, powerClearRequest} {
		if err := p.Find(); err != nil {
			return nil, fmt.Errorf("awake: %w", err)
		}
	}
	text, err := windows.UTF16PtrFromString(name + ": " + reason)
	if err != nil {
		return nil, fmt.Errorf("awake: %w", err)
	}
	ctx := &reasonContext{version: powerRequestContextVersion, flags: powerRequestContextSimpleString, simpleReasonString: text}
	h, _, callErr := powerCreateRequest.Call(uintptr(unsafe.Pointer(ctx)))
	runtime.KeepAlive(ctx)
	handle := windows.Handle(h)
	if handle == windows.InvalidHandle {
		return nil, fmt.Errorf("awake: PowerCreateRequest: %w", callErr)
	}
	if ok, _, callErr := powerSetRequest.Call(uintptr(handle), powerRequestSystemRequired); ok == 0 {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("awake: PowerSetRequest: %w", callErr)
	}
	return func() error {
		ok, _, callErr := powerClearRequest.Call(uintptr(handle), powerRequestSystemRequired)
		closeErr := windows.CloseHandle(handle)
		if ok == 0 {
			return fmt.Errorf("awake: PowerClearRequest: %w", callErr)
		}
		if closeErr != nil {
			return fmt.Errorf("awake: %w", closeErr)
		}
		return nil
	}, nil
}
