package ccdebugger

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// TI's USB vendor id and the product ids cc-tool supports.
const (
	TIVID = "0451"
	// DefaultSysfs is the Linux USB device tree.
	DefaultSysfs = "/sys/bus/usb/devices"
)

// knownProducts are the programmers cc-tool lists as supported (VID 0451).
var knownProducts = map[string]string{
	"16a2": "CC Debugger",
	"16a0": "SmartRF05 Evaluation Board",
}

// Debugger is a CC-Debugger found on the USB bus.
type Debugger struct {
	Serial  string
	Product string
	PID     string
	Address string // cc-tool "bus:device" address
	Path    string
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// scanSysfs lists supported TI programmers without any host tool.
func scanSysfs(root string) []Debugger {
	entries, _ := filepath.Glob(filepath.Join(root, "*"))
	var out []Debugger
	for _, e := range entries {
		if !strings.EqualFold(readTrim(filepath.Join(e, "idVendor")), TIVID) {
			continue
		}
		pid := strings.ToLower(readTrim(filepath.Join(e, "idProduct")))
		if _, ok := knownProducts[pid]; !ok {
			continue
		}
		d := Debugger{
			Serial:  readTrim(filepath.Join(e, "serial")),
			Product: readTrim(filepath.Join(e, "product")),
			PID:     pid,
			Path:    e,
		}
		if b, n := readTrim(filepath.Join(e, "busnum")), readTrim(filepath.Join(e, "devnum")); b != "" && n != "" {
			d.Address = b + ":" + n
		}
		if d.Product == "" {
			d.Product = knownProducts[pid]
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out
}

// deviceID is the stable plugin id. Clone debuggers can share one serial, so
// duplicates get the bus address appended.
func deviceID(d Debugger, all []Debugger) string {
	id := d.Serial
	if id == "" {
		return "usb-" + strings.ReplaceAll(d.Address, ":", "-")
	}
	for _, o := range all {
		if o.Path != d.Path && o.Serial == d.Serial {
			return id + "@" + d.Address
		}
	}
	return id
}

// Info is the parsed result of `cc-tool -t`.
type Info struct {
	Programmer string
	Target     string
	Device     map[string]string
	TargetInfo map[string]string
}

var kv = regexp.MustCompile(`^\s*([A-Za-z][A-Za-z0-9 ]*?):\s*(.*?)\s*$`)

// parseInfo reads the two sections cc-tool prints: "Device info" (the
// debugger) and "Target info" (the chip behind it).
func parseInfo(out string) Info {
	in := Info{Device: map[string]string{}, TargetInfo: map[string]string{}}
	section := ""
	for _, line := range strings.Split(out, "\n") {
		trim := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trim, "Device info"):
			section = "device"
			continue
		case strings.HasPrefix(trim, "Target info"):
			section = "target"
			continue
		}
		m := kv.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		k, v := strings.ToLower(m[1]), m[2]
		switch {
		case section == "" && k == "programmer":
			in.Programmer = v
		case section == "" && k == "target":
			in.Target = v
		case section == "device":
			in.Device[k] = v
		case section == "target":
			in.TargetInfo[k] = v
		}
	}
	return in
}

var macRe = regexp.MustCompile(`primary:\s*([0-9A-Fa-f:]{23}),\s*secondary:\s*([0-9A-Fa-f:]{23})`)

// parseMAC returns the primary and secondary IEEE addresses. An erased
// secondary address (all FF) is reported as empty.
func parseMAC(out string) (primary, secondary string, ok bool) {
	m := macRe.FindStringSubmatch(out)
	if m == nil {
		return "", "", false
	}
	primary, secondary = strings.ToUpper(m[1]), strings.ToUpper(m[2])
	if strings.Trim(strings.ReplaceAll(secondary, ":", ""), "F") == "" {
		secondary = ""
	}
	return primary, secondary, true
}

// failure extracts a human-readable error line from cc-tool output.
func failure(out string) string {
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimSpace(line)
		low := strings.ToLower(l)
		if strings.Contains(low, "not found") || strings.HasPrefix(low, "error") ||
			strings.Contains(low, "failed") || strings.Contains(low, "unable") {
			return l
		}
	}
	return ""
}
