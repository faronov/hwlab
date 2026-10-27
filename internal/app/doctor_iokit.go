package app

import (
	"fmt"
	"strings"

	"github.com/faronov/hwlab/internal/status"
	"github.com/faronov/hwlab/internal/usb"
)

// iokitProbe describes how the macOS doctor finds one native backend's probes
// in the IORegistry. pick returns the interface the plugin would claim.
type iokitProbe struct {
	prefix string // check-name prefix, e.g. "J-Link"
	plugin string // status registry key
	head   string // detail of the backend check
	none   string // detail when nothing is connected
	pick   func(usb.Device) (m iokitMatch, ok bool)
}

// iokitMatch is a probe found on the bus: the interface the plugin would claim
// and an optional note. warn marks a probe the backend cannot use as it is.
type iokitMatch struct {
	iface int
	note  string
	warn  bool
}

// iokitChecks is the macOS form of a native-backend doctor check. Discovery
// comes from the IORegistry (no sysfs, device nodes or udev rules) and nothing
// is opened. An interface bound to a macOS driver is reported as a warning,
// because the backend never detaches drivers and macOS offers no detach.
func iokitChecks(name string, p iokitProbe, devs []usb.Device) []DoctorCheck {
	checks := []DoctorCheck{{name, CheckOK, p.head}}
	found := 0
	for _, d := range devs {
		m, ok := p.pick(d)
		if !ok {
			continue
		}
		found++
		serial := d.Serial
		if serial == "" {
			serial = "(none: the id falls back to the USB location)"
		}
		detail := fmt.Sprintf("%04x:%04x %s; select it by USB serial %s (hwlab devices)", d.VID, d.PID, strings.TrimSpace(d.Product), serial)
		if m.note != "" {
			detail += "; " + m.note
		}
		if m.warn {
			checks = append(checks, DoctorCheck{p.prefix + ": detected", CheckWarn, detail})
			continue
		}
		checks = append(checks, DoctorCheck{p.prefix + ": detected", CheckOK, detail})
		n := m.iface
		var others []string
		for _, i := range d.Interfaces {
			if i.Number == n {
				continue
			}
			drv := i.Driver
			if drv == "" {
				drv = "no driver"
			}
			if i.TTY != "" {
				drv += ", /dev/" + i.TTY
			}
			others = append(others, fmt.Sprintf("%d=%s", i.Number, drv))
		}
		if len(others) > 0 {
			checks = append(checks, DoctorCheck{p.prefix + ": other interfaces", CheckOK, strings.Join(others, "; ") + " (hwlab claims only its own interface and never detaches a driver)"})
		}
		i, _ := d.Iface(n)
		if i.Driver != "" {
			checks = append(checks, DoctorCheck{p.prefix + ": access", CheckWarn, fmt.Sprintf("interface %d is held by %s; macOS cannot detach it, so opening reports busy", n, i.Driver)})
			continue
		}
		checks = append(checks, DoctorCheck{p.prefix + ": access", CheckOK, fmt.Sprintf("interface %d has no macOS driver; it is opened exclusively through IOKit on use (no permission setup needed)", n)})
	}
	if found == 0 {
		checks = append(checks, DoctorCheck{p.prefix + ": detected", CheckOK, p.none})
	}
	if e, ok := status.Lookup(p.plugin); ok && len(e.Backends) > 0 {
		checks = append(checks, DoctorCheck{p.prefix + ": macOS evidence", CheckOK, "the capability evidence below was gathered on Linux unless the line names macOS"})
		for _, c := range e.Backends[0].Capabilities {
			level := CheckOK
			if c.Status != status.CapRealHW {
				level = CheckWarn
			}
			checks = append(checks, DoctorCheck{p.prefix + " capability: " + c.Name, level, string(c.Status) + ": " + c.Evidence})
		}
	}
	return checks
}

