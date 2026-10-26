package telinkplugin

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/plugins/telink/sws"
	"github.com/faronov/hwlab/plugins/uart"
)

func newPlugin(t *testing.T) (*Plugin, *sws.Sim) {
	t.Helper()
	sim := sws.NewSim()
	l := func() []Entry { return []Entry{{ID: SimID, Sim: sim}} }
	return NewWith(l, defaultOpen), sim
}

// armed returns a plugin whose simulated target already answers over SWire, as
// after attach, which flash modification needs to identify the chip.
func armed(t *testing.T) (*Plugin, *sws.Sim) {
	t.Helper()
	p, sim := newPlugin(t)
	sim.Activated = true
	return p, sim
}

// withTB04 asserts the TB-04 board layout for one call.
func withTB04(args map[string]any) map[string]any {
	out := map[string]any{"layout": "tb04-512k"}
	for k, v := range args {
		out[k] = v
	}
	return out
}

func inv(t *testing.T, p *Plugin, cap, method string, args map[string]any) (map[string]any, error) {
	t.Helper()
	s, err := structpb.NewStruct(args)
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: SimID, Capability: cap, Method: method, Args: s})
	if err != nil {
		return nil, err
	}
	return r.GetResult().AsMap(), nil
}

func must(t *testing.T, p *Plugin, cap, method string, args map[string]any) map[string]any {
	t.Helper()
	r, err := inv(t, p, cap, method, args)
	if err != nil {
		t.Fatalf("%s.%s: %v", cap, method, err)
	}
	return r
}

const (
	D = pluginv1.CapDebugProbe
	M = pluginv1.CapMemoryAccess
	F = pluginv1.CapFlashProgramming
)

func fast() map[string]any { return map[string]any{"reset_ms": float64(5), "activate_ms": float64(10)} }

func halted() map[string]any {
	a := fast()
	a["halt"] = true
	return a
}

func TestParsePorts(t *testing.T) {
	es := parsePorts("a=/dev/ttyUSB0, b=/dev/ttyUSB1@921600,bad,=x,c=,d=/dev/x@12")
	if len(es) != 2 || es[0].ID != "a" || es[0].Baud != sws.DefaultBaud || es[1].Port != "/dev/ttyUSB1" || es[1].Baud != 921600 {
		t.Fatalf("%+v", es)
	}
}

func TestListDeviceCapabilitiesAndSynthetic(t *testing.T) {
	p, _ := newPlugin(t)
	ds, err := p.ListDevices(context.Background())
	if err != nil || len(ds) != 1 {
		t.Fatalf("%v %v", ds, err)
	}
	if ds[0].Properties["synthetic"] != "true" || ds[0].Properties["validated"] != "false" || len(ds[0].Capabilities) != 3 {
		t.Fatalf("%+v", ds[0])
	}
	if ds[0].Capabilities[0].Attributes["chip_erase"] != "not-offered" {
		t.Fatal("chip erase must be advertised as not offered")
	}
}

func TestInfoDoesNotTouchTarget(t *testing.T) {
	p, sim := newPlugin(t)
	r := must(t, p, D, "info", nil)
	if r["programmer_chip"] != "TLSR825x" || r["synthetic"] != true || r["validated"] != false {
		t.Fatalf("%v", r)
	}
	if sim.Activated || len(sim.FlashOps) != 0 {
		t.Fatal("info must not activate the target")
	}
}

func TestConnectReportsUnactivatedTarget(t *testing.T) {
	p, sim := newPlugin(t)
	r := must(t, p, D, "connect", nil)
	if r["target"] != "not answering over SWire" || r["flash_jedec"] != "0xef4013" || r["flash_size_code_plausible"] != true {
		t.Fatalf("%v", r)
	}
	if sim.Activated {
		t.Fatal("connect must not activate the target")
	}
}

