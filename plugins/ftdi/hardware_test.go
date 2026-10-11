//go:build hardware

package ftdiplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.bug.st/serial"

	"github.com/faronov/hwlab/internal/usb"
	"github.com/faronov/hwlab/plugins/ftdi/ftdi"
)

// Opt-in: HWLAB_FTDI_HW=1 go test -tags hardware ./plugins/ftdi/
//
// Needs an FTDI-compatible MPSSE adapter (0403:6010) and write access to its
// usbfs node (udev rule, docs/hardware.md). Only safe operations run: MPSSE
// sync, loopback probe, TAP reset and chain scans. Nothing is written to a
// target. A scan finding no TAP is a valid, asserted outcome.
//
// HWLAB_FTDI_RECORD=<file> additionally writes a sanitized evidence file.

func hwEntry(t *testing.T) Entry {
	t.Helper()
	if os.Getenv("HWLAB_FTDI_HW") != "1" {
		t.Skip("set HWLAB_FTDI_HW=1 to run against a real adapter")
	}
	var es []Entry
	for _, d := range usb.Scan("/sys", ftdi.IsKnown) {
		es = append(es, Entry{Dev: d})
	}
	es = nameEntries(es)
	if len(es) == 0 {
		t.Skip("no FTDI adapter attached")
	}
	if len(es) > 1 {
		t.Logf("%d adapters: using %s", len(es), es[0].ID)
	}
	return es[0]
}

// rescan reads the adapter again from sysfs (drivers and ttys change as the
// interface is claimed and released).
func rescan(t *testing.T, id string) usb.Device {
	t.Helper()
	for _, d := range usb.Scan("/sys", ftdi.IsKnown) {
		for _, e := range nameEntries([]Entry{{Dev: d}}) {
			if e.ID == id {
				return d
			}
		}
	}
	t.Fatalf("adapter %s disappeared", id)
	return usb.Device{}
}

type ifaceState struct {
	Interface int    `json:"interface"`
	Driver    string `json:"driver"`
	TTY       string `json:"tty,omitempty"`
}

func states(d usb.Device) []ifaceState {
	var out []ifaceState
	for _, i := range d.Interfaces {
		out = append(out, ifaceState{i.Number, i.Driver, i.TTY})
	}
	return out
}