// iokitHead is the backend-check detail on macOS. Hardware evidence is per
// capability (status registry); this line only names the transport.
func iokitHead(extra string) string {
	return "available (Go IOKit via purego; platform macOS; CGO no; " + extra + ")"
}

// vendorBulkIface returns the last vendor-class interface with bulk IN and
// OUT, the one plugins/jlinknative/usb.Scan claims.
func vendorBulkIface(d usb.Device) (n int, ok bool) {
	for _, i := range d.Interfaces {
		if in, out, _ := i.BulkEndpoints(); i.Class == 0xff && in != 0 && out != 0 {
			n, ok = i.Number, true
		}
	}
	return n, ok
}

func pickVIDPID(vid, pid uint16, iface int) func(usb.Device) (iokitMatch, bool) {
	return func(d usb.Device) (iokitMatch, bool) { return iokitMatch{iface: iface}, d.VID == vid && d.PID == pid }
}

var (
	iokitFTDI = iokitProbe{
		prefix: "FTDI/MPSSE", plugin: "ftdi",
		head: iokitHead("no libusb, OpenOCD or Python needed; channel A must not be bound to AppleUSBFTDI"),
		none: "no FTDI 0403:6010 adapter connected", pick: pickVIDPID(0x0403, 0x6010, 0),
	}
	iokitESP = iokitProbe{
		prefix: "ESP USB Serial/JTAG", plugin: "espusbjtag",
		head: iokitHead("no libusb, OpenOCD, esptool, ESP-IDF or Python needed"),
		none: "no Espressif 303a:1001 device connected", pick: pickVIDPID(0x303a, 0x1001, 2),
	}
	iokitSTLink = iokitProbe{
		prefix: "ST-Link", plugin: "stlink",
		head: iokitHead("no libusb, OpenOCD, STM32CubeProgrammer, st-flash or Python needed"),
		none: "no ST-Link V2 (0483:3748) connected", pick: pickVIDPID(0x0483, 0x3748, 0),
	}
	iokitCMSISDAP = iokitProbe{
		prefix: "CMSIS-DAP", plugin: "cmsisdap",
		head: iokitHead("v2 bulk only, v1 HID is not implemented on macOS; no libusb, OpenOCD, probe-rs or Python needed"),
		none: "no CMSIS-DAP probe connected",
		pick: func(d usb.Device) (iokitMatch, bool) {
			v1 := -1
			for _, i := range d.Interfaces {
				if !strings.Contains(strings.ToLower(i.Name), "cmsis-dap") {
					continue
				}
				if in, out, _ := i.BulkEndpoints(); i.Class == 0xff && in != 0 && out != 0 {
					return iokitMatch{iface: i.Number, note: "v2 bulk"}, true
				}
				if i.Class == 0x03 && v1 < 0 {
					v1 = i.Number
				}
			}
			if v1 >= 0 {
				return iokitMatch{iface: v1, note: "v1 HID only, which is not implemented on macOS", warn: true}, true
			}
			return iokitMatch{}, false
		},
	}
	iokitJLink = iokitProbe{
		prefix: "J-Link", plugin: "jlinknative",
		head: iokitHead("no JLinkExe, libjlinkarm, libusb, OpenOCD or Python; never falls back to jlink-segger"),
		none: "no J-Link connected",
		pick: func(d usb.Device) (iokitMatch, bool) {
			if d.VID != 0x1366 {
				return iokitMatch{}, false
			}
			if d.PID == 0x0101 {
				return iokitMatch{note: "runs its bootloader and answers no commands: unplug and replug it, or run JLinkExe (it reinstalls the probe firmware)", warn: true}, true
			}
			n, ok := vendorBulkIface(d)
			if !ok {
				return iokitMatch{}, false
			}
			return iokitMatch{iface: n, note: "the native id is jlink-native:" + strings.TrimLeft(d.Serial, "0")}, true
		},
	}
)

// iokitDevices enumerates USB devices from the IORegistry (macOS only).
func iokitDevices() []usb.Device { return usb.Scan(usb.DefaultRoot, nil) }
