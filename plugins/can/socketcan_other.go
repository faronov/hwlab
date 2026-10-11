//go:build !linux

package canplugin

import "errors"

// SocketCANSupported is false: SocketCAN exists only on Linux. USB-CAN adapters
// plug into the same Bus interface.
const SocketCANSupported = false

// OpenSocketCAN always fails off Linux.
func OpenSocketCAN(string, Options) (Bus, error) {
	return nil, errors.New("SocketCAN is only available on Linux")
}

// ListSocketCAN finds nothing off Linux.
func ListSocketCAN() ([]IfaceInfo, error) { return nil, nil }