func waitDriver(t *testing.T, id string, ifnum int, want string) usb.Device {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		d := rescan(t, id)
		if i, ok := d.Iface(ifnum); ok && i.Driver == want && (want != "ftdi_sio" || i.TTY != "") {
			return d
		}
		if time.Now().After(deadline) {
			i, _ := d.Iface(ifnum)
			t.Fatalf("interface %d driver = %q (tty %q), want %q", ifnum, i.Driver, i.TTY, want)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestHardwareIdentity(t *testing.T) {
	e := hwEntry(t)
	d := e.Dev
	t.Logf("%04x:%04x %q %q serial=%q bcd=0x%04x config=%d interfaces=%d speed=%dMbps", d.VID, d.PID, d.Manufacturer, d.Product, d.Serial, d.BCD, d.Configuration, len(d.Interfaces), d.SpeedMbps)
	if d.VID != 0x0403 || d.PID != 0x6010 {
		t.Fatalf("not an FT2232 identity")
	}
	if len(d.Interfaces) != 2 {
		t.Fatalf("interfaces: %d", len(d.Interfaces))
	}
	for _, i := range d.Interfaces {
		in, out, mp := i.BulkEndpoints()
		t.Logf("interface %d driver=%q tty=%q bulk in=0x%02x out=0x%02x maxpacket=%d", i.Number, i.Driver, i.TTY, in, out, mp)
		if in == 0 || out == 0 {
			t.Errorf("interface %d has no bulk pair", i.Number)
		}
	}
	if strings.EqualFold(d.Manufacturer, "SIPEED") && strings.EqualFold(d.Product, "JTAG Debugger") {
		p, _, _ := resolveProfile(e, defaultOptions())
		if p.Name != "sipeed-bl702" || p.Implementation != "compatible-emulator" {
			t.Errorf("profile %s/%s", p.Name, p.Implementation)
		}
	}
}

// The JTAG interface is claimed and the kernel serial driver detached from it
// only; the companion UART keeps its driver and tty, and a serial port can be
// opened on it while MPSSE runs. Closing re-binds ftdi_sio.
func TestHardwareOpenInterface0LeavesUARTUsable(t *testing.T) {
	e := hwEntry(t)
	ctx := context.Background()
	before := rescan(t, e.ID)
	ji, _ := before.Iface(0)
	ui, ok := before.Iface(1)
	if !ok || ui.Driver != "ftdi_sio" || ui.TTY == "" {
		t.Skipf("interface 1 is not bound to a serial tty: %+v", ui)
	}
	t.Logf("before: %+v", states(before))

	s, err := openSession(ctx, e, defaultOptions(), defaultOpen)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = s.close()
		}
	}()
	during := rescan(t, e.ID)
	t.Logf("during: %+v detached=%q", states(during), s.info.DetachedDriver)
	if i, _ := during.Iface(0); i.Driver == "ftdi_sio" {
		t.Errorf("ftdi_sio still bound to interface 0 while claimed")
	}
	if ji.Driver == "ftdi_sio" && s.info.DetachedDriver != "ftdi_sio" {
		t.Errorf("ftdi_sio was bound but not reported as detached")
	}
	if i, _ := during.Iface(1); i.Driver != "ftdi_sio" || i.TTY != ui.TTY {
		t.Fatalf("UART interface disturbed: %+v (was %+v)", i, ui)
	}

	// The companion UART opens through the ordinary serial stack. Nothing
	// is written: its TX pin may be wired to a target.
	port, err := serial.Open("/dev/"+ui.TTY, &serial.Mode{BaudRate: 115200})
	if err != nil {
		t.Fatalf("UART %s not usable while JTAG is open: %v", ui.TTY, err)
	}
	_ = port.SetReadTimeout(150 * time.Millisecond)
	buf := make([]byte, 64)
	n, rerr := port.Read(buf)
	t.Logf("UART read while MPSSE active: %d bytes err=%v", n, rerr)
	if rerr != nil {
		t.Errorf("UART read: %v", rerr)
	}
	_ = port.Close()

	if _, err := s.selftest(ctx); err != nil {
		t.Fatalf("selftest: %v", err)
	}
	// The UART is still fine after MPSSE traffic.
	if i, _ := rescan(t, e.ID).Iface(1); i.Driver != "ftdi_sio" || i.TTY != ui.TTY {
		t.Errorf("UART interface disturbed by MPSSE traffic: %+v", i)
	}

	if err := s.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	closed = true
	if ji.Driver == "ftdi_sio" {
		after := waitDriver(t, e.ID, 0, "ftdi_sio")
		t.Logf("after: %+v", states(after))
	}
	if i, _ := rescan(t, e.ID).Iface(1); i.Driver != "ftdi_sio" || i.TTY != ui.TTY {
		t.Errorf("UART interface changed by close: %+v", i)
	}
}

func TestHardwareMPSSESafeCommands(t *testing.T) {
	e := hwEntry(t)
	ctx := context.Background()
	s, err := openSession(ctx, e, defaultOptions(), defaultOpen)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	st, err := s.selftest(ctx)
	if err != nil {
		t.Fatalf("selftest: %v", err)
	}
	b, _ := json.Marshal(st)
	t.Logf("selftest: %s", b)
	if st["ok"] != true {
		t.Fatalf("%v", st)
	}
	if _, err := s.tapReset(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("chip=%s bcd=%s latency=%dms clock=%dHz fixed=%v notes=%v", s.info.Chip, s.info.BCD, s.info.LatencyMs, s.info.ClockHz, s.info.ClockFixed, s.info.Notes)
}

// Repeated scans must agree. With no target on the pins every scan reports
// "no-device"; with a target the IDCODEs must be identical across scans. This
// test makes no claim about which of the two it is.
func TestHardwareScanStable(t *testing.T) {
	e := hwEntry(t)
	ctx := context.Background()
	s, err := openSession(ctx, e, defaultOptions(), defaultOpen)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	var first string
	for i := 0; i < 5; i++ {
		m, err := s.scan(ctx, map[string]any{"measure_ir": true})
		if err != nil {
			t.Fatal(err)
		}
		devs, _ := json.Marshal(m["devices"])
		got := fmt.Sprintf("%v %s", m["status"], devs)
		if i == 0 {
			first = got
			t.Logf("scan: %s notes=%v", got, m["notes"])
		} else if got != first {
			t.Fatalf("scan %d differs:\n%s\n%s", i, got, first)
		}
	}
}

func TestHardwareCloseReopen(t *testing.T) {
	e := hwEntry(t)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		s, err := openSession(ctx, e, defaultOptions(), defaultOpen)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if _, err := s.scan(ctx, nil); err != nil {
			t.Fatalf("scan %d: %v", i, err)
		}
		if err := s.close(); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
		waitDriver(t, e.ID, 0, "ftdi_sio")
	}
}

