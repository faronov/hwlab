package xds110plugin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/usb"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/plugins/xds110/xds"
)

const usbID = "0451:bef3:SER1"

func simPlugin(t *testing.T) (*Plugin, *xds.Sim) {
	t.Helper()
	sim := xds.NewSim()
	u := &usb.Device{VID: 0x0451, PID: 0xbef3, Serial: "SER1", Product: "XDS110 (test)", Bus: 1, Address: 5}
	list := func() []Entry { return []Entry{{ID: SimID, Sim: sim}, {ID: usbID, USB: u}} }
	open := func(e Entry) (xds.Transport, error) {
		if e.Sim != nil {
			return e.Sim, nil
		}
		return nil, errors.New("no such device")
	}
	return NewWith(list, open), sim
}

func invoke(t *testing.T, p *Plugin, dev, capName, method string, args map[string]any) (map[string]any, error) {
	t.Helper()
	s, err := structpb.NewStruct(args)
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: dev, Capability: capName, Method: method, Args: s})
	if err != nil {
		return nil, err
	}
	return r.Result.AsMap(), nil
}

func mustInvoke(t *testing.T, p *Plugin, capName, method string, args map[string]any) map[string]any {
	t.Helper()
	r, err := invoke(t, p, SimID, capName, method, args)
	if err != nil {
		t.Fatalf("%s.%s: %v", capName, method, err)
	}
	return r
}

func openSim(t *testing.T, p *Plugin, cfg map[string]any) {
	t.Helper()
	if err := p.OpenDevice(context.Background(), SimID, cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.CloseDevice(context.Background(), SimID) })
}

func TestDeviceIDUsesSerialNotBusAddress(t *testing.T) {
	a := usb.Device{VID: 0x451, PID: 0xbef3, Serial: "LS1", Bus: 2, Address: 9, SysName: "2-2"}
	b := a
	b.Bus, b.Address, b.SysName = 1, 44, "1-4"
	if DeviceID(a) != "0451:bef3:LS1" || DeviceID(a) != DeviceID(b) {
		t.Fatalf("%s %s", DeviceID(a), DeviceID(b))
	}
	a.Serial = ""
	if DeviceID(a) != "0451:bef3@2-2" {
		t.Fatalf("%s", DeviceID(a))
	}
}

func TestListDevices(t *testing.T) {
	p, _ := simPlugin(t)
	ds, err := p.ListDevices(context.Background())
	if err != nil || len(ds) != 2 {
		t.Fatalf("%v %v", ds, err)
	}
	if ds[0].Properties["synthetic"] != "true" || ds[0].Transport != "fake" {
		t.Errorf("sim not marked synthetic: %+v", ds[0])
	}
	if ds[1].Serial != "SER1" || ds[1].Properties["synthetic"] != "" {
		t.Errorf("usb entry: %+v", ds[1])
	}
	var ids []string
	for _, c := range ds[1].Capabilities {
		ids = append(ids, c.Id)
	}
	if strings.Join(ids, ",") != "DebugProbe,MemoryAccess,FlashProgramming,RTT,LiveRawStream,PowerMeasurement,TimedCapture" {
		t.Errorf("only DebugProbe, MemoryAccess, CC23xx FlashProgramming, RTT, the capture stream and PowerMeasurement may be advertised, got %v", ids)
	}
	for _, c := range ds[1].Capabilities {
		if c.Id == pluginv1.CapPowerMeasurement && c.Attributes["energytrace"] != "PROTOCOL_PARTIAL" {
			t.Errorf("EnergyTrace must be advertised as PROTOCOL_PARTIAL: %v", c.Attributes)
		}
	}
	for _, c := range ds[0].Capabilities {
		if c.Id == pluginv1.CapPowerMeasurement {
			t.Error("the simulator has no EnergyTrace stream")
		}
	}
}

