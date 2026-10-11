package app

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/faronov/hwlab/internal/status"
)

// Check levels reported by Doctor.
const (
	CheckOK   = "ok"
	CheckWarn = "warn" // optional piece missing; some backends unusable
	CheckFail = "fail" // hwlab itself cannot work
)

// DoctorCheck is one environment check.
type DoctorCheck struct {
	Name   string `json:"name"`
	Level  string `json:"level"`
	Detail string `json:"detail"`
}

// DoctorPlugin is a plugin together with its validation status.
type DoctorPlugin struct {
	ID             string                `json:"id"`
	Installed      bool                  `json:"installed"`
	Classified     bool                  `json:"classified"`
	Error          string                `json:"error,omitempty"`
	Implementation status.Implementation `json:"implementation,omitempty"`
	Validation     status.Validation     `json:"validation,omitempty"`
	Dependency     status.Dependency     `json:"dependency,omitempty"`
	Missing        []string              `json:"missing_backends,omitempty"`
	Backends       []status.Backend      `json:"backends,omitempty"`
}

// DoctorReport is the result of Doctor.
type DoctorReport struct {
	Checks  []DoctorCheck  `json:"checks"`
	Plugins []DoctorPlugin `json:"plugins"`
}

// Failed reports whether any check is a hard failure.
func (r DoctorReport) Failed() bool {
	for _, c := range r.Checks {
		if c.Level == CheckFail {
			return true
		}
	}
	return false
}

// Doctor inspects the local environment: storage, plugin discovery against
// the support matrix, and the external tools and services some backends need.
// A missing optional tool is a warning, never a failure.
func (a *App) Doctor(ctx context.Context) DoctorReport {
	var r DoctorReport
	add := func(name, level, detail string) { r.Checks = append(r.Checks, DoctorCheck{name, level, detail}) }

	if err := writable(a.Cfg.SessionsDir); err != nil {
		add("sessions dir", CheckFail, a.Cfg.SessionsDir+": "+err.Error())
	} else {
		add("sessions dir", CheckOK, a.Cfg.SessionsDir+" is writable")
	}

	seen := map[string]bool{}
	for _, p := range a.ListPlugins(ctx) {
		seen[p.ID] = true
		dp := DoctorPlugin{ID: p.ID, Installed: true, Error: p.Error}
		if e, ok := status.Lookup(p.ID); ok {
			s := e.Summary()
			dp.Implementation, dp.Validation, dp.Dependency, dp.Missing, dp.Backends, dp.Classified = s.Implementation, s.Validation, s.Dependency, s.Missing, e.Backends, true
		} else if p.ID != "replay" {
			add("plugin "+p.ID, CheckWarn, "not in the support matrix: its validation status is unknown")
		}
		if p.Error != "" {
			add("plugin "+p.ID, CheckFail, "failed to start: "+p.Error)
		}
		r.Plugins = append(r.Plugins, dp)
	}
	for _, e := range status.Registry() {
		if seen[e.Plugin] {
			continue
		}
		s := e.Summary()
		r.Plugins = append(r.Plugins, DoctorPlugin{ID: e.Plugin, Classified: true, Implementation: s.Implementation,
			Validation: s.Validation, Dependency: s.Dependency, Missing: s.Missing, Backends: e.Backends})
	}
	sort.Slice(r.Plugins, func(i, j int) bool { return r.Plugins[i].ID < r.Plugins[j].ID })
	if len(seen) == 0 {
		add("plugins", CheckFail, "no plugin executables found in the search path")
	}

	tool := func(name, label, env string, extra ...string) {
		if p := findTool(name, env, extra...); p != "" {
			add(label, CheckOK, p)
		} else {
			add(label, CheckWarn, "not found (optional, see docs/hardware.md)")
		}
	}
	tool("tshark", "tshark (packet decode)", "")
	tool("JLinkExe", "SEGGER J-Link software", "HWLAB_JLINK_EXE", "/opt/SEGGER/JLink/JLinkExe")
	tool("cc-tool", "cc-tool (optional CC-Debugger fallback)", "HWLAB_CC_TOOL")
	r.Checks = append(r.Checks, ccDebuggerNativeChecks(seen["ccdebugger"])...)
	r.Checks = append(r.Checks, ftdiNativeChecks(seen["ftdi"], findTool("openocd", "HWLAB_OPENOCD"))...)
	r.Checks = append(r.Checks, espUSBJTAGChecks(seen["espusbjtag"], findTool("openocd", "HWLAB_OPENOCD"))...)

	r.Checks = append(r.Checks, stlinkChecks(seen["stlink"])...)
	r.Checks = append(r.Checks, cmsisdapChecks(seen["cmsisdap"])...)
	r.Checks = append(r.Checks, jlinkNativeChecks(seen["jlinknative"])...)

	if seen["telink"] {
		add("Telink TLSR825x: native SWire", CheckOK, "available (Go serial + SWire, no Python or probe-rs needed)")
	} else {
		add("Telink TLSR825x: native SWire", CheckWarn, "telink plugin not installed")
	}
	for _, o := range []struct{ name, label, env string }{
		{"TlsrPgm.py", "Telink TlsrPgm.py", "HWLAB_TLSRPGM"},
		{"probe-rs", "Telink probe-rs (tc32 fork)", "HWLAB_PROBE_RS"},
	} {
		if p := findTool(o.name, o.env); p != "" {
			add(o.label+" (optional)", CheckOK, p+" found; hwlab does not call it, the native backend is used")
		} else {
			add(o.label+" (optional)", CheckOK, "not installed; not needed, hwlab does not call it")
		}
	}

	addr := os.Getenv("HWLAB_SALEAE_ADDR")
	if addr == "" {
		addr = "127.0.0.1:10430"
	}
	if c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
		c.Close()
		add("Saleae Logic 2 automation", CheckOK, addr+" reachable")
	} else {
		add("Saleae Logic 2 automation", CheckWarn, addr+" not reachable (Logic 2 not running with automation)")
	}

	if _, err := os.ReadDir("/sys/kernel/debug/usb/usbmon"); err == nil {
		add("usbmon (live USB capture)", CheckOK, "readable")
	} else {
		add("usbmon (live USB capture)", CheckWarn, "not readable: live USB capture unavailable, recorded usbmon files still work")
	}
	return r
}

