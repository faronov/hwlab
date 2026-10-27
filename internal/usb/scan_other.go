//go:build !darwin

package usb

func platformScan(string, Match) ([]Device, bool) { return nil, false }

// UsesIOKit reports whether Scan(root, ...) reads the IORegistry instead of
// a sysfs tree. It is always false off macOS.
func UsesIOKit(string) bool { return false }
