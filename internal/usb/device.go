// Package usb is the small USB layer shared by native plugins: discovery of
// devices, interfaces and endpoints (sysfs on Linux, the IORegistry on macOS)
// and a transport that claims exactly one interface (Linux usbfs; IOKit
// IOUSBInterfaceInterface on macOS, called through purego). It has no libusb
// and no cgo dependency.
//
// A USB device with several interfaces (an FT2232 has two) is opened one
// interface at a time. Opening interface A never touches interface B, so a
// kernel serial driver bound to B keeps serving it.
package usb

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Errors every transport returns (wrapped, so test with errors.Is).
var (
	// ErrTimeout is a transfer that did not finish in time. A bulk IN
	// timeout loses no data.
	ErrTimeout = errors.New("usb: transfer timed out")
	// ErrDisconnected means the device was unplugged or reset away.
	ErrDisconnected = errors.New("usb: device disconnected")
	// ErrStall is an endpoint that answered with a STALL handshake. The
	// endpoint stays halted until HaltClearer.ClearHalt is called.
	ErrStall = errors.New("usb: endpoint stalled")
	// ErrBusy means the interface is claimed by a driver or process and the
	// caller did not allow detaching it.
	ErrBusy = errors.New("usb: interface is busy")
	// ErrPermission means the user may not open the device node.
	ErrPermission = errors.New("usb: permission denied")
	// ErrUnsupported is returned on platforms without a native transport.
	ErrUnsupported = errors.New("usb: native transport is available on Linux and macOS only")
	// ErrClosed is an operation on a closed interface.
	ErrClosed = errors.New("usb: interface is closed")
)

// Endpoint is one endpoint of an interface.
type Endpoint struct {
	Address   uint8  `json:"address"`
	Type      string `json:"type"` // Bulk, Interrupt, Isoc, Control
	MaxPacket int    `json:"max_packet"`
}

// In reports whether the endpoint is device-to-host.
func (e Endpoint) In() bool { return e.Address&0x80 != 0 }

// Interface is one USB interface of a device.
type Interface struct {
	Number    int        `json:"number"`
	Class     int        `json:"class"`
	Name      string     `json:"name,omitempty"`      // interface string, if any
	Driver    string     `json:"driver,omitempty"`    // kernel driver bound now
	TTY       string     `json:"tty,omitempty"`       // e.g. ttyUSB1 when a serial driver is bound
	Endpoints []Endpoint `json:"endpoints,omitempty"` // in descriptor order
}

// BulkEndpoints returns the first bulk IN and OUT endpoints (0 if absent).
func (i Interface) BulkEndpoints() (in, out uint8, maxPacket int) {
	for _, e := range i.Endpoints {
		if e.Type != "Bulk" {
			continue
		}
		if e.In() && in == 0 {
			in = e.Address
		}
		if !e.In() && out == 0 {
			out = e.Address
		}
		if e.MaxPacket > 0 && (maxPacket == 0 || e.MaxPacket < maxPacket) {
			maxPacket = e.MaxPacket
		}
	}
	return
}

// Device is a USB device as seen in sysfs (Linux) or the IORegistry (macOS).
type Device struct {
	Bus, Address int
	VID, PID     uint16
	BCD          uint16 // bcdDevice
	Manufacturer string
	Product      string
	Serial       string
	// SerialRaw is Serial without whitespace trimming. Some probes (ST-Link
	// clones) report a binary serial whose bytes may be spaces.
	SerialRaw     string
	SysName       string // e.g. "2-2"; on macOS derived from the locationID
	Configuration int    // active bConfigurationValue
	SpeedMbps     int
	Interfaces    []Interface
	// LocationID and RegistryID are set on macOS only: the IOKit port
	// location and the IORegistry entry the transport reopens. Both change
	// with the port or at every re-enumeration and are never an identity
	// (the identity is VID:PID plus Serial, as on Linux).
	LocationID uint32
	RegistryID uint64
}

// Iface returns interface n.
func (d Device) Iface(n int) (Interface, bool) {
	for _, i := range d.Interfaces {
		if i.Number == n {
			return i, true
		}
	}
	return Interface{}, false
}

// Path is the Linux usbfs device node.
func (d Device) Path() string { return fmt.Sprintf("/dev/bus/usb/%03d/%03d", d.Bus, d.Address) }

// Port is a transport bound to one claimed interface.
type Port interface {
	// ControlIn performs a device-to-host control transfer and returns the
	// bytes the device sent.
	ControlIn(ctx context.Context, requestType, request uint8, value, index uint16, length int) ([]byte, error)
	// ControlOut performs a host-to-device control transfer.
	ControlOut(ctx context.Context, requestType, request uint8, value, index uint16, data []byte) error
	// BulkOut sends data to an OUT endpoint in one transfer.
	BulkOut(ctx context.Context, endpoint uint8, data []byte, timeout time.Duration) (int, error)
	// BulkIn reads up to length bytes from an IN endpoint. A short or
	// status-only packet is a normal result; ErrTimeout means nothing arrived.
	BulkIn(ctx context.Context, endpoint uint8, length int, timeout time.Duration) ([]byte, error)
	// Describe is a human-readable transport description.
	Describe() string
	// DetachedDriver names the kernel driver that was detached from the
	// interface at open time ("" if none was).
	DetachedDriver() string
	// Close releases the interface and, if a driver was detached, asks the
	// kernel to bind it again. It is idempotent.
	Close() error
}

// HaltClearer is implemented by ports that can clear an endpoint halt
// (USB CLEAR_FEATURE ENDPOINT_HALT) after ErrStall.
type HaltClearer interface {
	ClearHalt(endpoint uint8) error
}

// DeviceResetter is implemented by ports that can issue a USB port reset
// (USBDEVFS_RESET) to recover a wedged device. The interface stays claimed.
type DeviceResetter interface {
	ResetDevice() error
}

// OpenOptions control how an interface is claimed.
type OpenOptions struct {
	// DetachDriver allows detaching the named kernel driver from the claimed
	// interface only (never from the others, never unloading the module).
	// Empty means a busy interface is an error.
	DetachDriver string
}
