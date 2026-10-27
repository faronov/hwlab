//go:build linux || darwin

package dap

import (
	"fmt"
	"strconv"

	"github.com/faronov/hwlab/internal/usb"
)

// openBulk claims only the CMSIS-DAP v2 interface (usbfs on Linux, IOKit on
// macOS); the other interfaces of a composite probe stay with their drivers.
func openBulk(d UsbDevice) (Transport, error) {
	vid, err1 := strconv.ParseUint(d.VID, 16, 16)
	pid, err2 := strconv.ParseUint(d.PID, 16, 16)
	if err1 != nil || err2 != nil {
		return nil, fmt.Errorf("bad USB id %s:%s", d.VID, d.PID)
	}
	ud := usb.Device{
		Bus: d.Bus, Address: d.Address, VID: uint16(vid), PID: uint16(pid), Serial: d.Serial, SysName: d.SysName,
		RegistryID: d.registryID, Interfaces: []usb.Interface{{Number: d.Interface, Class: 0xff}},
	}
	port, err := usb.Open(ud, d.Interface, usb.OpenOptions{})
	if err != nil {
		return nil, err
	}
	return newBulk(port, d), nil
}