func TestInfoAdvertisesNoPowerSource(t *testing.T) {
	p, _ := simPlugin(t)
	info := p.Info()
	for _, c := range info.Capabilities {
		if c.Id == pluginv1.CapPowerSource {
			t.Fatalf("%s must not be advertised", c.Id)
		}
	}
	if got := strings.Join(info.DangerousConfigKeys, ","); got != "allow_target_power,allow_flash,allow_erase,allow_chip_erase,allow_ccfg_write" {
		t.Fatalf("dangerous config keys: %v", got)
	}
	danger := map[string]pluginv1.Danger{}
	for _, m := range info.Methods {
		danger[m.Capability+"."+m.Name] = m.Danger
	}
	for _, n := range []string{"halt", "resume", "step", "reset", "read_registers", "write_register", "target_power", "clock_probe", "jtag_scan"} {
		if danger["DebugProbe."+n] != pluginv1.Danger_DANGER_CONFIRM {
			t.Errorf("DebugProbe.%s must be CONFIRM", n)
		}
	}
	if danger["MemoryAccess.write"] != pluginv1.Danger_DANGER_CONFIRM {
		t.Error("MemoryAccess.write must be CONFIRM")
	}
	for _, n := range []string{"DebugProbe.info", "DebugProbe.connect", "DebugProbe.status", "MemoryAccess.read"} {
		if danger[n] != pluginv1.Danger_DANGER_SAFE {
			t.Errorf("%s must be SAFE", n)
		}
	}
}

func TestInfoAndConnect(t *testing.T) {
	p, _ := simPlugin(t)
	r := mustInvoke(t, p, "DebugProbe", "info", nil)
	if r["firmware"] != "3.0.0.22" || r["hardware_id"] != "0x23" || r["synthetic"] != true || r["validated"] != false {
		t.Fatalf("info %v", r)
	}
	c := mustInvoke(t, p, "DebugProbe", "connect", nil)
	if c["core"] != "Cortex-M4" || c["halted"] != false || c["dpidr"] == nil {
		t.Fatalf("connect %v", c)
	}
}

func TestHaltStepResetRegisters(t *testing.T) {
	p, _ := simPlugin(t)
	h := mustInvoke(t, p, "DebugProbe", "halt", nil)
	if h["halted"] != true || h["pc"] == nil {
		t.Fatalf("halt %v", h)
	}
	if s := mustInvoke(t, p, "DebugProbe", "status", nil); s["halted"] != true {
		t.Fatalf("a transient call lost the halt: %v", s)
	}
	r := mustInvoke(t, p, "DebugProbe", "read_registers", nil)
	if regs := r["registers"].(map[string]any); len(regs) != 20 || regs["pc"] != r["pc"] {
		t.Fatalf("registers %v", r)
	}
	s := mustInvoke(t, p, "DebugProbe", "step", map[string]any{"count": 20.0})
	if s["steps"] != 20.0 || s["all_halted"] != true || s["pc_changed"] != 20.0 {
		t.Fatalf("step %v", s)
	}
	mustInvoke(t, p, "DebugProbe", "write_register", map[string]any{"name": "R5", "value": "0xCAFEF00D"})
	for _, k := range []string{"sysreset", "core"} {
		rr := mustInvoke(t, p, "DebugProbe", "reset", map[string]any{"kind": k, "halt": true})
		if rr["halted"] != true || rr["at_reset_vector"] != true {
			t.Fatalf("%s: %v", k, rr)
		}
	}
	if _, err := invoke(t, p, SimID, "DebugProbe", "reset", map[string]any{"kind": "dap_reset"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("CMSIS-DAP's dap_reset does not exist here: %v", err)
	}
}

func TestHardwareResetNeedsEvidence(t *testing.T) {
	p, sim := simPlugin(t)
	before := sim.Resets()
	r := mustInvoke(t, p, "DebugProbe", "reset", map[string]any{"kind": "hardware", "halt": true})
	if r["halted"] != true || r["reset_seen"] != true || sim.Resets() <= before {
		t.Fatalf("hardware: %v", r)
	}
	sim.IgnoreSRST(true)
	_, err := invoke(t, p, SimID, "DebugProbe", "reset", map[string]any{"kind": "hardware"})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "NOT claimed") {
		t.Fatalf("an unobserved pin reset must be an error: %v", err)
	}
}

