package ccdebugger

import (
	"bytes"
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/plugins/ccdebugger/native"
)

type trackedTransport struct {
	native.Transport
	closed bool
}

func (t *trackedTransport) Close() error { t.closed = true; return t.Transport.Close() }

type opens struct {
	addrs []native.Address
	last  *trackedTransport
}

func (o *opens) open(a native.Address) (native.Transport, error) {
	o.addrs = append(o.addrs, a)
	o.last = &trackedTransport{Transport: native.NewSimulator()}
	return o.last, nil
}

func nativePlugin(t *testing.T, tool Tool) (*Plugin, *opens) {
	t.Helper()
	o := &opens{}
	return NewWithNative(sysfsWith(t, ccDebugger), tool, o.open), o
}

func openWith(t *testing.T, p *Plugin, cfg map[string]any) {
	t.Helper()
	if err := p.OpenDevice(context.Background(), "90000003", cfg); err != nil {
		t.Fatal(err)
	}
}

func call(t *testing.T, p *Plugin, capName, method string, args map[string]any) map[string]any {
	t.Helper()
	res, err := invoke(t, p, capName, method, "90000003", args)
	if err != nil {
		t.Fatalf("%s.%s: %v", capName, method, err)
	}
	return res
}

func wantCode(t *testing.T, err error, c codes.Code) {
	t.Helper()
	if status.Code(err) != c {
		t.Fatalf("code = %v (%v), want %v", status.Code(err), err, c)
	}
}

var sessionCfg = map[string]any{"access": "reset-debug-session", "allow_target_reset": true, "allow_cpu_control": true,
	"allow_memory_access": true, "allow_memory_write": true, "allow_breakpoints": true, "allow_dma_enable": true}

func TestNativeInfoAndCapabilities(t *testing.T) {
	p, _ := nativePlugin(t, &fakeTool{})
	info := p.Info()
	if len(info.DangerousConfigKeys) != 11 || !info.RuntimeRequirements[0].Optional {
		t.Fatalf("dangerous keys %v, cc-tool optional=%v", info.DangerousConfigKeys, info.RuntimeRequirements[0].Optional)
	}
	for _, m := range []string{"attach", "halt", "resume", "step", "reset_halt", "enable_dma", "set_breakpoint"} {
		found := false
		for _, s := range info.Methods {
			if s.Name == m {
				found = s.Danger == pluginv1.Danger_DANGER_CONFIRM
			}
		}
		if !found {
			t.Errorf("%s must exist and be CONFIRM", m)
		}
	}
	for _, s := range info.Methods {
		if s.Capability == pluginv1.CapMemoryAccess && s.Name == "write" && s.Danger != pluginv1.Danger_DANGER_CONFIRM {
			t.Error("memory write must be CONFIRM")
		}
	}
	devs, _ := p.ListDevices(context.Background())
	if len(devs) != 1 || !pluginv1.HasCapability(devs[0].Capabilities, pluginv1.CapMemoryAccess) ||
		pluginv1.HasCapability(devs[0].Capabilities, pluginv1.CapRTT) || devs[0].Capabilities[0].Attributes["backend"] != "native" {
		t.Fatalf("devices %v", devs)
	}
	// Without cc-tool the native backend keeps all capabilities, including flash programming.
	p2, _ := nativePlugin(t, &fakeTool{err: os.ErrNotExist})
	devs, _ = p2.ListDevices(context.Background())
	if !pluginv1.HasCapability(devs[0].Capabilities, pluginv1.CapFlashProgramming) || !pluginv1.HasCapability(devs[0].Capabilities, pluginv1.CapDebugProbe) {
		t.Fatalf("without cc-tool: %v", devs[0].Capabilities)
	}
	// The cc-tool-only plugin does not advertise native methods or keys.
	legacy := NewWith("/nonexistent", &fakeTool{}).Info()
	if len(legacy.DangerousConfigKeys) != 0 {
		t.Fatalf("legacy dangerous keys %v", legacy.DangerousConfigKeys)
	}
}

