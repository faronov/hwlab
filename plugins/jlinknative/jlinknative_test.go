package jlinknativeplugin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/targetaccept"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/plugins/jlinknative/sim"
	jusb "github.com/faronov/hwlab/plugins/jlinknative/usb"
)

func simPlugin(t *testing.T) (*Plugin, *sim.Sim) {
	t.Helper()
	s := sim.New()
	usbInfo := &jusb.Info{Serial: "20000001", Interface: 2, In: 0x83, Out: 0x03, MaxPacket: 64}
	list := func() []Entry { return []Entry{{ID: SimID, Sim: s}, {ID: usbInfo.ID(), USB: usbInfo}} }
	open := func(e Entry) (jusb.Transport, error) {
		if e.Sim != nil {
			return e.Sim, nil
		}
		return nil, jusbGone()
	}
	return NewWith(list, open), s
}

func jusbGone() error { return errors.New("no such device") }

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

func openSim(t *testing.T, p *Plugin) {
	t.Helper()
	if err := p.OpenDevice(context.Background(), SimID, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.CloseDevice(context.Background(), SimID) })
}

func TestListDevicesMarksSimAndUSB(t *testing.T) {
	p, _ := simPlugin(t)
	ds, err := p.ListDevices(context.Background())
	if err != nil || len(ds) != 2 {
		t.Fatalf("%v %v", ds, err)
	}
	if ds[0].Properties["synthetic"] != "true" || ds[0].Transport != "fake" {
		t.Errorf("sim not marked synthetic: %+v", ds[0])
	}
	if ds[1].Id != "jlink-native:20000001" || ds[1].Serial != "20000001" || ds[1].Properties["validated"] != "false" {
		t.Errorf("usb entry: %+v", ds[1])
	}
	for _, d := range ds {
		var ids []string
		for _, c := range d.Capabilities {
			ids = append(ids, c.Id)
		}
		if strings.Join(ids, ",") != "DebugProbe,MemoryAccess,RTT,LiveRawStream" {
			t.Errorf("only DebugProbe, MemoryAccess, RTT and LiveRawStream may be advertised, got %v", ids)
		}
	}
}

func TestInfoAdvertisesNoFlash(t *testing.T) {
	p, _ := simPlugin(t)
	info := p.Info()
	for _, c := range info.Capabilities {
		if c.Id == pluginv1.CapFlashProgramming {
			t.Fatalf("%s must not be advertised", c.Id)
		}
	}
	danger := map[string]pluginv1.Danger{}
	for _, m := range info.Methods {
		danger[m.Capability+"."+m.Name] = m.Danger
	}
	for _, n := range []string{"halt", "resume", "step", "reset", "read_registers", "write_register", "set_breakpoint", "set_watchpoint"} {
		if danger["DebugProbe."+n] != pluginv1.Danger_DANGER_CONFIRM {
			t.Errorf("DebugProbe.%s must be CONFIRM", n)
		}
	}
	for _, n := range []string{"DebugProbe.info", "DebugProbe.connect", "DebugProbe.status", "MemoryAccess.read"} {
		if danger[n] != pluginv1.Danger_DANGER_SAFE {
			t.Errorf("%s must be SAFE", n)
		}
	}
}

func TestInfoReportsRawAndFeatures(t *testing.T) {
	p, _ := simPlugin(t)
	r := mustInvoke(t, p, "DebugProbe", "info", nil)
	if r["backend"] != "jlink-native" || r["synthetic"] != true || r["validated"] != false {
		t.Fatalf("info %v", r)
	}
	f := r["features"].(map[string]any)
	if f["rtt"] != StatusRealHW || f["jtag_transport"] != StatusUntested || f["flash_programming"] != StatusNotImplemented {
		t.Fatalf("features %v", f)
	}
}

func TestConnectAndMemory(t *testing.T) {
	p, _ := simPlugin(t)
	c := mustInvoke(t, p, "DebugProbe", "connect", nil)
	if c["core"] != "Cortex-M4" || c["dpidr"] != "0x2ba01477" {
		t.Fatalf("connect %v", c)
	}
	mustInvoke(t, p, "MemoryAccess", "write", map[string]any{"address": 0x20000100, "data": "01020304"})
	r := mustInvoke(t, p, "MemoryAccess", "read", map[string]any{"address": 0x20000100, "length": 4})
	if r["hex"] != "01020304" {
		t.Fatalf("read %v", r)
	}
}