func TestBreakpointAndWatchpoint(t *testing.T) {
	p, _ := simPlugin(t)
	if _, err := invoke(t, p, SimID, "DebugProbe", "set_breakpoint", map[string]any{"address": 0x100}); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "open device") {
		t.Fatalf("a breakpoint without an open device must be refused: %v", err)
	}
	openSim(t, p, nil)
	start := mustInvoke(t, p, "DebugProbe", "halt", nil)["pc"]
	target := mustInvoke(t, p, "DebugProbe", "step", map[string]any{"count": 4.0})["pc"]
	mustInvoke(t, p, "DebugProbe", "write_register", map[string]any{"name": "pc", "value": start})
	b := mustInvoke(t, p, "DebugProbe", "set_breakpoint", map[string]any{"address": target, "run_wait_ms": 1000.0})
	if b["hit"] != true || b["halted_pc"] != target {
		t.Fatalf("breakpoint %v", b)
	}
	mustInvoke(t, p, "DebugProbe", "halt", nil)
	if c := mustInvoke(t, p, "DebugProbe", "clear_breakpoint", map[string]any{"address": target}); c["installed"] != 0.0 {
		t.Fatalf("clear %v", c)
	}
	w := mustInvoke(t, p, "DebugProbe", "set_watchpoint", map[string]any{"address": 0x20000100, "kind": "write", "mask_bits": 2.0})
	if w["size_bytes"] != 4.0 {
		t.Fatalf("watch %v", w)
	}
	if c := mustInvoke(t, p, "DebugProbe", "clear_watchpoint", map[string]any{"all": true}); c["installed"] != 0.0 {
		t.Fatalf("clear %v", c)
	}
}

func TestMemory(t *testing.T) {
	p, _ := simPlugin(t)
	w := mustInvoke(t, p, "MemoryAccess", "write", map[string]any{"address": float64(0x20000010), "data": "0xDEADBEEF01"})
	if w["verified"] != true || w["length"] != 5.0 {
		t.Fatalf("write %v", w)
	}
	if r := mustInvoke(t, p, "MemoryAccess", "read", map[string]any{"address": "0x20000010", "length": 5}); r["hex"] != "deadbeef01" {
		t.Fatalf("read back %v", r)
	}
	r := mustInvoke(t, p, "MemoryAccess", "read", map[string]any{"address": 0, "length": 8192})
	if r["hex"] != nil || r["truncated"] != true || len(r["sha256"].(string)) != 64 {
		t.Fatalf("large read must not inline data: %v", r)
	}
	if _, err := invoke(t, p, SimID, "MemoryAccess", "write", map[string]any{"address": 0x100, "data": "01020304"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("flash write: %v", err)
	}
	if _, err := invoke(t, p, SimID, "MemoryAccess", "read", map[string]any{"address": 0, "length": 0}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("zero length: %v", err)
	}
}

func TestNoTargetIsReportedAndProbeStillWorks(t *testing.T) {
	p, sim := simPlugin(t)
	sim.SetNoTarget(true)
	_, err := invoke(t, p, SimID, "DebugProbe", "connect", nil)
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "target connected") {
		t.Fatalf("got %v", err)
	}
	mustInvoke(t, p, "DebugProbe", "info", nil)
}

func TestClockProbeReportsEncodingNotPrecision(t *testing.T) {
	p, _ := simPlugin(t)
	r := mustInvoke(t, p, "DebugProbe", "clock_probe", map[string]any{"rates_hz": []any{100000.0, 1000000.0, 14000000.0, 50000.0}})
	rows := r["clocks"].([]any)
	if len(rows) != 4 {
		t.Fatalf("%v", r)
	}
	r1 := rows[1].(map[string]any)
	if r1["accepted"] != true || r1["delay_count"] != 17.0 {
		t.Fatalf("1 MHz row %v", r1)
	}
	if rows[2].(map[string]any)["delay_count"] != 0.0 || rows[3].(map[string]any)["clamped"] != true {
		t.Fatalf("rows %v", rows)
	}
	if !strings.Contains(r["note"].(string), "not proof") {
		t.Fatal("clock_probe must not claim the effective rate")
	}
}

func TestJTAGScan(t *testing.T) {
	p, _ := simPlugin(t)
	r := mustInvoke(t, p, "DebugProbe", "jtag_scan", nil)
	if r["status"] != "ok" || len(r["devices"].([]any)) != len(xds.SimIDCodes) {
		t.Fatalf("%v", r)
	}
}