func TestAttachHardThenDebug(t *testing.T) {
	p, sim := newPlugin(t)
	a := fast()
	a["halt"] = true
	r := must(t, p, D, "attach", a)
	if r["target_chip"] != "TLSR825x" || r["halted"] != true || r["mode"] != "hard" {
		t.Fatalf("%v", r)
	}
	if !sim.Activated {
		t.Fatal("not activated")
	}
	c := must(t, p, D, "connect", nil)
	if c["target"] != "answering" || c["target_chip_id"] != "0x5562" {
		t.Fatalf("%v", c)
	}
	st := must(t, p, D, "status", nil)
	if st["reachable"] != true || st["cpu_control"] != "0x06" || st["pc_stable"] != true {
		t.Fatalf("%v", st)
	}
	pcBefore := st["pc"].(string)
	s := must(t, p, D, "step", map[string]any{"count": float64(4)})
	if s["steps"].(float64) != 4 || s["pc"] == pcBefore {
		t.Fatalf("%v", s)
	}
	regs := must(t, p, D, "read_registers", nil)
	rm := regs["registers"].(map[string]any)
	if rm["pc"] != s["pc"] || rm["sp"] != "0x0084fff0" || regs["halted"] != true || regs["warning"] != nil {
		t.Fatalf("%v", regs)
	}
	bp := must(t, p, D, "set_breakpoint", map[string]any{"address": "0x2468", "wait_ms": float64(200)})
	if bp["hit"] != true || bp["pc"] != "0x00002468" {
		t.Fatalf("%v", bp)
	}
	must(t, p, D, "clear_breakpoint", nil)
	res := must(t, p, D, "resume", nil)
	if res["halted"] != false {
		t.Fatalf("%v", res)
	}
	if w := must(t, p, D, "read_registers", nil); w["warning"] == nil {
		t.Fatal("registers of a running CPU must carry a warning")
	}
}

// A comparator stop leaves the control byte at 0x08: only the status word
// says the CPU is halted, and stepping needs stall mode first.
func TestBreakpointStopIsHaltedAndSteppable(t *testing.T) {
	p, _ := newPlugin(t)
	must(t, p, D, "attach", fast())
	bp := must(t, p, D, "set_breakpoint", map[string]any{"address": "0x2468", "run_wait_ms": float64(200)})
	if bp["hit"] != true || bp["pc"] != "0x00002468" {
		t.Fatalf("%v", bp)
	}
	st := must(t, p, D, "status", nil)
	if st["halted"] != true || st["halt_reason"] != "breakpoint" || st["cpu_control"] != "0x08" || st["pc_stable"] != true {
		t.Fatalf("%v", st)
	}
	if r := must(t, p, D, "read_registers", nil); r["halted"] != true || r["warning"] != nil {
		t.Fatalf("%v", r)
	}
	s := must(t, p, D, "step", map[string]any{"count": float64(2)})
	if s["pc_before"] != "0x00002468" || s["pc_after"] != "0x0000246c" || s["pc_unchanged"] != nil {
		t.Fatalf("%v", s)
	}
	r := must(t, p, D, "resume", nil)
	if r["breakpoints"] == nil || r["stepped_over"] != nil {
		t.Fatalf("%v", r)
	}
	must(t, p, D, "clear_breakpoint", map[string]any{"all": true})
}

func TestResumeStepsOverBreakpointAtPC(t *testing.T) {
	p, _ := newPlugin(t)
	must(t, p, D, "attach", fast())
	must(t, p, D, "set_breakpoint", map[string]any{"address": "0x2468", "wait_ms": float64(200)})
	r := must(t, p, D, "resume", nil)
	if r["stepped_over"] != "0x002468" {
		t.Fatalf("%v", r)
	}
	w := must(t, p, D, "wait_halt", map[string]any{"wait_ms": float64(200)})
	if w["halted"] != true || w["halt_reason"] != "breakpoint" || w["timeout"] != false {
		t.Fatalf("%v", w)
	}
}

