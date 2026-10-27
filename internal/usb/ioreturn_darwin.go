//go:build darwin

package usb

import (
	"errors"
	"fmt"
)

// IOReturn values (IOKit/IOReturn.h, IOKit/usb/USB.h).
const (
	kIOReturnSuccess         = 0
	kIOReturnError           = 0xe00002bc
	kIOReturnNoResources     = 0xe00002be
	kIOReturnNoDevice        = 0xe00002c0
	kIOReturnNotPrivileged   = 0xe00002c1
	kIOReturnBadArgument     = 0xe00002c2
	kIOReturnExclusiveAccess = 0xe00002c5
	kIOReturnNotOpen         = 0xe00002cd
	kIOReturnBusy            = 0xe00002d5
	kIOReturnTimeout         = 0xe00002d6
	kIOReturnOffline         = 0xe00002d7
	kIOReturnNotPermitted    = 0xe00002e2
	kIOReturnUnderrun        = 0xe00002e7
	kIOReturnOverrun         = 0xe00002e8
	kIOReturnAborted         = 0xe00002eb
	kIOReturnNotResponding   = 0xe00002ed

	kIOUSBPipeStalled         = 0xe000404f
	kIOUSBTransactionReturned = 0xe0004050
	kIOUSBTransactionTimeout  = 0xe0004051
	kIOUSBUnknownPipeErr      = 0xe0004061
)

// ioReturnError is a raw IOKit status code.
type ioReturnError uint32

func (e ioReturnError) Error() string {
	name := map[uint32]string{
		kIOReturnError: "kIOReturnError", kIOReturnNoResources: "kIOReturnNoResources",
		kIOReturnNoDevice: "kIOReturnNoDevice", kIOReturnNotPrivileged: "kIOReturnNotPrivileged",
		kIOReturnBadArgument: "kIOReturnBadArgument", kIOReturnExclusiveAccess: "kIOReturnExclusiveAccess",
		kIOReturnNotOpen: "kIOReturnNotOpen", kIOReturnBusy: "kIOReturnBusy", kIOReturnTimeout: "kIOReturnTimeout",
		kIOReturnOffline: "kIOReturnOffline", kIOReturnNotPermitted: "kIOReturnNotPermitted",
		kIOReturnUnderrun: "kIOReturnUnderrun", kIOReturnOverrun: "kIOReturnOverrun", kIOReturnAborted: "kIOReturnAborted",
		kIOReturnNotResponding: "kIOReturnNotResponding", kIOUSBPipeStalled: "kIOUSBPipeStalled",
		kIOUSBTransactionReturned: "kIOUSBTransactionReturned", kIOUSBTransactionTimeout: "kIOUSBTransactionTimeout",
		kIOUSBUnknownPipeErr: "kIOUSBUnknownPipeErr",
	}[uint32(e)]
	if name == "" {
		return fmt.Sprintf("IOReturn 0x%08x", uint32(e))
	}
	return fmt.Sprintf("%s (0x%08x)", name, uint32(e))
}

// ioReturn wraps a non-zero status (nil for kIOReturnSuccess).
func ioReturn(kr uint32) error {
	if kr == kIOReturnSuccess {
		return nil
	}
	return ioReturnError(kr)
}

// mapIOReturn turns an IOKit transfer status into the package's sentinel
// errors. Codes that can mean either an unplug or a transient bus error
// (aborted, not responding) are left generic here; the port re-checks the
// registry to tell them apart.
func mapIOReturn(what string, kr uint32) error {
	err := ioReturnError(kr)
	switch kr {
	case kIOReturnSuccess:
		return nil
	case kIOUSBTransactionTimeout, kIOReturnTimeout:
		return fmt.Errorf("%w: %s", ErrTimeout, what)
	case kIOUSBPipeStalled:
		return fmt.Errorf("%w: %s", ErrStall, what)
	case kIOReturnNoDevice, kIOReturnOffline:
		return fmt.Errorf("%w: %s: %w", ErrDisconnected, what, err)
	case kIOReturnExclusiveAccess, kIOReturnBusy:
		return fmt.Errorf("%w: %s: %w", ErrBusy, what, err)
	case kIOReturnNotPrivileged, kIOReturnNotPermitted:
		return fmt.Errorf("%w: %s: %w", ErrPermission, what, err)
	case kIOReturnNotOpen:
		return fmt.Errorf("%w: %s: %w", ErrClosed, what, err)
	}
	return fmt.Errorf("usb: %s: %w", what, err)
}

// mayBeUnplug reports whether kr is a status an unplug can surface as before
// the registry entry is terminated.
func mayBeUnplug(err error) bool {
	var kr ioReturnError
	if !errors.As(err, &kr) {
		return false
	}
	switch uint32(kr) {
	case kIOReturnAborted, kIOReturnNotResponding, kIOUSBTransactionReturned, kIOReturnError, kIOReturnNoDevice, kIOReturnOffline:
		return true
	}
	return false
}
