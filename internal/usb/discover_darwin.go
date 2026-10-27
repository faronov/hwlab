//go:build darwin

package usb

import (
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unsafe"
)

// platformScan serves the default root "/sys" from the IORegistry on macOS,
// which has no sysfs. Any other root is a sysfs tree (tests, fixtures).
func platformScan(root string, match Match) ([]Device, bool) {
	if !UsesIOKit(root) {
		return nil, false
	}
	return scanIOKit(match), true
}

// UsesIOKit reports whether Scan(root, ...) reads the IORegistry instead of
// a sysfs tree: true on macOS for the default root "/sys".
func UsesIOKit(root string) bool { return filepath.Clean(root) == DefaultRoot }

// scanIOKit lists IOUSBHostDevice services. It reads registry properties and,
// for wanted devices only, the cached configuration descriptor (for the
// endpoints) through a device user client that is never opened: nothing is
// claimed, configured or written.
func scanIOKit(match Match) []Device {
	if loadIOKit() != nil {
		return nil
	}
	devs := services(ioServiceMatching("IOUSBHostDevice"))
	defer releaseAll(devs)
	var out []Device
	for _, svc := range devs {
		d, ok := deviceFromService(svc, match)
		if ok {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Bus != out[j].Bus {
			return out[i].Bus < out[j].Bus
		}
		return out[i].Address < out[j].Address
	})
	return out
}

func deviceFromService(svc uint32, match Match) (Device, bool) {
	vid, ok1 := numProp(svc, "idVendor")
	pid, ok2 := numProp(svc, "idProduct")
	loc, _ := numProp(svc, "locationID")
	if !ok1 || !ok2 || uint32(loc)&0x00FFFFFF == 0 { // root hubs have no port nibbles
		return Device{}, false
	}
	if match != nil && !match(uint16(vid), uint16(pid)) {
		return Device{}, false
	}
	addr, _ := numProp(svc, "USB Address", "kUSBAddress")
	bcd, _ := numProp(svc, "bcdDevice")
	cfg, _ := numProp(svc, "kUSBCurrentConfiguration")
	speed, _ := numProp(svc, "Device Speed", "USBSpeed")
	manu, _ := strProp(svc, "kUSBVendorString", "USB Vendor Name")
	prod, _ := strProp(svc, "kUSBProductString", "USB Product Name")
	serial, _ := strProp(svc, "kUSBSerialNumberString", "USB Serial Number")
	d := Device{
		Bus: int(uint32(loc) >> 24), Address: int(addr),
		VID: uint16(vid), PID: uint16(pid), BCD: uint16(bcd),
		Manufacturer: strings.TrimSpace(manu), Product: strings.TrimSpace(prod),
		Serial: strings.TrimSpace(serial), SerialRaw: serial,
		SysName:       locationName(uint32(loc)),
		Configuration: int(cfg),
		SpeedMbps:     speedMbps(speed),
		LocationID:    uint32(loc),
		RegistryID:    registryID(svc),
	}
	d.Interfaces = interfacesOf(svc, int(cfg))
	return d, true
}

// locationName renders a locationID like a Linux sysfs port path: bus number
// (top byte), then one port number per non-zero nibble, e.g. 0x14210000 is
// "20-2.1". It is a physical-port name for diagnostics, never an identity.
func locationName(loc uint32) string {
	var sb strings.Builder
	sb.WriteString(strconv.Itoa(int(loc >> 24)))
	sep := "-"
	for shift := 20; shift >= 0; shift -= 4 {
		p := (loc >> uint(shift)) & 0xF
		if p == 0 {
			break
		}
		sb.WriteString(sep)
		sb.WriteString(strconv.Itoa(int(p)))
		sep = "."
	}
	return sb.String()
}

// speedMbps maps the IOKit "Device Speed" enumeration to the sysfs value.
func speedMbps(s int64) int {
	switch s {
	case 0:
		return 1 // low speed, 1.5 Mbit/s
	case 1:
		return 12
	case 2:
		return 480
	case 3:
		return 5000
	case 4:
		return 10000
	}
	return 0
}

func interfacesOf(dev uint32, cfg int) []Interface {
	kids := children(dev)
	defer releaseAll(kids)
	var out []Interface
	type alt struct{ num, alt int }
	var alts []alt
	for _, k := range kids {
		if ioObjectConformsTo(k, "IOUSBHostInterface") == 0 {
			continue
		}
		num, ok := numProp(k, "bInterfaceNumber")
		if !ok {
			continue
		}
		class, _ := numProp(k, "bInterfaceClass")
		a, _ := numProp(k, "bAlternateSetting")
		name, _ := strProp(k, "kUSBString", "USB Interface Name")
		in := Interface{Number: int(num), Class: int(class), Name: strings.TrimSpace(name), Driver: interfaceDriver(k)}
		if tty := searchString(k, "IOCalloutDevice"); tty != "" {
			in.TTY = strings.TrimPrefix(tty, "/dev/")
		}
		out = append(out, in)
		alts = append(alts, alt{int(num), int(a)})
	}
	if len(out) > 0 {
		if desc := configDescriptor(dev, cfg); len(desc) > 0 {
			parsed, _ := parseConfigDescriptor(desc)
			for i := range out {
				for _, p := range parsed {
					if p.Number == alts[i].num && p.Alt == alts[i].alt {
						out[i].Endpoints = p.Endpoints
						break
					}
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

// interfaceDriver names what holds the interface: the kernel driver that
// opened it exclusively, else the first matched driver below it.
func interfaceDriver(iface uint32) string {
	if s, ok := strProp(iface, "UsbExclusiveOwner"); ok && s != "" {
		return s
	}
	kids := children(iface)
	defer releaseAll(kids)
	for _, k := range kids {
		if s, ok := strProp(k, "IOUserClass"); ok && s != "" {
			return s
		}
		if c := className(k); c != "" && !strings.Contains(c, "UserClient") {
			return c
		}
	}
	return ""
}

// configDescriptor copies the configuration descriptor with
// bConfigurationValue cfg that the host stack cached at enumeration. The
// device user client is created for this read only and is never opened
// (USBDeviceOpen is not called), so no other client is disturbed.
func configDescriptor(dev uint32, cfg int) []byte {
	obj, _, err := plugin(dev, cfuuidDeviceUserClient, iidDevice100)
	if err != nil {
		return nil
	}
	defer obj.release()
	for idx := 0; idx < 8; idx++ {
		var ptr unsafe.Pointer
		if kr := obj.call(slotDevGetConfigurationDescriptorPtr, uintptr(idx), uintptr(unsafe.Pointer(&ptr))); kr != 0 || ptr == nil {
			return nil
		}
		hdr := unsafe.Slice((*byte)(ptr), 9)
		total := int(hdr[2]) | int(hdr[3])<<8
		if total < 9 {
			return nil
		}
		if cfg == 0 || int(hdr[5]) == cfg {
			return append([]byte(nil), unsafe.Slice((*byte)(ptr), total)...)
		}
	}
	return nil
}