func TestThreeBreakpointSlots(t *testing.T) {
	p, _ := newPlugin(t)
	must(t, p, D, "attach", halted())
	for _, a := range []string{"0x1100", "0x1200", "0x1300"} {
		r := must(t, p, D, "set_breakpoint", map[string]any{"address": a})
		if r["note"] == nil || r["hit"] != nil {
			t.Fatalf("%v", r)
		}
	}
	if r := must(t, p, D, "set_breakpoint", map[string]any{"address": "0x1200"}); r["already_armed"] != true {
		t.Fatalf("%v", r)
	}
	if _, err := inv(t, p, D, "set_breakpoint", map[string]any{"address": "0x1400"}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("%v", err)
	}
	l := must(t, p, D, "list_breakpoints", nil)
	if fmt.Sprint(l["breakpoints"]) != "[0x001100 0x001200 0x001300]" || l["stopped"] != true || l["halted"] != true {
		t.Fatalf("%v", l)
	}
	if st := must(t, p, D, "status", nil); st["halt_reason"] != "request" {
		t.Fatalf("%v", st)
	}
	c := must(t, p, D, "clear_breakpoint", map[string]any{"address": "0x1200"})
	if fmt.Sprint(c["breakpoints"]) != "[0x001100 0x001300]" || c["halted"] != true {
		t.Fatalf("%v", c)
	}
	if _, err := inv(t, p, D, "clear_breakpoint", map[string]any{"address": "0x1200"}); status.Code(err) != codes.NotFound {
		t.Fatalf("%v", err)
	}
	must(t, p, D, "clear_breakpoint", nil)
	if l := must(t, p, D, "list_breakpoints", nil); len(l["breakpoints"].([]any)) != 0 {
		t.Fatalf("%v", l)
	}
}

func TestStepInHardwareStallReportsUnchangedPC(t *testing.T) {
	p, sim := newPlugin(t)
	must(t, p, D, "attach", halted())
	sim.StuckPC = true
	s := must(t, p, D, "step", map[string]any{"count": float64(3)})
	if s["pc_unchanged"] != true || s["hint"] == nil || s["pc_before"] != s["pc_after"] {
		t.Fatalf("%v", s)
	}
}

func TestMemoryReadBelowRegisterSpaceNeedsOptIn(t *testing.T) {
	p, _ := newPlugin(t)
	must(t, p, D, "attach", fast())
	_, err := inv(t, p, M, "read", map[string]any{"address": "0x4d570", "length": float64(4)})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "FlashProgramming.read") {
		t.Fatalf("%v", err)
	}
	r := must(t, p, M, "read", map[string]any{"address": "0x7d", "length": float64(3), "allow_registers": true})
	if r["hex"] != "036255" {
		t.Fatalf("%v", r)
	}
}

func TestStepRequiresHaltedCPU(t *testing.T) {
	p, _ := newPlugin(t)
	must(t, p, D, "attach", fast())
	_, err := inv(t, p, D, "step", nil)
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "halt it first") {
		t.Fatalf("%v", err)
	}
}

func TestHaltStopMode(t *testing.T) {
	p, sim := newPlugin(t)
	must(t, p, D, "attach", fast())
	r := must(t, p, D, "halt", map[string]any{"mode": "stop"})
	if r["cpu_control"] != "0x05" {
		t.Fatalf("%v", r)
	}
	if ctl, _, _, _ := sim.Stats(); ctl != 5 {
		t.Fatal(ctl)
	}
	if _, err := inv(t, p, D, "halt", map[string]any{"mode": "freeze"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("%v", err)
	}
}

func TestStatusUnreachableIsNotAnError(t *testing.T) {
	p, _ := newPlugin(t)
	r := must(t, p, D, "status", nil)
	if r["reachable"] != false || r["hint"] == nil {
		t.Fatalf("%v", r)
	}
}

func TestSoftAttachNeedsAnswerAndSoftReset(t *testing.T) {
	p, sim := newPlugin(t)
	a := fast()
	a["mode"] = "soft"
	if _, err := inv(t, p, D, "attach", a); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("soft attach of an unactivated target must fail: %v", err)
	}
	must(t, p, D, "attach", fast())
	must(t, p, D, "attach", a)
	if _, _, _, reboots := sim.Stats(); reboots != 1 {
		t.Fatalf("reboots %d", reboots)
	}
	r := must(t, p, D, "reset", map[string]any{"kind": "soft"})
	if r["kind"] != "soft" {
		t.Fatal(r)
	}
	if st := must(t, p, D, "status", nil); st["reachable"] != false {
		t.Fatal("SWire must be inactive after a reset")
	}
}