func TestNativeSmartRFBoardIsToolOnly(t *testing.T) {
	sr := map[string]string{"idVendor": "0451", "idProduct": "16a0", "product": "SmartRF05", "serial": "S5", "busnum": "2", "devnum": "9"}
	o := &opens{}
	p := NewWithNative(sysfsWith(t, sr), &fakeTool{}, o.open)
	devs, _ := p.ListDevices(context.Background())
	if pluginv1.HasCapability(devs[0].Capabilities, pluginv1.CapMemoryAccess) {
		t.Fatalf("native advertised for 0451:16a0: %v", devs[0].Capabilities)
	}
	if err := p.OpenDevice(context.Background(), "S5", nil); err != nil {
		t.Fatal(err)
	}
	_, err := invoke(t, p, pluginv1.CapDebugProbe, "status", "S5", nil)
	wantCode(t, err, codes.FailedPrecondition)
	if len(o.addrs) != 0 {
		t.Fatal("native transport opened for an unsupported adapter")
	}
}

func TestNativeConfigValidation(t *testing.T) {
	p, _ := nativePlugin(t, &fakeTool{})
	for name, cfg := range map[string]map[string]any{
		"unknown backend":   {"backend": "usb"},
		"unknown access":    {"access": "always"},
		"grant on adapter":  {"allow_memory_access": true},
		"write w/o access":  {"access": "existing-debug-session", "allow_memory_write": true},
		"reset w/o reset":   {"access": "reset-debug-session"},
		"non-bool grant":    {"access": "existing-debug-session", "allow_cpu_control": "yes"},
		"bad timeout":       {"timeout_ms": 0.0},
		"non-string target": {"target": 5.0},
	} {
		if err := p.OpenDevice(context.Background(), "90000003", cfg); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestNativeInfoNeedsNoTool(t *testing.T) {
	ft := &fakeTool{}
	p, o := nativePlugin(t, ft)
	openWith(t, p, nil)
	res := call(t, p, pluginv1.CapDebugProbe, "info", nil)
	ad := res["adapter"].(map[string]any)
	if res["backend"] != "native" || res["target"] != "CC2530" || ad["target_id"] != "0x2530" || ad["firmware_version"] != "0x5" {
		t.Fatalf("%v", res)
	}
	if len(ft.runs) != 0 || len(o.addrs) != 1 || o.addrs[0] != (native.Address{Bus: 2, Address: 4}) {
		t.Fatalf("cc-tool runs %d, opens %v", len(ft.runs), o.addrs)
	}
	sess := res["session"].(map[string]any)
	if sess["access"] != "adapter-only" || sess["attached"] != false {
		t.Fatalf("session %v", sess)
	}
	// An adapter-only session refuses every target operation without sending any.
	for _, m := range []string{"status", "read_registers", "halt"} {
		_, err := invoke(t, p, pluginv1.CapDebugProbe, m, "90000003", nil)
		wantCode(t, err, codes.PermissionDenied)
	}
}

func TestNativeBackendToolUsesCCTool(t *testing.T) {
	ft := &fakeTool{out: realInfo}
	p, o := nativePlugin(t, ft)
	openWith(t, p, map[string]any{"backend": "cc-tool"})
	res := call(t, p, pluginv1.CapDebugProbe, "info", nil)
	if res["target"] != "CC2530" || len(ft.runs) != 1 || len(o.addrs) != 0 {
		t.Fatalf("%v runs=%d opens=%d", res, len(ft.runs), len(o.addrs))
	}
	_, err := invoke(t, p, pluginv1.CapDebugProbe, "status", "90000003", nil)
	wantCode(t, err, codes.FailedPrecondition)
}

func TestNativeDebugSessionEndToEnd(t *testing.T) {
	p, o := nativePlugin(t, &fakeTool{})
	openWith(t, p, sessionCfg)
	_, err := invoke(t, p, pluginv1.CapMemoryAccess, "read", "90000003", map[string]any{"address": 0.0, "length": 4.0})
	wantCode(t, err, codes.PermissionDenied) // no attach yet
	att := call(t, p, pluginv1.CapDebugProbe, "attach", nil)
	if att["halted"] != true || att["status_after"].(map[string]any)["raw"] != float64(0x22) {
		t.Fatalf("attach %v", att)
	}
	st := call(t, p, pluginv1.CapDebugProbe, "status", nil)
	if st["debug_config"].(map[string]any)["dma_pause"] != true {
		t.Fatalf("status %v", st)
	}
	regs := call(t, p, pluginv1.CapDebugProbe, "read_registers", nil)
	if regs["pc"] != "0x0" || regs["sp"] != float64(7) || len(regs["r"].([]any)) != 8 {
		t.Fatalf("regs %v", regs)
	}
	if v := call(t, p, pluginv1.CapDebugProbe, "read_sfr", map[string]any{"address": "0x81"}); v["value"] != float64(7) {
		t.Fatalf("sfr %v", v)
	}
	_, err = invoke(t, p, pluginv1.CapDebugProbe, "read_sfr", "90000003", map[string]any{"address": 0x88})
	wantCode(t, err, codes.InvalidArgument)

	code := call(t, p, pluginv1.CapMemoryAccess, "read", map[string]any{"space": "code", "address": "0x10", "length": 8.0})
	flash := call(t, p, pluginv1.CapMemoryAccess, "read", map[string]any{"space": "flash", "address": "0x17FFC", "length": 600.0})
	b, _ := hex.DecodeString(flash["data"].(string))
	if len(b) != 600 || len(code["data"].(string)) != 16 {
		t.Fatalf("code %v flash len %d", code, len(b))
	}
	// The simulated flash is deterministic: byte(i*7 + i>>8). A read that crosses the
	// 32 KiB bank at 0x18000 must be assembled correctly from two chunks.
	for i, v := range b {
		a := 0x17FFC + i
		if want := byte(a*7 + a>>8); v != want {
			t.Fatalf("flash[%#x] = %#x, want %#x", a, v, want)
		}
	}

	wr := call(t, p, pluginv1.CapMemoryAccess, "write", map[string]any{"address": "0x200", "data": "0102030405"})
	rd := call(t, p, pluginv1.CapMemoryAccess, "read", map[string]any{"space": "xdata", "address": "0x200", "length": 5.0})
	if wr["verified"] != true || rd["data"] != "0102030405" {
		t.Fatalf("write %v read %v", wr, rd)
	}
	bp := call(t, p, pluginv1.CapDebugProbe, "set_breakpoint", map[string]any{"slot": 1.0, "address": "0x123"})
	if bp["address"] != "0x123" || bp["enabled"] != true {
		t.Fatalf("bp %v", bp)
	}

	call(t, p, pluginv1.CapDebugProbe, "enable_dma", nil)
	rs := call(t, p, pluginv1.CapDebugProbe, "resume", nil)
	if rs["status_after"].(map[string]any)["cpu_halted"] != false {
		t.Fatalf("resume %v", rs)
	}
	hl := call(t, p, pluginv1.CapDebugProbe, "halt", nil)
	if hl["command_sent"] != true {
		t.Fatalf("halt %v", hl)
	}
	if len(o.addrs) != 1 {
		t.Fatalf("one USB session expected, opened %d", len(o.addrs))
	}
	if err := p.CloseDevice(context.Background(), "90000003"); err != nil || !o.last.closed {
		t.Fatalf("close: %v closed=%v", err, o.last.closed)
	}
}

func TestNativeMemoryReadFileAndLimits(t *testing.T) {
	p, _ := nativePlugin(t, &fakeTool{})
	openWith(t, p, map[string]any{"access": "existing-debug-session", "allow_memory_access": true})
	dst := filepath.Join(t.TempDir(), "flash.bin")
	res := call(t, p, pluginv1.CapMemoryAccess, "read", map[string]any{"space": "flash", "address": 0.0, "length": 3000.0, "path": dst})
	b, err := os.ReadFile(dst)
	if err != nil || len(b) != 3000 || res["path"] != dst || res["data"] != nil {
		t.Fatalf("file read: %v len=%d %v", err, len(b), res)
	}
	for _, args := range []map[string]any{
		{"space": "flash", "address": 0.0, "length": 2000.0},                       // inline too large
		{"space": "xdata", "address": "0x1FFF", "length": 2.0},                     // past space
		{"space": "eeprom", "address": 0.0, "length": 1.0},                         // unknown space
		{"space": "flash", "address": 0.0, "length": 16.0, "path": dst},            // exists
		{"address": -1.0, "length": 1.0},                                           // negative
		{"space": "flash", "address": "0x3E7FF", "length": 2.0, "path": dst + "2"}, // past flash
	} {
		_, err := invoke(t, p, pluginv1.CapMemoryAccess, "read", "90000003", args)
		if status.Code(err) != codes.InvalidArgument && status.Code(err) != codes.AlreadyExists {
			t.Errorf("%v: %v", args, err)
		}
	}
	// Existing-session policy never permits a write without the write grant.
	_, err = invoke(t, p, pluginv1.CapMemoryAccess, "write", "90000003", map[string]any{"address": 0.0, "data": "00"})
	wantCode(t, err, codes.PermissionDenied)
	_, err = invoke(t, p, pluginv1.CapMemoryAccess, "write", "90000003", map[string]any{"space": "flash", "address": 0.0, "data": "00"})
	wantCode(t, err, codes.InvalidArgument)
	_, err = invoke(t, p, pluginv1.CapMemoryAccess, "write", "90000003", map[string]any{"address": "0x1DFF", "data": "0000"})
	wantCode(t, err, codes.InvalidArgument)
}

func TestNativeFailureFaultsSessionUntilReopened(t *testing.T) {
	p, o := nativePlugin(t, &fakeTool{})
	openWith(t, p, map[string]any{"access": "existing-debug-session", "allow_memory_access": true, "allow_cpu_control": true})
	call(t, p, pluginv1.CapDebugProbe, "resume", nil)
	// Registers can only be read from a halted CPU: a transport-class failure that faults the session.
	_, err := invoke(t, p, pluginv1.CapDebugProbe, "read_registers", "90000003", nil)
	wantCode(t, err, codes.Unavailable)
	_, err = invoke(t, p, pluginv1.CapDebugProbe, "status", "90000003", nil)
	wantCode(t, err, codes.PermissionDenied)
	if !strings.Contains(err.Error(), "faulted") || len(o.addrs) != 1 {
		t.Fatalf("faulted session must refuse without reconnecting: %v, opens=%d", err, len(o.addrs))
	}
	if err := p.CloseDevice(context.Background(), "90000003"); err != nil || !o.last.closed {
		t.Fatal("close")
	}
	openWith(t, p, map[string]any{"access": "existing-debug-session", "allow_memory_access": true})
	call(t, p, pluginv1.CapDebugProbe, "read_registers", nil)
	if len(o.addrs) != 2 {
		t.Fatalf("reopen: opens=%d", len(o.addrs))
	}
}

func TestToolOperationReleasesNativeSession(t *testing.T) {
	ft := &fakeTool{out: realMAC}
	p, o := nativePlugin(t, ft)
	openWith(t, p, map[string]any{"access": "existing-debug-session", "allow_memory_access": true})
	call(t, p, pluginv1.CapDebugProbe, "read_registers", nil)
	first := o.last
	if first.closed {
		t.Fatal("session closed too early")
	}
	call(t, p, pluginv1.CapDebugProbe, "reset", nil)
	if !first.closed {
		t.Fatal("cc-tool ran while the native session still held the USB interface")
	}
	call(t, p, pluginv1.CapDebugProbe, "read_registers", nil)
	if len(o.addrs) != 2 || o.last == first {
		t.Fatalf("native session was not reopened after the cc-tool operation: opens=%v", o.addrs)
	}
	if !strings.Contains(strings.Join(ft.runs[0].args, " "), "--reset") {
		t.Fatalf("argv %v", ft.runs[0].args)
	}
}

func TestSimulatedFlashContentIsDeterministic(t *testing.T) {
	tr := native.NewSimulator()
	d, err := native.New(tr, native.Config{Access: native.ExistingDebugSession, Timeout: 5e9,
		Permissions: native.Permissions{MemoryAccess: true}})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := d.ReadFlashCode(0x10000, 8)
	b, _ := d.ReadFlashCode(0x10000, 8)
	if !bytes.Equal(a, b) || len(a) != 8 {
		t.Fatalf("%x %x", a, b)
	}
}
