//go:build !linux && !darwin

package usb

// Open is not available on this platform.
func Open(Device, int, OpenOptions) (Port, error) { return nil, ErrUnsupported }
