//go:build darwin

package awake

import (
	"fmt"
	"sync"

	"github.com/ebitengine/purego"
)

// macOS: an IOKit power assertion of type PreventUserIdleSystemSleep, the
// kind a video call holds: the system does not idle-sleep while it exists,
// the display still may. IOKit and CoreFoundation are dlopen'ed and called
// through purego, so no cgo is needed (internal/ble does the same for
// CoreBluetooth). The assertion is released with the process if Release
// is never called. `pmset -g assertions` lists it under the name given.

const (
	ioKitPath          = "/System/Library/Frameworks/IOKit.framework/IOKit"
	coreFoundationPath = "/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation"

	// kIOPMAssertionTypePreventUserIdleSystemSleep.
	assertionType = "PreventUserIdleSystemSleep"
	// kIOPMAssertionLevelOn.
	assertionLevelOn uint32 = 255
	// kCFStringEncodingUTF8.
	cfStringEncodingUTF8 uint32 = 0x08000100
	// kIOReturnSuccess.
	ioReturnSuccess int32 = 0
	// kIOPMAssertionDetailsKey, kIOPMAssertionHumanReadableReasonKey: what
	// System Settings and `pmset -g assertions` show as the reason.
	detailsKey             = "Details"
	humanReadableReasonKey = "HumanReadableReason"
)

var (
	loadOnce sync.Once
	loadErr  error

	// IOReturn IOPMAssertionCreateWithName(CFStringRef type, IOPMAssertionLevel level, CFStringRef name, IOPMAssertionID *id)
	ioPMAssertionCreateWithName func(assertionType uintptr, level uint32, name uintptr, id *uint32) int32
	// IOReturn IOPMAssertionRelease(IOPMAssertionID id)
	ioPMAssertionRelease func(id uint32) int32
	// IOReturn IOPMAssertionSetProperty(IOPMAssertionID id, CFStringRef property, CFTypeRef value)
	ioPMAssertionSetProperty func(id uint32, property uintptr, value uintptr) int32
	// CFStringRef CFStringCreateWithCString(CFAllocatorRef alloc, const char *s, CFStringEncoding encoding)
	cfStringCreateWithCString func(alloc uintptr, s string, encoding uint32) uintptr
	// void CFRelease(CFTypeRef cf)
	cfRelease func(cf uintptr)
)

func load() error {
	loadOnce.Do(func() {
		loadErr = func() error {
			cf, err := purego.Dlopen(coreFoundationPath, purego.RTLD_GLOBAL|purego.RTLD_NOW)
			if err != nil {
				return fmt.Errorf("awake: %w", err)
			}
			iokit, err := purego.Dlopen(ioKitPath, purego.RTLD_GLOBAL|purego.RTLD_NOW)
			if err != nil {
				return fmt.Errorf("awake: %w", err)
			}
			purego.RegisterLibFunc(&cfStringCreateWithCString, cf, "CFStringCreateWithCString")
			purego.RegisterLibFunc(&cfRelease, cf, "CFRelease")
			purego.RegisterLibFunc(&ioPMAssertionCreateWithName, iokit, "IOPMAssertionCreateWithName")
			purego.RegisterLibFunc(&ioPMAssertionRelease, iokit, "IOPMAssertionRelease")
			purego.RegisterLibFunc(&ioPMAssertionSetProperty, iokit, "IOPMAssertionSetProperty")
			return nil
		}()
	})
	return loadErr
}

// cfString makes a CFString the caller releases.
func cfString(s string) (uintptr, error) {
	ref := cfStringCreateWithCString(0, s, cfStringEncodingUTF8)
	if ref == 0 {
		return 0, fmt.Errorf("awake: CFStringCreateWithCString(%q) returned NULL", s)
	}
	return ref, nil
}

func take(name, reason string) (func() error, error) {
	if err := load(); err != nil {
		return nil, err
	}
	typeRef, err := cfString(assertionType)
	if err != nil {
		return nil, err
	}
	defer cfRelease(typeRef)
	nameRef, err := cfString(name)
	if err != nil {
		return nil, err
	}
	defer cfRelease(nameRef)
	var id uint32
	if ret := ioPMAssertionCreateWithName(typeRef, assertionLevelOn, nameRef, &id); ret != ioReturnSuccess {
		return nil, fmt.Errorf("awake: IOPMAssertionCreateWithName: IOReturn 0x%08x", uint32(ret))
	}
	// The reason is for the person reading System Settings or pmset; a
	// property that cannot be set changes nothing about the hold.
	for _, key := range []string{detailsKey, humanReadableReasonKey} {
		keyRef, kerr := cfString(key)
		valRef, verr := cfString(reason)
		if kerr == nil && verr == nil {
			_ = ioPMAssertionSetProperty(id, keyRef, valRef)
		}
		if kerr == nil {
			cfRelease(keyRef)
		}
		if verr == nil {
			cfRelease(valRef)
		}
	}
	return func() error {
		if ret := ioPMAssertionRelease(id); ret != ioReturnSuccess {
			return fmt.Errorf("awake: IOPMAssertionRelease: IOReturn 0x%08x", uint32(ret))
		}
		return nil
	}, nil
}