// The LP-XDS110ET (hardware id 0x23) has no software-switchable target supply:
// target_power must say so instead of sending a command that is acknowledged
// and does nothing (measured on real hardware, docs/notes/phase27).
func TestTargetPowerUnsupportedOnLPXDS110ET(t *testing.T) {
	p, sim := simPlugin(t)
	openSim(t, p, map[string]any{"allow_target_power": true})
	for _, a := range []map[string]any{
		{"enable": true, "millivolts": 3300.0, "confirm": powerConfirm},
		{"enable": false},
	} {
		_, err := invoke(t, p, SimID, "DebugProbe", "target_power", a)
		wantErr(t, err, codes.FailedPrecondition, "UNSUPPORTED_BY_ATTACHED_HW")
		if !IsUnsupportedByHW(err) {
			t.Fatalf("not classified as unsupported by the attached hardware: %v", err)
		}
	}
	if _, on := sim.Supply(); on {
		t.Fatal("a supply command was sent to hardware without a supply")
	}
	r := mustInvoke(t, p, "DebugProbe", "info", nil)
	if r["target_supply_capable"] != false {
		t.Fatalf("target_supply_capable = %v on 0x23", r["target_supply_capable"])
	}
}

func TestTargetPowerGate(t *testing.T) {
	p, sim := simPlugin(t)
	sim.Hardware = xds.HWStandAlone
	// A one-shot call can never grant power, and nothing is switched.
	if _, err := invoke(t, p, SimID, "DebugProbe", "target_power", map[string]any{"enable": true, "millivolts": 3300.0, "confirm": powerConfirm, "allow_target_power": true}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a call must not be able to grant allow_target_power: %v", err)
	}
	if _, err := invoke(t, p, SimID, "DebugProbe", "target_power", map[string]any{"enable": true, "millivolts": 3300.0, "confirm": powerConfirm}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("without the device setting: %v", err)
	}
	openSim(t, p, nil)
	if _, err := invoke(t, p, SimID, "DebugProbe", "target_power", map[string]any{"enable": true, "millivolts": 3300.0, "confirm": powerConfirm}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("device opened without allow_target_power: %v", err)
	}
	if err := p.CloseDevice(context.Background(), SimID); err != nil {
		t.Fatal(err)
	}
	openSim(t, p, map[string]any{"allow_target_power": true})
	for _, a := range []map[string]any{
		{"enable": true, "millivolts": 3300.0},
		{"enable": true, "millivolts": 3300.0, "confirm": "yes"},
		{"millivolts": 3300.0, "confirm": powerConfirm},
	} {
		if _, err := invoke(t, p, SimID, "DebugProbe", "target_power", a); err == nil {
			t.Fatalf("accepted %v", a)
		}
	}
	if _, err := invoke(t, p, SimID, "DebugProbe", "target_power", map[string]any{"enable": true, "millivolts": 6000.0, "confirm": powerConfirm}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("out of range: %v", err)
	}
	if _, on := sim.Supply(); on {
		t.Fatal("the supply was switched by a refused call")
	}
	mustInvoke(t, p, "DebugProbe", "target_power", map[string]any{"enable": true, "millivolts": 3300.0, "confirm": powerConfirm})
	if mv, on := sim.Supply(); mv != 3300 || !on {
		t.Fatalf("%d %v", mv, on)
	}
	// Closing the device switches off what it switched on.
	if err := p.CloseDevice(context.Background(), SimID); err != nil {
		t.Fatal(err)
	}
	if _, on := sim.Supply(); on {
		t.Fatal("the supply stayed on after CloseDevice")
	}
}

func TestOptionValidation(t *testing.T) {
	p, _ := simPlugin(t)
	ctx := context.Background()
	for _, cfg := range []map[string]any{{"clock_hz": 10.0}, {"clock_hz": 1e9}, {"ap": 300.0}, {"timeout_ms": 1.0}, {"bogus": 1.0}, {"allow_target_power": "yes"}} {
		if err := p.OpenDevice(ctx, SimID, cfg); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%v: got %v", cfg, err)
		}
	}
	if err := p.OpenDevice(ctx, "missing", nil); status.Code(err) != codes.NotFound {
		t.Errorf("missing: %v", err)
	}
	if _, err := invoke(t, p, usbID, "DebugProbe", "info", nil); status.Code(err) != codes.Unavailable {
		t.Errorf("unopenable probe: %v", err)
	}
	if _, err := invoke(t, p, SimID, "FlashProgramming", "program", nil); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "UNSUPPORTED_BY_ATTACHED_HW") {
		t.Errorf("flash on a Cortex-M target without SACI must be UNSUPPORTED_BY_ATTACHED_HW: %v", err)
	}
	if _, err := invoke(t, p, SimID, "PowerMeasurement", "start", nil); status.Code(err) != codes.Unimplemented {
		t.Errorf("EnergyTrace must be unimplemented: %v", err)
	}
}

