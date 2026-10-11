package uart

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// usbInterface returns the USB device (sysfs name such as "2-2") and the
// interface number behind a tty, so a multi-interface adapter (for example an
// FT2232 whose other channel is driven by another plugin) can be told apart
// from a single-function UART. Empty when the tty is not a USB interface.
func usbInterface(path string) (device, iface string) {
	link, err := filepath.EvalSymlinks(filepath.Join("/sys/class/tty", filepath.Base(path), "device"))
	if err != nil {
		return "", ""
	}
	// usb-serial ports link to the port node below the interface; CDC-ACM
	// ports link to the interface itself.
	b, err := os.ReadFile(filepath.Join(link, "bInterfaceNumber"))
	if err != nil {
		link = filepath.Dir(link)
		if b, err = os.ReadFile(filepath.Join(link, "bInterfaceNumber")); err != nil {
			return "", ""
		}
	}
	name := filepath.Base(link) // e.g. 2-2:1.0
	if i := strings.IndexByte(name, ':'); i > 0 {
		device = name[:i]
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 16, 8) // sysfs prints hex
	if err != nil {
		return "", ""
	}
	return device, strconv.Itoa(int(n))
}
