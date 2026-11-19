//go:build darwin

package usb

import "strings"

func platformSerialPorts(root string, match Match) ([]SerialPort, bool) {
	if !UsesIOKit(root) {
		return nil, false
	}
	if loadIOKit() != nil {
		return nil, true
	}
	devs := services(ioServiceMatching("IOUSBHostDevice"))
	defer releaseAll(devs)
	var out []SerialPort
	for _, svc := range devs {
		vid, ok1 := numProp(svc, "idVendor")
		pid, ok2 := numProp(svc, "idProduct")
		loc, _ := numProp(svc, "locationID")
		if !ok1 || !ok2 || uint32(loc)&0x00FFFFFF == 0 || (match != nil && !match(uint16(vid), uint16(pid))) {
			continue
		}
		manu, _ := strProp(svc, "kUSBVendorString", "USB Vendor Name")
		prod, _ := strProp(svc, "kUSBProductString", "USB Product Name")
		serial, _ := strProp(svc, "kUSBSerialNumberString", "USB Serial Number")
		var ttys []SerialPort
		calloutDevices(svc, loc, -1, 0, &ttys)
		for _, t := range ttys {
			t.VID, t.PID = uint16(vid), uint16(pid)
			t.Serial, t.Manufacturer, t.Product = strings.TrimSpace(serial), strings.TrimSpace(manu), strings.TrimSpace(prod)
			t.Location = locationName(uint32(loc))
			out = append(out, t)
		}
	}
	sortSerial(out)
	return out, true
}

// calloutDevices collects the IOCalloutDevice nodes below e (the serial
// driver's IOSerialBSDClient), with the USB interface number above each. It
// does not descend into other USB devices (a hub's downstream ports).
func calloutDevices(e uint32, loc int64, iface, depth int, out *[]SerialPort) {
	if depth > 8 {
		return
	}
	kids := children(e)
	defer releaseAll(kids)
	for _, c := range kids {
		if l, ok := numProp(c, "locationID"); ok && l != loc {
			continue
		}
		n := iface
		if v, ok := numProp(c, "bInterfaceNumber"); ok {
			n = int(v)
		}
		if s, ok := strProp(c, "IOCalloutDevice"); ok && s != "" {
			*out = append(*out, SerialPort{TTY: s, Interface: n})
		}
		calloutDevices(c, loc, n, depth+1, out)
	}
}