func TestHardResetReleasesRST(t *testing.T) {
	p, sim := newPlugin(t)
	must(t, p, D, "attach", fast())
	must(t, p, D, "reset", map[string]any{"reset_ms": float64(5)})
	if !sim.ResetLevel || sim.Activated {
		t.Fatalf("RST level %v activated %v", sim.ResetLevel, sim.Activated)
	}
}

func TestAttachFailsClearlyWithoutTarget(t *testing.T) {
	p, sim := newPlugin(t)
	sim.NoTarget = true
	_, err := inv(t, p, D, "attach", fast())
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "SWM-SWS") {
		t.Fatalf("%v", err)
	}
}

func TestMemoryReadWriteRules(t *testing.T) {
	p, _ := newPlugin(t)
	must(t, p, D, "attach", fast())
	w := must(t, p, M, "write", map[string]any{"address": "0x840100", "data": "deadbeef"})
	if w["verified"] != true {
		t.Fatalf("%v", w)
	}
	r := must(t, p, M, "read", map[string]any{"address": "0x840100", "length": float64(4)})
	if r["hex"] != "deadbeef" || r["address"] != "0x840100" {
		t.Fatalf("%v", r)
	}
	// Anything outside SRAM, notably the CPU debug block, needs an explicit flag.
	_, err := inv(t, p, M, "write", map[string]any{"address": "0x602", "data": "05"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("register write without allow_registers: %v", err)
	}
	if _, err := inv(t, p, M, "write", map[string]any{"address": "0x84fffe", "data": "01020304"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("write running past SRAM end: %v", err)
	}
	if _, err := inv(t, p, M, "write", map[string]any{"address": "0x602", "data": "05", "allow_registers": true}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []map[string]any{
		{"address": "0x1000000", "length": float64(1)},
		{"address": "0x840000", "length": float64(0)},
		{"address": "0xfffff0", "length": float64(64)},
		{"address": -1.0, "length": float64(1)},
	} {
		if _, err := inv(t, p, M, "read", bad); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("%v: %v", bad, err)
		}
	}
	big := must(t, p, M, "read", map[string]any{"address": "0x840000", "length": float64(8192)})
	if big["truncated"] != true || big["hex"] != nil || big["sha256"] == nil {
		t.Fatalf("%v", big)
	}
}

func TestFlashReadToNewFileOnly(t *testing.T) {
	p, sim := armed(t)
	path := filepath.Join(t.TempDir(), "dump.bin")
	r := must(t, p, F, "read", withTB04(map[string]any{"address": float64(0), "length": float64(0x80000), "path": path}))
	b, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(b, sim.Flash()) {
		t.Fatalf("dump differs: %v", err)
	}
	regions := r["regions"].([]any)
	if len(regions) != 6 {
		t.Fatalf("regions %v", regions)
	}
	if _, err := inv(t, p, F, "read", map[string]any{"address": float64(0), "length": float64(16), "path": path}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("an existing file must not be overwritten: %v", err)
	}
	f := must(t, p, F, "read", map[string]any{"address": "0x76000", "length": float64(4)})
	if f["hex"] != "deadbeef" {
		t.Fatalf("factory data must be readable: %v", f)
	}
}

func TestProgramRefusalsLeaveFlashAlone(t *testing.T) {
	p, sim := armed(t)
	before := append([]byte(nil), sim.Flash()...)
	overflow := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(overflow, make([]byte, 4097), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, method string
		args         map[string]any
		code         codes.Code
	}{
		{"erase factory eui", "erase", map[string]any{"address": "0x76000", "length": float64(16), "allow_journals": true}, codes.PermissionDenied},
		{"erase factory cfg", "erase", map[string]any{"address": "0x77000", "length": float64(4096), "allow_journals": true}, codes.PermissionDenied},
		{"program over factory", "program", map[string]any{"address": "0x76000", "data": "01", "allow_journals": true}, codes.PermissionDenied},
		{"program reaching journals", "program", map[string]any{"address": "0x6f000", "path": overflow}, codes.PermissionDenied},
		{"erase journal without opt in", "erase", map[string]any{"address": "0x70000", "length": float64(1)}, codes.PermissionDenied},
		{"unaligned program", "program", map[string]any{"address": "0x1010", "data": "01"}, codes.InvalidArgument},
		{"program no image", "program", map[string]any{"address": "0x1000"}, codes.InvalidArgument},
		{"path and data", "program", map[string]any{"address": "0x1000", "data": "01", "path": "x"}, codes.InvalidArgument},
		{"erase past flash", "erase", map[string]any{"address": "0x7f000", "length": float64(0x2000), "allow_journals": true}, codes.PermissionDenied},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := inv(t, p, F, c.method, withTB04(c.args))
			if status.Code(err) != c.code {
				t.Fatalf("code %v want %v (%v)", status.Code(err), c.code, err)
			}
		})
	}
	if len(sim.FlashOps) != 0 || !bytes.Equal(before, sim.Flash()) {
		t.Fatalf("flash was touched: %+v", sim.FlashOps)
	}
}

