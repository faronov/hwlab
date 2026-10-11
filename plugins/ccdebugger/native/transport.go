package native

import "time"

// Transport is the USB layer under a [Debugger]: the debug interface 0 of one
// CC-Debugger, already opened and claimed. Implementations must not change the
// device configuration or detach kernel drivers.
type Transport interface {
	// Revision is the USB bcdDevice value.
	Revision() (uint16, error)
	ControlRead(requestType, request uint8, value, index uint16, length int, timeout time.Duration) ([]byte, error)
	ControlWrite(requestType, request uint8, value, index uint16, data []byte, timeout time.Duration) (int, error)
	BulkWrite(endpoint uint8, data []byte, timeout time.Duration) (int, error)
	BulkRead(endpoint uint8, length int, timeout time.Duration) ([]byte, error)
	Close() error
}

// Address selects one USB device.
type Address struct {
	Bus     int
	Address int
}

// Valid reports whether both parts are in the USB ranges.
func (a Address) Valid() bool {
	return a.Bus >= 1 && a.Bus <= 255 && a.Address >= 1 && a.Address <= 127
}