func writable(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".doctor-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

func findTool(name, env string, extra ...string) string {
	var cands []string
	if env != "" {
		cands = append(cands, os.Getenv(env))
	}
	if p, err := exec.LookPath(name); err == nil {
		cands = append(cands, p)
	}
	cands = append(cands, extra...)
	for _, p := range cands {
		if p == "" {
			continue
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			if abs, err := filepath.Abs(p); err == nil {
				return abs
			}
			return p
		}
	}
	return ""
}

// ccDebuggerNativeChecks reports the native usbfs CC-Debugger backend on its
// own, independent of the optional cc-tool fallback.
func ccDebuggerNativeChecks(installed bool) []DoctorCheck {
	const name = "CC-Debugger: native usbfs backend"
	if !installed {
		return []DoctorCheck{{name, CheckWarn, "ccdebugger plugin not installed"}}
	}
	if runtime.GOOS != "linux" {
		return []DoctorCheck{{name, CheckWarn, "native USB backend is Linux-only; use the optional cc-tool fallback"}}
	}
	ents, _ := filepath.Glob("/sys/bus/usb/devices/*/idProduct")
	read := func(dir, f string) string {
		b, _ := os.ReadFile(filepath.Join(dir, f))
		return strings.TrimSpace(string(b))
	}
	var found, denied []string
	for _, e := range ents {
		dir := filepath.Dir(e)
		if read(dir, "idVendor") != "0451" || read(dir, "idProduct") != "16a2" {
			continue
		}
		bus, _ := strconv.Atoi(read(dir, "busnum"))
		dev, _ := strconv.Atoi(read(dir, "devnum"))
		node := fmt.Sprintf("/dev/bus/usb/%03d/%03d", bus, dev)
		f, err := os.OpenFile(node, os.O_RDWR, 0)
		if err != nil {
			denied = append(denied, node+": "+err.Error())
			continue
		}
		f.Close()
		found = append(found, node)
	}
	switch {
	case len(denied) > 0:
		return []DoctorCheck{{name, CheckWarn, "CC Debugger present but not accessible (add a udev rule, see docs/hardware.md): " + strings.Join(denied, "; ")}}
	case len(found) > 0:
		return []DoctorCheck{{name, CheckOK, "CC Debugger 0451:16a2 accessible via usbfs at " + strings.Join(found, ", ") + " (no libusb, Python or cc-tool needed)"}}
	}
	return []DoctorCheck{{name, CheckOK, "available (Go usbfs, no libusb, Python or cc-tool needed); no CC Debugger connected"}}
}

// ftdiNativeChecks reports the native FTDI/MPSSE JTAG backend: it needs only
// Linux usbfs. OpenOCD is an optional cross-check tool, never a requirement.
func ftdiNativeChecks(installed bool, openocd string) []DoctorCheck {
	const name = "FTDI/MPSSE: native backend"
	if !installed {
		return []DoctorCheck{{name, CheckWarn, "ftdi plugin not installed"}}
	}
	oc := DoctorCheck{"FTDI/MPSSE: OpenOCD (optional cross-check)", CheckOK, "not installed; not needed, the native backend does not use it"}
	if openocd != "" {
		oc.Detail = openocd + " found; hwlab does not call it, the native backend is used"
	}
	if runtime.GOOS != "linux" {
		return []DoctorCheck{{name, CheckWarn, "native USB backend is Linux-only"}, oc}
	}
	ents, _ := filepath.Glob("/sys/bus/usb/devices/*/idProduct")
	read := func(dir, f string) string {
		b, _ := os.ReadFile(filepath.Join(dir, f))
		return strings.TrimSpace(string(b))
	}
	// iface describes the kernel driver and tty bound to interface n.
	iface := func(dir string, n int) string {
		base := fmt.Sprintf("%s:1.%d", dir, n)
		drv := "no driver"
		if l, err := os.Readlink(filepath.Join(base, "driver")); err == nil {
			drv = filepath.Base(l)
		}
		if tty, _ := filepath.Glob(filepath.Join(base, "ttyUSB*")); len(tty) > 0 {
			return drv + ", /dev/" + filepath.Base(tty[0])
		}
		return drv
	}
	var checks []DoctorCheck
	found := 0
	for _, e := range ents {
		dir := filepath.Dir(e)
		if read(dir, "idVendor") != "0403" || read(dir, "idProduct") != "6010" {
			continue
		}
		found++
		bus, _ := strconv.Atoi(read(dir, "busnum"))
		dev, _ := strconv.Atoi(read(dir, "devnum"))
		node := fmt.Sprintf("/dev/bus/usb/%03d/%03d", bus, dev)
		label := strings.TrimSpace(read(dir, "manufacturer") + " " + read(dir, "product"))
		detail := fmt.Sprintf("0403:6010 %s; JTAG interface: channel A (%s); companion UART: channel B (%s)", label, iface(dir, 0), iface(dir, 1))
		checks = append(checks, DoctorCheck{"FTDI/MPSSE: detected", CheckOK, detail})
		f, err := os.OpenFile(node, os.O_RDWR, 0)
		if err != nil {
			checks = append(checks, DoctorCheck{"FTDI/MPSSE: access", CheckWarn, node + " not accessible (add a udev rule, see docs/hardware.md): " + err.Error()})
			continue
		}
		f.Close()
		checks = append(checks, DoctorCheck{"FTDI/MPSSE: access", CheckOK, node + " opens read-write; only the JTAG interface is claimed, the UART stays with ftdi_sio"})
	}
	if found == 0 {
		checks = append(checks, DoctorCheck{"FTDI/MPSSE: detected", CheckOK, "no FTDI 0403:6010 adapter connected"})
	}
	head := DoctorCheck{name, CheckOK, "available (Go usbfs; platform Linux; CGO no; no libusb, OpenOCD or Python needed)"}
	return append(append([]DoctorCheck{head}, checks...), oc)
}

// espCapabilityChecks lists every capability with its own status from the
// support registry; a capability is never upgraded because a neighbour works.
func espCapabilityChecks() []DoctorCheck {
	e, ok := status.Lookup("espusbjtag")
	if !ok || len(e.Backends) == 0 {
		return nil
	}
	var out []DoctorCheck
	for _, c := range e.Backends[0].Capabilities {
		level := CheckOK
		if c.Status != status.CapRealHW {
			level = CheckWarn
		}
		out = append(out, DoctorCheck{"ESP capability: " + c.Name, level, string(c.Status) + ": " + c.Evidence})
	}
	return out
}

// espUSBJTAGChecks reports the native Espressif USB Serial/JTAG backend. It
// needs only Linux usbfs; OpenOCD and ESP-IDF are optional references.
func espUSBJTAGChecks(installed bool, openocd string) []DoctorCheck {
	const name = "ESP USB Serial/JTAG: native backend"
	if !installed {
		return []DoctorCheck{{name, CheckWarn, "espusbjtag plugin not installed"}}
	}
	oc := DoctorCheck{"ESP USB Serial/JTAG: OpenOCD (optional cross-check)", CheckOK, "not installed; not needed, the native backend does not use it"}
	if openocd != "" {
		oc.Detail = openocd + " found; hwlab does not call it, the native backend is used"
	}
	if runtime.GOOS != "linux" {
		return []DoctorCheck{{name, CheckWarn, "native USB backend is Linux-only"}, oc}
	}
	ents, _ := filepath.Glob("/sys/bus/usb/devices/*/idProduct")
	read := func(dir, f string) string {
		b, _ := os.ReadFile(filepath.Join(dir, f))
		return strings.TrimSpace(string(b))
	}
	// ifaceInfo describes the kernel driver and tty bound to interface n.
	ifaceInfo := func(dir string, n int) string {
		base := fmt.Sprintf("%s:1.%d", dir, n)
		drv := "no driver"
		if l, err := os.Readlink(filepath.Join(base, "driver")); err == nil {
			drv = filepath.Base(l)
		}
		return drv
	}
	var checks []DoctorCheck
	found := 0
	for _, e := range ents {
		dir := filepath.Dir(e)
		if read(dir, "idVendor") != "303a" || read(dir, "idProduct") != "1001" {
			continue
		}
		found++
		bus, _ := strconv.Atoi(read(dir, "busnum"))
		dev, _ := strconv.Atoi(read(dir, "devnum"))
		node := fmt.Sprintf("/dev/bus/usb/%03d/%03d", bus, dev)
		tty := ""
		if m, _ := filepath.Glob(filepath.Join(dir+":1.0", "tty", "ttyACM*")); len(m) > 0 {
			tty = ", /dev/" + filepath.Base(m[0])
		}
		label := strings.TrimSpace(read(dir, "manufacturer") + " " + read(dir, "product"))
		detail := fmt.Sprintf("303a:1001 %s serial %s; JTAG interface 2 (%s); companion CDC serial interfaces 0/1 (%s%s)",
			label, read(dir, "serial"), ifaceInfo(dir, 2), ifaceInfo(dir, 0), tty)
		checks = append(checks, DoctorCheck{"ESP USB Serial/JTAG: detected", CheckOK, detail})
		f, err := os.OpenFile(node, os.O_RDWR, 0)
		if err != nil {
			checks = append(checks, DoctorCheck{"ESP USB Serial/JTAG: access", CheckWarn, node + " not accessible (add a udev rule, see docs/hardware.md): " + err.Error()})
			continue
		}
		f.Close()
		checks = append(checks, DoctorCheck{"ESP USB Serial/JTAG: access", CheckOK, node + " opens read-write; only interface 2 is claimed, cdc_acm keeps the serial port"})
	}
	if found == 0 {
		checks = append(checks, DoctorCheck{"ESP USB Serial/JTAG: detected", CheckOK, "no Espressif 303a:1001 device connected"})
	}
	head := DoctorCheck{name, CheckOK, "available (Go usbfs; platform Linux; CGO no; no libusb, OpenOCD, esptool, ESP-IDF or Python needed)"}
	checks = append(checks, espCapabilityChecks()...)
	return append(append([]DoctorCheck{head}, checks...), oc)
}

// stlinkChecks reports the native ST-Link backend: probes on the bus, access to
// them, and every capability graded separately from the support registry.
func stlinkChecks(installed bool) []DoctorCheck {
	const name = "ST-Link: native backend"
	if !installed {
		return []DoctorCheck{{name, CheckWarn, "stlink plugin not installed"}}
	}
	if runtime.GOOS != "linux" {
		return []DoctorCheck{{name, CheckWarn, "native USB backend is Linux-only"}}
	}
	checks := []DoctorCheck{{name, CheckOK, "available (Go usbfs; platform Linux; CGO no; no libusb, OpenOCD, STM32CubeProgrammer, st-flash or Python needed)"}}
	read := func(dir, f string) string {
		b, _ := os.ReadFile(filepath.Join(dir, f))
		return strings.TrimSpace(string(b))
	}
	ents, _ := filepath.Glob("/sys/bus/usb/devices/*/idProduct")
	found := 0
	for _, e := range ents {
		dir := filepath.Dir(e)
		if read(dir, "idVendor") != "0483" || read(dir, "idProduct") != "3748" {
			continue
		}
		found++
		bus, _ := strconv.Atoi(read(dir, "busnum"))
		dev, _ := strconv.Atoi(read(dir, "devnum"))
		node := fmt.Sprintf("/dev/bus/usb/%03d/%03d", bus, dev)
		checks = append(checks, DoctorCheck{"ST-Link: detected", CheckOK, "0483:3748 " + read(dir, "product") + "; select it by its USB serial (hwlab devices)"})
		f, err := os.OpenFile(node, os.O_RDWR, 0)
		if err != nil {
			checks = append(checks, DoctorCheck{"ST-Link: access", CheckWarn, node + " not accessible (add a udev rule, see docs/hardware.md): " + err.Error()})
			continue
		}
		f.Close()
		checks = append(checks, DoctorCheck{"ST-Link: access", CheckOK, node + " opens read-write"})
	}
	if found == 0 {
		checks = append(checks, DoctorCheck{"ST-Link: detected", CheckOK, "no ST-Link V2 (0483:3748) connected"})
	}
	if e, ok := status.Lookup("stlink"); ok && len(e.Backends) > 0 {
		for _, c := range e.Backends[0].Capabilities {
			level := CheckOK
			if c.Status != status.CapRealHW {
				level = CheckWarn
			}
			checks = append(checks, DoctorCheck{"ST-Link capability: " + c.Name, level, string(c.Status) + ": " + c.Evidence})
		}
	}
	return checks
}

// cmsisdapChecks reports the native CMSIS-DAP backend: probes on the bus
// (found by their "CMSIS-DAP" interface string, as the plugin does), whether the
// DAP interface can be opened, which kernel drivers sibling interfaces keep,
// and every capability graded separately from the support registry.
func cmsisdapChecks(installed bool) []DoctorCheck {
	const name = "CMSIS-DAP: native backend"
	if !installed {
		return []DoctorCheck{{name, CheckWarn, "cmsisdap plugin not installed"}}
	}
	if runtime.GOOS != "linux" {
		return []DoctorCheck{{name, CheckWarn, "native USB backend is Linux-only"}}
	}
	checks := []DoctorCheck{{name, CheckOK, "available (Go usbfs bulk v2 + hidraw v1; platform Linux; CGO no; no libusb, OpenOCD, probe-rs or Python needed)"}}
	read := func(dir, f string) string {
		b, _ := os.ReadFile(filepath.Join(dir, f))
		return strings.TrimSpace(string(b))
	}
	ifaces, _ := filepath.Glob("/sys/bus/usb/devices/*:*/interface")
	found := 0
	for _, ifp := range ifaces {
		idir := filepath.Dir(ifp)
		if !strings.Contains(strings.ToLower(read(idir, "interface")), "cmsis-dap") {
			continue
		}
		dir := filepath.Dir(idir)
		found++
		kind := "v2 bulk"
		if read(idir, "bInterfaceClass") == "03" {
			kind = "v1 HID"
		}
		serial := read(dir, "serial")
		if serial == "" {
			serial = "(none: the id falls back to the USB topology)"
		}
		checks = append(checks, DoctorCheck{"CMSIS-DAP: detected", CheckOK, fmt.Sprintf("%s:%s %s, %s; select it by USB serial %s (hwlab devices)",
			read(dir, "idVendor"), read(dir, "idProduct"), read(dir, "product"), kind, serial)})
		var siblings []string
		others, _ := filepath.Glob(dir + "/" + filepath.Base(dir) + ":*")
		for _, o := range others {
			if o == idir {
				continue
			}
			drv, _ := os.Readlink(filepath.Join(o, "driver"))
			if drv == "" {
				drv = "no driver"
			} else {
				drv = filepath.Base(drv)
			}
			siblings = append(siblings, filepath.Base(o)+"="+drv)
		}
		if len(siblings) > 0 {
			checks = append(checks, DoctorCheck{"CMSIS-DAP: other interfaces", CheckOK, strings.Join(siblings, ", ") + " (hwlab claims only the DAP interface and never detaches a driver)"})
		}
		if kind == "v1 HID" {
			continue
		}
		bus, _ := strconv.Atoi(read(dir, "busnum"))
		dev, _ := strconv.Atoi(read(dir, "devnum"))
		node := fmt.Sprintf("/dev/bus/usb/%03d/%03d", bus, dev)
		f, err := os.OpenFile(node, os.O_RDWR, 0)
		if err != nil {
			checks = append(checks, DoctorCheck{"CMSIS-DAP: access", CheckWarn, node + " not accessible (add a udev rule, see docs/hardware.md): " + err.Error()})
			continue
		}
		f.Close()
		checks = append(checks, DoctorCheck{"CMSIS-DAP: access", CheckOK, node + " opens read-write"})
	}
	if found == 0 {
		checks = append(checks, DoctorCheck{"CMSIS-DAP: detected", CheckOK, "no CMSIS-DAP probe connected"})
	}
	if e, ok := status.Lookup("cmsisdap"); ok && len(e.Backends) > 0 {
		for _, c := range e.Backends[0].Capabilities {
			level := CheckOK
			if c.Status != status.CapRealHW {
				level = CheckWarn
			}
			checks = append(checks, DoctorCheck{"CMSIS-DAP capability: " + c.Name, level, string(c.Status) + ": " + c.Evidence})
		}
	}
	return checks
}

// jlinkNativeChecks reports the native J-Link backend (jlink-native): SEGGER
// probes on the bus, whether the device node opens, a probe stuck in its
// bootloader (it needs a physical replug) and every capability graded
// separately. Nothing here talks to the probe.
func jlinkNativeChecks(installed bool) []DoctorCheck {
	const name = "J-Link: native backend (jlink-native)"
	if !installed {
		return []DoctorCheck{{name, CheckWarn, "jlinknative plugin not installed"}}
	}
	if runtime.GOOS != "linux" {
		return []DoctorCheck{{name, CheckWarn, "native USB backend is Linux-only"}}
	}
	checks := []DoctorCheck{{name, CheckOK, "available (Go usbfs; platform Linux; CGO no; no JLinkExe, libjlinkarm, libusb, OpenOCD or Python; never falls back to jlink-segger)"}}
	read := func(dir, f string) string {
		b, _ := os.ReadFile(filepath.Join(dir, f))
		return strings.TrimSpace(string(b))
	}
	devs, _ := filepath.Glob("/sys/bus/usb/devices/*")
	found := 0
	for _, dir := range devs {
		if read(dir, "idVendor") != "1366" {
			continue
		}
		found++
		pid := read(dir, "idProduct")
		serial := read(dir, "serial")
		serial = strings.TrimLeft(serial, "0")
		if pid == "0101" {
			checks = append(checks, DoctorCheck{"J-Link: detected", CheckWarn, "1366:0101 runs its bootloader and answers no commands: unplug and replug it"})
			continue
		}
		checks = append(checks, DoctorCheck{"J-Link: detected", CheckOK, fmt.Sprintf("1366:%s %s; the native id is jlink-native:%s (USB serial; bus/device numbers are never identity)", pid, read(dir, "product"), serial)})
		bus, _ := strconv.Atoi(read(dir, "busnum"))
		dev, _ := strconv.Atoi(read(dir, "devnum"))
		node := fmt.Sprintf("/dev/bus/usb/%03d/%03d", bus, dev)
		f, err := os.OpenFile(node, os.O_RDWR, 0)
		if err != nil {
			checks = append(checks, DoctorCheck{"J-Link: access", CheckWarn, node + " not accessible (add a udev rule, see docs/hardware.md): " + err.Error()})
			continue
		}
		f.Close()
		checks = append(checks, DoctorCheck{"J-Link: access", CheckOK, node + " opens read-write"})
	}
	if found == 0 {
		checks = append(checks, DoctorCheck{"J-Link: detected", CheckOK, "no J-Link connected"})
	}
	if e, ok := status.Lookup("jlinknative"); ok && len(e.Backends) > 0 {
		for _, c := range e.Backends[0].Capabilities {
			level := CheckOK
			if c.Status != status.CapRealHW {
				level = CheckWarn
			}
			checks = append(checks, DoctorCheck{"J-Link native capability: " + c.Name, level, string(c.Status) + ": " + c.Evidence})
		}
	}
	return checks
}