func TestProgramVerifyEraseHappyPath(t *testing.T) {
	p, sim := armed(t)
	sim.ProtectFlash()
	img := make([]byte, 5000)
	for i := range img {
		img[i] = byte(i*3 + 5)
	}
	path := filepath.Join(t.TempDir(), "app.bin")
	if err := os.WriteFile(path, img, 0o644); err != nil {
		t.Fatal(err)
	}
	bk := haltAndBackup(t, p)
	r := must(t, p, F, "program", withTB04(map[string]any{"address": "0x8000", "path": path, "backup": bk}))
	if r["flash_protection_cleared"] != true || r["sectors_erased"].(float64) != 2 || r["verified_bytes"].(float64) != 5000 || r["flash_kib"].(float64) != 512 {
		t.Fatalf("%v", r)
	}
	if v := must(t, p, F, "verify", map[string]any{"address": "0x8000", "path": path}); v["match"] != true {
		t.Fatalf("%v", v)
	}
	sim.Flash()[0x8000+100] ^= 0x10
	v := must(t, p, F, "verify", map[string]any{"address": "0x8000", "path": path})
	if v["match"] != false || v["first_difference"] != "0x008064" {
		t.Fatalf("%v", v)
	}
	e := must(t, p, F, "erase", withTB04(map[string]any{"address": "0x8000", "length": float64(5000), "backup": bk}))
	if e["sectors_erased"].(float64) != 2 || sim.Flash()[0x8000] != 0xff {
		t.Fatalf("%v", e)
	}
	if !bytes.Equal(sim.Flash()[0x76000:0x76004], []byte{0xde, 0xad, 0xbe, 0xef}) {
		t.Fatal("factory EUI changed")
	}
}

func TestProgramHexData(t *testing.T) {
	p, sim := armed(t)
	data := []byte{1, 2, 3, 4, 5}
	bk := haltAndBackup(t, p)
	must(t, p, F, "program", withTB04(map[string]any{"address": "0x1000", "data": hex.EncodeToString(data), "backup": bk}))
	if !bytes.Equal(sim.Flash()[0x1000:0x1005], data) {
		t.Fatal("not programmed")
	}
}

