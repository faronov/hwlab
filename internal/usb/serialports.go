package usb

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SerialPort is one serial device node of a USB device (a USB-UART bridge
// or a CDC-ACM interface) with the identity of that device.
type SerialPort struct {
	// TTY is the device node to open: /dev/cu.* on macOS, /dev/tty* on Linux.
	TTY string
	// Interface is the USB interface number of the port, -1 if unknown.
	Interface    int
	VID, PID     uint16
	Serial       string
	Manufacturer string
	Product      string
	// Location is the physical port path ("20-2.1"): stable while the
	// device stays in the same port, unlike the node name or bus address.
	Location string
}

// SerialPorts lists the serial ports of the USB devices match accepts. Like
// Scan it only reads the IORegistry (macOS) or sysfs (Linux): no device or
// port is opened.
func SerialPorts(root string, match Match) []SerialPort {
	if ps, ok := platformSerialPorts(root, match); ok {
		return ps
	}
	return sysfsSerialPorts(root, match)
}

func sysfsSerialPorts(root string, match Match) []SerialPort {
	var out []SerialPort
	for _, d := range scanSysfs(root, match) {
		dir := filepath.Join(root, "bus", "usb", "devices", d.SysName)
		subs, _ := os.ReadDir(dir)
		for _, s := range subs {
			if !strings.HasPrefix(s.Name(), d.SysName+":") {
				continue
			}
			idir := filepath.Join(dir, s.Name())
			n := readNum(filepath.Join(idir, "bInterfaceNumber"), 16)
			var ttys []string
			for _, pat := range []string{"tty*", "tty/tty*"} {
				m, _ := filepath.Glob(filepath.Join(idir, pat))
				for _, p := range m {
					if b := filepath.Base(p); b != "tty" {
						ttys = append(ttys, b)
					}
				}
			}
			for _, t := range ttys {
				out = append(out, SerialPort{TTY: "/dev/" + t, Interface: n, VID: d.VID, PID: d.PID, Serial: d.Serial,
					Manufacturer: d.Manufacturer, Product: d.Product, Location: d.SysName})
			}
		}
	}
	sortSerial(out)
	return out
}

func sortSerial(ps []SerialPort) {
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].Location != ps[j].Location {
			return ps[i].Location < ps[j].Location
		}
		return ps[i].Interface < ps[j].Interface
	})
}
