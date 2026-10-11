package dap

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Version of the CMSIS-DAP transport a probe offers.
const (
	// V1 is CMSIS-DAP over USB HID reports (hidraw on Linux).
	V1 = 1
	// V2 is CMSIS-DAP over vendor-class bulk endpoints.
	V2 = 2
)

// UsbDevice is a CMSIS-DAP interface found on the USB bus.
type UsbDevice struct {
	Bus, Address int
	VID, PID     string // lowercase hex, 4 digits
	Manufacturer string
	Product      string
	Serial       string
	SysName      string // e.g. "1-2"
	Interface    int
	Version      int // V1 (HID) or V2 (bulk)
	EPIn, EPOut  uint8
	MaxPacket    int
	HIDRaw       string // /dev/hidrawN for V1
}

// ID is a stable, path-free identifier: vid:pid plus the serial, or the bus
// position when the probe has no serial.
func (d UsbDevice) ID() string {
	if d.Serial != "" {
		return fmt.Sprintf("%s:%s:%s", d.VID, d.PID, d.Serial)
	}
	return fmt.Sprintf("%s:%s@%s", d.VID, d.PID, d.SysName)
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readHex(path string) int {
	v, err := strconv.ParseUint(readTrim(path), 16, 32)
	if err != nil {
		return 0
	}
	return int(v)
}

func readDec(path string) int {
	v, err := strconv.Atoi(readTrim(path))
	if err != nil {
		return 0
	}
	return v
}

// Scan lists CMSIS-DAP interfaces below sysfs root (normally "/sys"). A
// CMSIS-DAP interface is recognised by "CMSIS-DAP" in its interface string,
// which the specification requires of both v1 and v2 probes. Nothing is
// opened and nothing is written. Where the probe offers both a v2 bulk and a
// v1 HID interface, the v2 one is listed.
func Scan(root string) []UsbDevice {
	devRoot := filepath.Join(root, "bus", "usb", "devices")
	entries, err := os.ReadDir(devRoot)
	if err != nil {
		return nil
	}
	var out []UsbDevice
	for _, e := range entries {
		name := e.Name()
		if strings.Contains(name, ":") || strings.HasPrefix(name, "usb") {
			continue
		}
		dir := filepath.Join(devRoot, name)
		dev := UsbDevice{
			Bus: readDec(filepath.Join(dir, "busnum")), Address: readDec(filepath.Join(dir, "devnum")),
			VID: strings.ToLower(readTrim(filepath.Join(dir, "idVendor"))), PID: strings.ToLower(readTrim(filepath.Join(dir, "idProduct"))),
			Manufacturer: readTrim(filepath.Join(dir, "manufacturer")), Product: readTrim(filepath.Join(dir, "product")),
			Serial: readTrim(filepath.Join(dir, "serial")), SysName: name,
		}
		if dev.VID == "" || dev.Bus == 0 || dev.Address == 0 {
			continue
		}
		subs, _ := os.ReadDir(dir)
		var best *UsbDevice
		for _, s := range subs {
			if !strings.HasPrefix(s.Name(), name+":") {
				continue
			}
			idir := filepath.Join(dir, s.Name())
			if !strings.Contains(strings.ToLower(readTrim(filepath.Join(idir, "interface"))), "cmsis-dap") {
				continue
			}
			cand := dev
			cand.Interface = readHex(filepath.Join(idir, "bInterfaceNumber"))
			switch readHex(filepath.Join(idir, "bInterfaceClass")) {
			case 0xFF:
				cand.Version = V2
				fillBulk(idir, &cand)
				if cand.EPIn == 0 || cand.EPOut == 0 {
					continue
				}
			case 0x03:
				cand.Version = V1
				if m, _ := filepath.Glob(filepath.Join(idir, "*", "hidraw", "hidraw*")); len(m) > 0 {
					cand.HIDRaw = "/dev/" + filepath.Base(m[0])
				}
				cand.MaxPacket = 64
				fillInterrupt(idir, &cand)
			default:
				continue
			}
			if best == nil || cand.Version > best.Version {
				c := cand
				best = &c
			}
		}
		if best != nil {
			out = append(out, *best)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

func endpoints(idir string) []string {
	m, _ := filepath.Glob(filepath.Join(idir, "ep_*"))
	return m
}

func fillBulk(idir string, d *UsbDevice) {
	for _, ep := range endpoints(idir) {
		if !strings.EqualFold(readTrim(filepath.Join(ep, "type")), "Bulk") {
			continue
		}
		addr := uint8(readHex(filepath.Join(ep, "bEndpointAddress")))
		mp := readHex(filepath.Join(ep, "wMaxPacketSize"))
		if addr&0x80 != 0 {
			if d.EPIn == 0 {
				d.EPIn = addr
			}
		} else if d.EPOut == 0 {
			d.EPOut = addr
		}
		if mp > 0 && (d.MaxPacket == 0 || mp < d.MaxPacket) {
			d.MaxPacket = mp
		}
	}
}

func fillInterrupt(idir string, d *UsbDevice) {
	for _, ep := range endpoints(idir) {
		if mp := readHex(filepath.Join(ep, "wMaxPacketSize")); mp > 0 {
			d.MaxPacket = mp
			return
		}
	}
}