func TestOptionsValidation(t *testing.T) {
	p, _ := newPlugin(t)
	for _, bad := range []map[string]any{
		{"swdiv": 0.0}, {"baud": 12.0}, {"timeout_ms": 1.0}, {"layout": "other"},
	} {
		if _, err := inv(t, p, D, "info", bad); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("%v: %v", bad, err)
		}
	}
	if err := p.OpenDevice(context.Background(), SimID, map[string]any{"bogus": 1.0}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unknown open setting: %v", err)
	}
	if err := p.OpenDevice(context.Background(), SimID, map[string]any{"swdiv": 4.0}); err != nil {
		t.Fatal(err)
	}
	r := must(t, p, D, "info", nil)
	if r["swdiv"].(float64) != 4 {
		t.Fatalf("swdiv option not applied: %v", r)
	}
	if _, err := inv(t, p, D, "info", map[string]any{"baud": 460800.0}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("per-call device options on an open device: %v", err)
	}
	if err := p.CloseDevice(context.Background(), SimID); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownDeviceAndMethod(t *testing.T) {
	p, _ := newPlugin(t)
	_, err := p.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: "nope", Capability: D, Method: "info"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("%v", err)
	}
	if _, err := inv(t, p, F, "chip_erase", nil); status.Code(err) != codes.Unimplemented {
		t.Fatalf("chip erase must not exist: %v", err)
	}
}

func TestNoProgrammerBehindPort(t *testing.T) {
	sim := sws.NewSim()
	sim.DropNext = 100
	p := NewWith(func() []Entry { return []Entry{{ID: SimID, Sim: sim}} }, defaultOpen)
	_, err := inv(t, p, D, "info", map[string]any{"timeout_ms": 20.0})
	if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "SWire programmer") {
		t.Fatalf("%v", err)
	}
}

func TestMethodSpecsDangerLevels(t *testing.T) {
	p, _ := newPlugin(t)
	danger := map[string]pluginv1.Danger{}
	for _, m := range p.Info().Methods {
		danger[m.Capability+"."+m.Name] = m.Danger
	}
	safe := []string{"DebugProbe.info", "DebugProbe.connect", "DebugProbe.status", "DebugProbe.read_registers", "DebugProbe.chip_info", "DebugProbe.list_breakpoints", "MemoryAccess.read",
		"FlashProgramming.read", "FlashProgramming.verify", "FlashProgramming.info"}
	confirm := []string{"DebugProbe.attach", "DebugProbe.halt", "DebugProbe.resume", "DebugProbe.step", "DebugProbe.reset", "DebugProbe.set_breakpoint",
		"DebugProbe.clear_breakpoint", "DebugProbe.wait_halt", "MemoryAccess.write", "FlashProgramming.erase", "FlashProgramming.program", "FlashProgramming.backup", "FlashProgramming.restore"}
	for _, n := range safe {
		if danger[n] != pluginv1.Danger_DANGER_SAFE {
			t.Errorf("%s should be SAFE", n)
		}
	}
	for _, n := range confirm {
		if danger[n] != pluginv1.Danger_DANGER_CONFIRM {
			t.Errorf("%s should be CONFIRM", n)
		}
	}
	if len(danger) != len(safe)+len(confirm) {
		t.Errorf("%d methods, %d classified", len(danger), len(safe)+len(confirm))
	}
}

func TestModifyFailsClosedWithoutVerifiedLayout(t *testing.T) {
	data := map[string]any{"address": "0x1000", "data": "01", "length": float64(16)}
	t.Run("default layout is unverified", func(t *testing.T) {
		p, sim := armed(t)
		before := append([]byte(nil), sim.Flash()...)
		for _, m := range []string{"program", "erase"} {
			_, err := inv(t, p, F, m, data)
			if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "allowed_on_current_target: false") {
				t.Fatalf("%s: %v", m, err)
			}
		}
		if len(sim.FlashOps) != 0 || !bytes.Equal(before, sim.Flash()) {
			t.Fatal("flash touched")
		}
	})
	t.Run("blank factory block does not confirm tb04", func(t *testing.T) {
		p, sim := armed(t)
		for i := 0x76000; i < 0x77000; i++ {
			sim.Flash()[i] = 0xff
		}
		_, err := inv(t, p, F, "program", withTB04(data))
		if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "reads blank") || len(sim.FlashOps) != 0 {
			t.Fatalf("%v %+v", err, sim.FlashOps)
		}
	})
	t.Run("target not answering", func(t *testing.T) {
		p, sim := newPlugin(t)
		_, err := inv(t, p, F, "program", withTB04(data))
		if status.Code(err) != codes.FailedPrecondition || len(sim.FlashOps) != 0 {
			t.Fatalf("%v", err)
		}
	})
	t.Run("capabilities say why", func(t *testing.T) {
		p, _ := armed(t)
		r := must(t, p, D, "connect", nil)
		caps := r["capabilities"].(map[string]any)
		fp := caps["flash_program"].(map[string]any)
		if fp["implemented"] != true || fp["allowed_on_current_target"] != false || fp["reason"] == "" {
			t.Fatalf("%v", caps)
		}
		if r["target_family"] != "TLSR825x" || r["target_variant"] != "undetermined" {
			t.Fatalf("%v", r)
		}
		r = must(t, p, D, "connect", map[string]any{"layout": "tb04-512k"})
		if r["capabilities"].(map[string]any)["flash_program"].(map[string]any)["allowed_on_current_target"] != true {
			t.Fatalf("%v", r["capabilities"])
		}
	})
}