func TestStepRegistersBreakpoint(t *testing.T) {
	p, _ := simPlugin(t)
	openSim(t, p)
	mustInvoke(t, p, "DebugProbe", "halt", nil)
	r := mustInvoke(t, p, "DebugProbe", "step", map[string]any{"count": 100.0})
	if r["steps"] != 100.0 || r["all_halted"] != true {
		t.Fatalf("step %v", r)
	}
	regs := mustInvoke(t, p, "DebugProbe", "read_registers", nil)["registers"].(map[string]any)
	if len(regs) != 20 {
		t.Fatalf("registers %v", regs)
	}
	if _, err := invoke(t, p, SimID, "DebugProbe", "step", map[string]any{"count": 0.0}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("count 0: %v", err)
	}
}

func TestOneShotBreakpointNeedsOpenDevice(t *testing.T) {
	p, _ := simPlugin(t)
	_, err := invoke(t, p, SimID, "DebugProbe", "set_breakpoint", map[string]any{"address": 0x100})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "open device") {
		t.Fatalf("got %v", err)
	}
}

func TestNoTargetIsClassified(t *testing.T) {
	p, s := simPlugin(t)
	s.SetNoTarget(true)
	_, err := invoke(t, p, SimID, "DebugProbe", "connect", nil)
	if err == nil || !strings.Contains(err.Error(), "SWD_NO_ACK") {
		t.Fatalf("got %v", err)
	}
	mustInvoke(t, p, "DebugProbe", "info", nil)
}

func TestPowerOffIsClassified(t *testing.T) {
	p, s := simPlugin(t)
	s.SetNoTarget(true)
	s.SetVTarget(0)
	_, err := invoke(t, p, SimID, "DebugProbe", "connect", nil)
	if err == nil || !strings.Contains(err.Error(), "TARGET_POWER_OFF") {
		t.Fatalf("got %v", err)
	}
}

func TestOptionValidation(t *testing.T) {
	p, _ := simPlugin(t)
	ctx := context.Background()
	for _, cfg := range []map[string]any{{"clock_hz": 10.0}, {"clock_hz": 1e9}, {"clock_hz": 1500.0}, {"ap": 300.0}, {"bogus": 1.0}} {
		if err := p.OpenDevice(ctx, SimID, cfg); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%v: got %v", cfg, err)
		}
	}
	if err := p.OpenDevice(ctx, "missing", nil); status.Code(err) != codes.NotFound {
		t.Errorf("missing: %v", err)
	}
}

func TestClockProbeReportsAndLimits(t *testing.T) {
	p, s := simPlugin(t)
	r := mustInvoke(t, p, "DebugProbe", "clock_probe", map[string]any{"rates_hz": []any{100e3, 4e6, 50e6}})
	rows := r["rates"].([]any)
	if len(rows) != 3 {
		t.Fatalf("%v", r)
	}
	if rows[2].(map[string]any)["accepted"] != false {
		t.Fatalf("50 MHz must be refused: %v", rows[2])
	}
	if s.SpeedKHz() != 1000 {
		t.Fatalf("probe left at %d kHz", s.SpeedKHz())
	}
}

func TestUnknownMethodAndFlash(t *testing.T) {
	p, _ := simPlugin(t)
	for _, c := range [][2]string{{"FlashProgramming", "program"}, {"DebugProbe", "rtt"}} {
		if _, err := invoke(t, p, SimID, c[0], c[1], nil); status.Code(err) != codes.Unimplemented {
			t.Errorf("%v: %v", c, err)
		}
	}
}

func TestAcceptanceRunOnSimulatedCortexM4(t *testing.T) {
	p, _ := simPlugin(t)
	openSim(t, p)
	rep := targetaccept.Run(func(c, m string, a map[string]any) (map[string]any, error) { return invoke(t, p, SimID, c, m, a) },
		targetaccept.Options{Backend: "jlink-native-sim", Log: t.Logf, Reset: false, ResetVectorBreakpoint: true, PerfRepeats: 3, SkipROMTable: true})
	if err := rep.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestJTAGScanOnSimulatedChain(t *testing.T) {
	p, s := simPlugin(t)
	s.SetJTAGChain(0x4BA00477, 0x0BC11477)
	r := mustInvoke(t, p, "DebugProbe", "jtag_scan", map[string]any{"max_devices": float64(4)})
	devs, _ := r["devices"].([]any)
	if len(devs) != 2 || r["status_of_feature"] != StatusUntested {
		t.Fatalf("%v", r)
	}
	// The probe goes back to SWD and the target stays usable.
	if c := mustInvoke(t, p, "DebugProbe", "connect", nil); c["dpidr"] != "0x2ba01477" {
		t.Fatalf("SWD after the scan: %v", c)
	}
	if _, err := invoke(t, p, SimID, "DebugProbe", "jtag_scan", map[string]any{"max_devices": 0.0}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad max_devices: %v", err)
	}
}
