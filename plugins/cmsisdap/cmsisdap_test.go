package cmsisdapplugin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/plugins/cmsisdap/dap"
)

func simPlugin(t *testing.T) (*Plugin, *dap.Sim) {
	t.Helper()
	sim := dap.NewSim()
	usb := &dap.UsbDevice{VID: "0d28", PID: "0204", Serial: "S1", Product: "DAPLink", Version: dap.V2, Bus: 1, Address: 5}
	list := func() []Entry { return []Entry{{ID: SimID, Sim: sim}, {ID: "0d28:0204:S1", USB: usb}} }
	open := func(e Entry) (dap.Transport, error) {
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

func TestListDevices(t *testing.T) {
	p, _ := simPlugin(t)
	ds, err := p.ListDevices(context.Background())
	if err != nil || len(ds) != 2 {
		t.Fatalf("%v %v", ds, err)
	}
	if ds[0].Properties["synthetic"] != "true" || ds[0].Transport != "fake" {
		t.Errorf("sim not marked synthetic: %+v", ds[0])
	}
	if ds[1].Properties["synthetic"] != "" || ds[1].Properties["validated"] != "false" || ds[1].Properties["transport_version"] != "v2 bulk" {
		t.Errorf("usb entry: %+v", ds[1])
	}
	var ids []string
	for _, c := range ds[1].Capabilities {
		ids = append(ids, c.Id)
	}
	if strings.Join(ids, ",") != "DebugProbe,MemoryAccess,FlashProgramming" {
		t.Errorf("only DebugProbe, MemoryAccess and CC23xx FlashProgramming may be advertised, got %v", ids)
	}
}

func TestInfoAdvertisesNoRTT(t *testing.T) {
	p, _ := simPlugin(t)
	info := p.Info()
	for _, c := range info.Capabilities {
		if c.Id == pluginv1.CapRTT {
			t.Fatalf("%s must not be advertised", c.Id)
		}
	}
	danger := map[string]pluginv1.Danger{}
	for _, m := range info.Methods {
		danger[m.Capability+"."+m.Name] = m.Danger
	}
	for _, n := range []string{"halt", "resume", "step", "reset", "read_registers", "write_register"} {
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
	if r["serial"] != "SIM0001" || r["swd"] != true || r["jtag_driven"] != "chain-scan-only" || r["synthetic"] != true || r["validated"] != false {
		t.Fatalf("info %v", r)
	}
	c := mustInvoke(t, p, "DebugProbe", "connect", nil)
	if c["core"] != "Cortex-M4" || c["dpidr"] != "0x2ba01477" || c["halted"] != false || c["revision"] != "r0p1" {
		t.Fatalf("connect %v", c)
	}
}

func TestTransientLeaseKeepsHalt(t *testing.T) {
	p, _ := simPlugin(t)
	h := mustInvoke(t, p, "DebugProbe", "halt", nil)
	if h["halted"] != true || h["pc"] == nil {
		t.Fatalf("halt %v", h)
	}
	// a separate transient call still sees the core halted
	s := mustInvoke(t, p, "DebugProbe", "status", nil)
	if s["halted"] != true {
		t.Fatalf("status %v", s)
	}
	r := mustInvoke(t, p, "DebugProbe", "read_registers", nil)
	regs := r["registers"].(map[string]any)
	if len(regs) != 20 || regs["pc"] != r["pc"] {
		t.Fatalf("registers %v", r)
	}
	mustInvoke(t, p, "DebugProbe", "resume", nil)
}

func TestReadRegistersNeedsHalt(t *testing.T) {
	p, _ := simPlugin(t)
	_, err := invoke(t, p, SimID, "DebugProbe", "read_registers", nil)
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "halt") {
		t.Fatalf("got %v", err)
	}
	r := mustInvoke(t, p, "DebugProbe", "read_registers", map[string]any{"halt": true})
	if r["halted"] != true {
		t.Fatalf("%v", r)
	}
}

func TestStepResetWriteRegister(t *testing.T) {
	p, _ := simPlugin(t)
	h := mustInvoke(t, p, "DebugProbe", "halt", nil)
	s := mustInvoke(t, p, "DebugProbe", "step", nil)
	if s["pc"] == h["pc"] {
		t.Fatalf("pc did not advance: %v -> %v", h["pc"], s["pc"])
	}
	w := mustInvoke(t, p, "DebugProbe", "write_register", map[string]any{"name": "R5", "value": "0xCAFEF00D"})
	if w["value"] != "0xcafef00d" {
		t.Fatalf("write %v", w)
	}
	if _, err := invoke(t, p, SimID, "DebugProbe", "write_register", map[string]any{"name": "nope", "value": 1}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad register: %v", err)
	}
	r := mustInvoke(t, p, "DebugProbe", "reset", map[string]any{"halt": true})
	if r["halted"] != true || r["pc"] != "0x00000100" || r["kind"] != "sysreset" {
		t.Fatalf("reset %v", r)
	}
	if _, err := invoke(t, p, SimID, "DebugProbe", "reset", map[string]any{"kind": "warp"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad kind: %v", err)
	}
}

func TestMemoryReadWrite(t *testing.T) {
	p, _ := simPlugin(t)
	r := mustInvoke(t, p, "MemoryAccess", "read", map[string]any{"address": "0x0", "length": 8})
	if r["hex"] != "0000012001010000" || r["address"] != "0x00000000" {
		t.Fatalf("read %v", r)
	}
	w := mustInvoke(t, p, "MemoryAccess", "write", map[string]any{"address": float64(0x20000010), "data": "0xDEADBEEF01"})
	if w["verified"] != true || w["length"] != float64(5) {
		t.Fatalf("write %v", w)
	}
	r = mustInvoke(t, p, "MemoryAccess", "read", map[string]any{"address": "0x20000010", "length": 5})
	if r["hex"] != "deadbeef01" {
		t.Fatalf("read back %v", r)
	}
}

func TestMemoryReadLargeIsSummarised(t *testing.T) {
	p, _ := simPlugin(t)
	r := mustInvoke(t, p, "MemoryAccess", "read", map[string]any{"address": 0, "length": 8192})
	if r["hex"] != nil || r["truncated"] != true || len(r["hex_preview"].(string)) != 2*previewBytes || len(r["sha256"].(string)) != 64 {
		t.Fatalf("large read must not inline data: %v", r)
	}
}

func TestMemoryArgumentValidation(t *testing.T) {
	p, _ := simPlugin(t)
	bad := []map[string]any{
		{"address": "zzz", "length": 4},
		{"address": 0, "length": 0},
		{"address": 0, "length": maxReadBytes + 1},
		{"address": -1, "length": 4},
		{"address": 4294967296.0, "length": 4},
		{"length": 4},
	}
	for _, a := range bad {
		if _, err := invoke(t, p, SimID, "MemoryAccess", "read", a); status.Code(err) != codes.InvalidArgument {
			t.Errorf("read %v: got %v", a, err)
		}
	}
	for _, d := range []string{"", "xyz", "abc"} {
		if _, err := invoke(t, p, SimID, "MemoryAccess", "write", map[string]any{"address": 0x20000000, "data": d}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("write %q: got %v", d, err)
		}
	}
	if _, err := invoke(t, p, SimID, "MemoryAccess", "write", map[string]any{"address": 0x20000000, "data": strings.Repeat("ab", maxWriteBytes+1)}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("oversize write: %v", err)
	}
}

func TestUnmappedAndFlashWriteFail(t *testing.T) {
	p, _ := simPlugin(t)
	if _, err := invoke(t, p, SimID, "MemoryAccess", "read", map[string]any{"address": "0x60000000", "length": 4}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unmapped read: %v", err)
	}
	if _, err := invoke(t, p, SimID, "MemoryAccess", "write", map[string]any{"address": 0x100, "data": "01020304"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("flash write: %v", err)
	}
	// still usable afterwards
	mustInvoke(t, p, "MemoryAccess", "read", map[string]any{"address": 0, "length": 4})
}

func TestNoTargetIsReported(t *testing.T) {
	p, sim := simPlugin(t)
	sim.SetNoTarget(true)
	_, err := invoke(t, p, SimID, "DebugProbe", "connect", nil)
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "wiring") {
		t.Fatalf("got %v", err)
	}
	// the probe itself still answers
	mustInvoke(t, p, "DebugProbe", "info", nil)
}

func TestOpenDeviceLifecycleAndOptions(t *testing.T) {
	p, _ := simPlugin(t)
	ctx := context.Background()
	if err := p.OpenDevice(ctx, SimID, map[string]any{"clock_hz": 400000.0}); err != nil {
		t.Fatal(err)
	}
	c := mustInvoke(t, p, "DebugProbe", "connect", nil)
	if c["clock_hz"] != 400000.0 {
		t.Fatalf("device option not used: %v", c)
	}
	c = mustInvoke(t, p, "DebugProbe", "connect", map[string]any{"clock_hz": 2000000.0})
	if c["clock_hz"] != 2000000.0 {
		t.Fatalf("per-call override: %v", c)
	}
	// open device keeps the target halted across calls
	mustInvoke(t, p, "DebugProbe", "halt", nil)
	if s := mustInvoke(t, p, "DebugProbe", "status", nil); s["halted"] != true {
		t.Fatalf("%v", s)
	}
	if err := p.CloseDevice(ctx, SimID); err != nil {
		t.Fatal(err)
	}
	if err := p.CloseDevice(ctx, SimID); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestOptionValidation(t *testing.T) {
	p, _ := simPlugin(t)
	ctx := context.Background()
	for _, cfg := range []map[string]any{{"clock_hz": 10.0}, {"clock_hz": 1e9}, {"ap": 300.0}, {"ap": 1.5}, {"timeout_ms": 1.0}, {"bogus": 1.0}} {
		if err := p.OpenDevice(ctx, SimID, cfg); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%v: got %v", cfg, err)
		}
	}
	if err := p.OpenDevice(ctx, "missing", nil); status.Code(err) != codes.NotFound {
		t.Errorf("missing: %v", err)
	}
	if _, err := invoke(t, p, "0d28:0204:S1", "DebugProbe", "info", nil); status.Code(err) != codes.Unavailable {
		t.Errorf("unopenable probe: %v", err)
	}
}

func TestUnknownMethod(t *testing.T) {
	p, _ := simPlugin(t)
	if _, err := invoke(t, p, SimID, "FlashProgramming", "program", nil); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "UNSUPPORTED_BY_ATTACHED_HW") {
		t.Fatalf("flash on a Cortex-M target without SACI must be UNSUPPORTED_BY_ATTACHED_HW: %v", err)
	}
	if _, err := invoke(t, p, SimID, "DebugProbe", "rtt", nil); status.Code(err) != codes.Unimplemented {
		t.Fatalf("got %v", err)
	}
}