func TestFlashReadWithoutVerifiedLayoutListsNoRegions(t *testing.T) {
	p, _ := armed(t)
	r := must(t, p, F, "read", map[string]any{"address": "0x76000", "length": float64(4)})
	if r["hex"] != "deadbeef" {
		t.Fatalf("%v", r)
	}
	if rs := r["regions"].([]any); len(rs) != 1 || !strings.HasPrefix(rs[0].(string), "unverified") {
		t.Fatalf("%v", rs)
	}
}

func TestCandidatesOnlyEnumerate(t *testing.T) {
	enum := func() ([]uart.Info, error) {
		return []uart.Info{
			{Path: "/dev/ttyUSB0", VID: "1A86", PID: "7523", USB: true, Product: "USB Serial"},
			{Path: "/dev/ttyUSB1", VID: "1a86", PID: "7523", USB: true},
			{Path: "/dev/ttyACM0", VID: "1366", PID: "0105", USB: true},
			{Path: "/dev/ttyS0"},
		}, nil
	}
	got := candidates([]Entry{{ID: "pgm", Port: "/dev/ttyUSB1"}}, enum)
	if len(got) != 1 || got[0].ID != "ttyUSB0" || !got[0].Candidate || got[0].USB != "1a86:7523" {
		t.Fatalf("%+v", got)
	}
	if got := candidates(nil, func() ([]uart.Info, error) { return nil, errors.New("no udev") }); got != nil {
		t.Fatalf("%+v", got)
	}
}

func TestCandidateDeviceIsMarkedUnprobed(t *testing.T) {
	l := func() []Entry {
		return []Entry{{ID: "ttyUSB0", Port: "/dev/ttyUSB0", Baud: sws.DefaultBaud, Candidate: true, USB: "1a86:7523"}}
	}
	p := NewWith(l, func(Entry, int) (sws.Link, func() error, error) { t.Fatal("must not open"); return nil, nil, nil })
	ds, err := p.ListDevices(context.Background())
	if err != nil || len(ds) != 1 || ds[0].Properties["candidate"] != "true" || ds[0].Properties["probed"] != "false" {
		t.Fatalf("%v %+v", err, ds)
	}
}

func TestInvokeHonoursCancellation(t *testing.T) {
	p, _ := armed(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s, _ := structpb.NewStruct(map[string]any{"address": "0x0", "length": float64(16)})
	_, err := p.Invoke(ctx, &pluginv1.InvokeRequest{DeviceId: SimID, Capability: F, Method: "read", Args: s})
	if status.Code(err) != codes.Canceled {
		t.Fatalf("%v", err)
	}
}

func TestSimulatorOnlyWhenRequested(t *testing.T) {
	t.Setenv(SimEnv, "")
	t.Setenv(PortsEnv, "")
	p := New()
	ds, _ := p.ListDevices(context.Background())
	for _, d := range ds {
		if d.Id == SimID || d.Properties["synthetic"] == "true" {
			t.Fatalf("the simulator must not appear in the production path: %+v", d)
		}
	}
}
