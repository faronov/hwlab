//go:build !linux

package dap

import "errors"

// OpenUSB is not available on this platform; probes are reached through
// Linux usbfs and hidraw only.
func OpenUSB(UsbDevice) (Transport, error) {
	return nil, errors.New("CMSIS-DAP USB access is implemented for Linux only (usbfs/hidraw)")
}
