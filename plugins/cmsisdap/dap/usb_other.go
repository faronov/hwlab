//go:build !linux && !darwin

package dap

import "errors"

// OpenUSB is not available on this platform; probes are reached through
// Linux usbfs/hidraw and macOS IOKit (v2 bulk) only.
func OpenUSB(UsbDevice) (Transport, error) {
	return nil, errors.New("CMSIS-DAP USB access is implemented for Linux (usbfs/hidraw) and macOS (IOKit, v2 bulk) only")
}
