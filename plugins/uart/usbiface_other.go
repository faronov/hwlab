//go:build !linux

package uart

func usbInterface(string) (device, iface string) { return "", "" }
