package jlink

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SEGGERVID is the USB vendor id of SEGGER J-Link probes (also used by the
// J-Link OB firmware on many evaluation boards).
const SEGGERVID = "1366"

// DefaultSysfs is the Linux USB device tree.
const DefaultSysfs = "/sys/bus/usb/devices"

// Probe is a J-Link probe found on the USB bus.
type Probe struct {
	Serial  string // decimal serial number without leading zeros
	Product string // USB product string, or the commander's ProductName
	// Model and Nickname come from the commander's ShowEmuList. The USB product
	// string is not the model: a probe's user-set nickname can end up in it
	// (a SAM-ICE with nickname "jlinkv9" enumerates as "J-Link_jlinkv9").
	Model    string
	Nickname string
	PID      string
	Path     string // sysfs entry, empty when the probe came from the commander
}

// NormalizeSerial strips the leading zeros USB descriptors pad the serial
// number with, which is how J-Link tools print and accept it.
func NormalizeSerial(s string) string {
	s = strings.TrimSpace(s)
	t := strings.TrimLeft(s, "0")
	if t == "" && s != "" {
		return "0"
	}
	return t
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// scanSysfs lists SEGGER USB devices without any SEGGER software. A missing
// sysfs (non-Linux hosts) is not an error: probes are then found through the
// commander's ShowEmuList instead.
func scanSysfs(root string) []Probe {
	entries, err := filepath.Glob(filepath.Join(root, "*"))
	if err != nil {
		return nil
	}
	var out []Probe
	for _, e := range entries {
		if !strings.EqualFold(readTrim(filepath.Join(e, "idVendor")), SEGGERVID) {
			continue
		}
		out = append(out, Probe{
			Serial:  NormalizeSerial(readTrim(filepath.Join(e, "serial"))),
			Product: readTrim(filepath.Join(e, "product")),
			PID:     strings.ToLower(readTrim(filepath.Join(e, "idProduct"))),
			Path:    e,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Serial < out[j].Serial })
	return out
}

// mergeProbes adds probes from b that are not in a (matched by serial) and
// fills Model and Nickname of the ones that are.
func mergeProbes(a, b []Probe) []Probe {
	seen := map[string]bool{}
	for i, p := range a {
		seen[p.Serial] = true
		for _, q := range b {
			if q.Serial == p.Serial {
				a[i].Model, a[i].Nickname = q.Product, q.Nickname
			}
		}
	}
	for _, p := range b {
		if !seen[p.Serial] {
			a = append(a, p)
			seen[p.Serial] = true
		}
	}
	return a
}
