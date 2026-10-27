package usb

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// readRaw returns the file with only the single trailing newline sysfs adds
// removed: unlike readTrim it keeps whitespace bytes that are part of a
// binary serial number.
func readRaw(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(string(b), "\n")
}

func readNum(path string, base int) int {
	v, err := strconv.ParseUint(readTrim(path), base, 32)
	if err != nil {
		return 0
	}
	return int(v)
}

// Match decides whether a device is wanted.
type Match func(vid, pid uint16) bool

// DefaultRoot is the sysfs root plugins scan. On macOS, which has no sysfs,
// Scan serves this root from the IORegistry instead.
const DefaultRoot = "/sys"

// Scan lists USB devices for which match returns true: below sysfs root on
// Linux (root is normally DefaultRoot; tests pass a fixture tree), and from
// the IORegistry on macOS when root is DefaultRoot. Nothing is opened and
// nothing is written.
func Scan(root string, match Match) []Device {
	if devs, ok := platformScan(root, match); ok {
		return devs
	}
	return scanSysfs(root, match)
}

func scanSysfs(root string, match Match) []Device {
	devRoot := filepath.Join(root, "bus", "usb", "devices")
	entries, err := os.ReadDir(devRoot)
	if err != nil {
		return nil
	}
	var out []Device
	for _, e := range entries {
		name := e.Name()
		if strings.Contains(name, ":") || strings.HasPrefix(name, "usb") {
			continue
		}
		dir := filepath.Join(devRoot, name)
		d := Device{
			Bus: readNum(filepath.Join(dir, "busnum"), 10), Address: readNum(filepath.Join(dir, "devnum"), 10),
			VID: uint16(readNum(filepath.Join(dir, "idVendor"), 16)), PID: uint16(readNum(filepath.Join(dir, "idProduct"), 16)),
			BCD:          uint16(readNum(filepath.Join(dir, "bcdDevice"), 16)),
			Manufacturer: readTrim(filepath.Join(dir, "manufacturer")), Product: readTrim(filepath.Join(dir, "product")),
			Serial: readTrim(filepath.Join(dir, "serial")), SerialRaw: readRaw(filepath.Join(dir, "serial")), SysName: name,
			Configuration: readNum(filepath.Join(dir, "bConfigurationValue"), 10),
			SpeedMbps:     readNum(filepath.Join(dir, "speed"), 10),
		}
		if d.Bus == 0 || d.Address == 0 || (d.VID == 0 && d.PID == 0) {
			continue
		}
		if match != nil && !match(d.VID, d.PID) {
			continue
		}
		d.Interfaces = scanInterfaces(dir, name)
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Bus != out[j].Bus {
			return out[i].Bus < out[j].Bus
		}
		return out[i].Address < out[j].Address
	})
	return out
}

func scanInterfaces(dir, name string) []Interface {
	subs, _ := os.ReadDir(dir)
	var out []Interface
	for _, s := range subs {
		if !strings.HasPrefix(s.Name(), name+":") {
			continue
		}
		idir := filepath.Join(dir, s.Name())
		in := Interface{
			Number: readNum(filepath.Join(idir, "bInterfaceNumber"), 16),
			Class:  readNum(filepath.Join(idir, "bInterfaceClass"), 16),
			Name:   readTrim(filepath.Join(idir, "interface")),
		}
		if link, err := os.Readlink(filepath.Join(idir, "driver")); err == nil {
			in.Driver = filepath.Base(link)
		}
		if m, _ := filepath.Glob(filepath.Join(idir, "tty*")); len(m) > 0 {
			sort.Strings(m)
			in.TTY = filepath.Base(m[0])
			// CDC-ACM keeps the node one level down, in a "tty" directory.
			if in.TTY == "tty" {
				if sub, _ := filepath.Glob(filepath.Join(m[0], "tty*")); len(sub) > 0 {
					sort.Strings(sub)
					in.TTY = filepath.Base(sub[0])
				}
			}
		}
		eps, _ := filepath.Glob(filepath.Join(idir, "ep_*"))
		sort.Strings(eps)
		for _, ep := range eps {
			in.Endpoints = append(in.Endpoints, Endpoint{
				Address:   uint8(readNum(filepath.Join(ep, "bEndpointAddress"), 16)),
				Type:      readTrim(filepath.Join(ep, "type")),
				MaxPacket: readNum(filepath.Join(ep, "wMaxPacketSize"), 16),
			})
		}
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}
