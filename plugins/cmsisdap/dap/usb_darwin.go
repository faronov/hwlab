//go:build darwin

package dap

import "fmt"

// OpenUSB opens a CMSIS-DAP v2 probe's bulk interface through IOKit. Only that
// interface is opened; the CDC-ACM and HID interfaces of a composite probe stay
// with their macOS drivers. CMSIS-DAP v1 (HID reports) is not implemented on
// macOS: it would need IOHIDManager and no v1 probe was available to test it.
func OpenUSB(d UsbDevice) (Transport, error) {
	switch d.Version {
	case V2:
		return openBulk(d)
	case V1:
		return nil, fmt.Errorf("CMSIS-DAP v1 (HID) %s:%s is not implemented on macOS; use a CMSIS-DAP v2 (bulk) probe or Linux", d.VID, d.PID)
	}
	return nil, fmt.Errorf("unknown CMSIS-DAP transport version %d", d.Version)
}
