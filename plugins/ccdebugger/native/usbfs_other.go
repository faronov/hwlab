//go:build !linux

package native

// Supported reports whether this build has a native USB transport.
const Supported = false

// Open is not implemented on this platform: the native backend currently
// speaks to the adapter through Linux usbfs only. A libusb/IOKit transport can
// be added behind [Transport] without changing the debugger.
func Open(Address) (Transport, error) {
	return nil, transportf("the native CC-Debugger transport is only implemented for Linux (usbfs); use the cc-tool backend on this platform")
}