type goneTransport struct {
	*xds.Sim
	gone *atomic.Bool
}

func (g goneTransport) Exchange(req []byte, d time.Duration) ([]byte, error) {
	if g.gone.Load() {
		return nil, fmt.Errorf("XDS110 command: %w", usb.ErrDisconnected)
	}
	return g.Sim.Exchange(req, d)
}

func TestDeviceGoneThenReconnectBySerial(t *testing.T) {
	sim := xds.NewSim()
	var gone atomic.Bool
	opens := 0
	list := func() []Entry { return []Entry{{ID: SimID, Sim: sim}} }
	open := func(Entry) (xds.Transport, error) {
		if gone.Load() {
			return nil, fmt.Errorf("open: %w", usb.ErrDisconnected)
		}
		opens++
		return goneTransport{Sim: sim, gone: &gone}, nil
	}
	p := NewWith(list, open)
	if err := p.OpenDevice(context.Background(), SimID, nil); err != nil {
		t.Fatal(err)
	}
	mustInvoke(t, p, "DebugProbe", "halt", nil)
	gone.Store(true)
	for i := 0; i < 2; i++ {
		done := make(chan error, 1)
		go func() { _, err := invoke(t, p, SimID, "DebugProbe", "status", nil); done <- err }()
		select {
		case err := <-done:
			if !IsDeviceGone(err) {
				t.Fatalf("round %d: want DEVICE_GONE, got %v", i, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("hang after unplug")
		}
	}
	if _, err := invoke(t, p, SimID, "DebugProbe", "info", nil); !IsDeviceGone(err) {
		t.Fatalf("info while gone: %v", err)
	}
	gone.Store(false)
	if r := mustInvoke(t, p, "DebugProbe", "info", nil); r["firmware"] != "3.0.0.22" || opens < 2 {
		t.Fatalf("reopen: %v (opens %d)", r, opens)
	}
	mustInvoke(t, p, "DebugProbe", "status", nil)
}

func TestBusyIsExplicit(t *testing.T) {
	list := func() []Entry { return []Entry{{ID: SimID, Sim: xds.NewSim()}} }
	open := func(Entry) (xds.Transport, error) {
		return nil, fmt.Errorf("usbfs: %w: interface 2 is claimed by another process", usb.ErrBusy)
	}
	p := NewWith(list, open)
	if err := p.OpenDevice(context.Background(), SimID, nil); !IsBusy(err) || IsDeviceGone(err) {
		t.Fatalf("want BUSY, got %v", err)
	}
	if _, err := invoke(t, p, SimID, "DebugProbe", "info", nil); !IsBusy(err) {
		t.Fatalf("transient call: want BUSY, got %v", err)
	}
}

type releasing struct {
	*xds.Sim
	held *bool
}

func (r releasing) Close() error { *r.held = false; return nil }

func TestReopenReleasesOwnHandleFirst(t *testing.T) {
	sim := xds.NewSim()
	list := func() []Entry { return []Entry{{ID: SimID, Sim: sim}} }
	held := false
	open := func(Entry) (xds.Transport, error) {
		if held {
			return nil, fmt.Errorf("%w: held by this process", usb.ErrBusy)
		}
		held = true
		return releasing{Sim: sim, held: &held}, nil
	}
	p := NewWith(list, open)
	for i := 0; i < 3; i++ {
		if err := p.OpenDevice(context.Background(), SimID, nil); err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
	}
	if err := p.CloseDevice(context.Background(), SimID); err != nil {
		t.Fatal(err)
	}
	if held {
		t.Fatal("the interface is still claimed after CloseDevice")
	}
}

func TestUSBErrorsClassify(t *testing.T) {
	cases := []struct {
		err  error
		code codes.Code
		gone bool
		busy bool
	}{
		{usb.ErrDisconnected, codes.Unavailable, true, false},
		{fmt.Errorf("x: %w", usb.ErrBusy), codes.FailedPrecondition, false, true},
		{usb.ErrPermission, codes.PermissionDenied, false, false},
		{usb.ErrTimeout, codes.DeadlineExceeded, false, false},
		{errors.New("plain"), codes.FailedPrecondition, false, false},
	}
	for _, c := range cases {
		got := classify(c.err)
		if status.Code(got) != c.code || IsDeviceGone(got) != c.gone || IsBusy(got) != c.busy {
			t.Errorf("%v -> %v", c.err, got)
		}
	}
}