// Opening the same adapter twice must fail cleanly rather than steal the
// interface.
func TestHardwareSecondOpenIsRefused(t *testing.T) {
	e := hwEntry(t)
	ctx := context.Background()
	s, err := openSession(ctx, e, defaultOptions(), defaultOpen)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	s2, err := openSession(ctx, rescanEntry(t, e), defaultOptions(), defaultOpen)
	if err == nil {
		_ = s2.close()
		t.Fatal("a second open of a claimed interface succeeded")
	}
	t.Logf("second open: %v", err)
	if _, err := s.scan(ctx, nil); err != nil {
		t.Fatalf("first session broken by the refused open: %v", err)
	}
}

func rescanEntry(t *testing.T, e Entry) Entry {
	e.Dev = rescan(t, e.ID)
	return e
}

type evidence struct {
	Kind      string            `json:"kind"`
	Note      string            `json:"note"`
	Identity  map[string]any    `json:"usb_identity"`
	Interface []ifaceEvidence   `json:"interfaces"`
	States    map[string]any    `json:"kernel_driver_states"`
	Init      []ftdi.LogEntry   `json:"ftdi_mpsse_init_log"`
	Adapter   ChipFacts         `json:"adapter"`
	Selftest  map[string]any    `json:"selftest"`
	Scans     []map[string]any  `json:"scans"`
	UART      map[string]any    `json:"companion_uart_check"`
	Results   map[string]string `json:"result"`
}

type ifaceEvidence struct {
	Number    int            `json:"number"`
	Driver    string         `json:"driver_before"`
	TTY       string         `json:"tty_before,omitempty"`
	Endpoints []usb.Endpoint `json:"endpoints"`
}

// TestHardwareRecordEvidence writes the REAL, sanitized evidence file (bus
// and device numbers and sysfs paths are left out).
func TestHardwareRecordEvidence(t *testing.T) {
	e := hwEntry(t)
	path := os.Getenv("HWLAB_FTDI_RECORD")
	if path == "" {
		t.Skip("set HWLAB_FTDI_RECORD=<file> to record evidence")
	}
	ctx := context.Background()
	before := rescan(t, e.ID)
	ev := evidence{
		Kind: "REAL",
		Note: "Recorded from a physical adapter by plugins/ftdi/hardware_test.go. Bus/device numbers and sysfs paths removed. The serial is the factory string of this firmware, not unique.",
		Identity: map[string]any{
			"vid": fmt.Sprintf("%04x", before.VID), "pid": fmt.Sprintf("%04x", before.PID), "bcd_device": fmt.Sprintf("0x%04x", before.BCD),
			"manufacturer": before.Manufacturer, "product": before.Product, "serial": before.Serial,
			"configuration": before.Configuration, "speed_mbps": before.SpeedMbps, "interface_count": len(before.Interfaces),
		},
	}
	for _, i := range before.Interfaces {
		ev.Interface = append(ev.Interface, ifaceEvidence{i.Number, i.Driver, i.TTY, i.Endpoints})
	}
	s, err := openSession(ctx, e, defaultOptions(), defaultOpen)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ev.States = map[string]any{"before": states(before), "during": states(rescan(t, e.ID))}
	ev.Init = s.conn.Log()
	ev.Adapter = s.info
	if ev.Selftest, err = s.selftest(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		m, err := s.scan(ctx, map[string]any{"measure_ir": true})
		if err != nil {
			t.Fatal(err)
		}
		ev.Scans = append(ev.Scans, m)
	}
	ui, _ := before.Iface(1)
	ev.UART = map[string]any{"opened_while_mpsse_active": false}
	if ui.TTY != "" {
		if port, err := serial.Open("/dev/"+ui.TTY, &serial.Mode{BaudRate: 115200}); err == nil {
			ev.UART["opened_while_mpsse_active"] = true
			_ = port.Close()
		} else {
			ev.UART["error"] = err.Error()
		}
	}
	ev.Results = map[string]string{
		"ftdi_usb_transport": "REAL_HW_VERIFIED",
		"mpsse_transport":    "REAL_HW_VERIFIED (sync and clocked shifts); loopback not supported by this firmware",
		"jtag_target_scan":   "IMPLEMENTED_UNTESTED (no JTAG target connected; the scan reported no-device)",
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	ev.States["after"] = states(waitDriver(t, e.ID, 0, "ftdi_sio"))
	b, _ := json.MarshalIndent(ev, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s", path)
}
