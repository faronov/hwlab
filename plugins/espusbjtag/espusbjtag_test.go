package espusbjtag

import (
	"context"
	"encoding/hex"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/riscv/rvsim"
	"github.com/faronov/hwlab/internal/usb"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/plugins/espusbjtag/sim"
)

// realLayout mirrors the descriptor layout observed on the ESP32-C6.
func realLayout() usb.Device {
	d := SimDevice()
	d.Serial, d.SysName, d.Bus, d.Address = "60:55:F9:F6:96:88", "2-1", 2, 44
	d.Interfaces[0].TTY, d.Interfaces[0].Driver = "ttyACM1", "cdc_acm"
	return d
}

type rig struct {
	*Plugin
	mu      sync.Mutex
	target  *rvsim.Target
	devs    []*sim.Device
	opens   []int
	opts    []usb.OpenOptions
	openErr error
}

func newRig(t *testing.T) *rig {
	t.Helper()
	return newRigCfg(t, SimConfig())
}

func newRigCfg(t *testing.T, cfg sim.Config) *rig {
	t.Helper()
	r := &rig{}
	r.target = rvsim.New(cfg.Target)
	cfg.Shared = r.target
	list := func() []Entry { return nameEntries([]Entry{{Dev: realLayout(), Sim: &cfg}}) }
	open := func(e Entry, iface int, opt usb.OpenOptions) (usb.Port, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.openErr != nil {
			return nil, r.openErr
		}
		d := sim.New(*e.Sim)
		r.devs = append(r.devs, d)
		r.opens = append(r.opens, iface)
		r.opts = append(r.opts, opt)
		return d, nil
	}
	r.Plugin = NewWith(list, open)
	return r
}

const wantID = "303a:1001:60_55_F9_F6_96_88"

func (r *rig) call(capName, method string, args map[string]any) (map[string]any, error) {
	s, _ := structpb.NewStruct(args)
	res, err := r.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: wantID, Capability: capName, Method: method, Args: s})
	if err != nil {
		return nil, err
	}
	return res.Result.AsMap(), nil
}

func (r *rig) must(t *testing.T, capName, method string, args map[string]any) map[string]any {
	t.Helper()
	m, err := r.call(capName, method, args)
	if err != nil {
		t.Fatalf("%s.%s: %v", capName, method, err)
	}
	return m
}

func code(err error) codes.Code { return status.Code(err) }

func TestListDevices(t *testing.T) {
	r := newRig(t)
	ds, err := r.ListDevices(context.Background())
	if err != nil || len(ds) != 1 {
		t.Fatalf("%v %v", ds, err)
	}
	d := ds[0]
	if d.Id != wantID || d.Serial != "60:55:F9:F6:96:88" {
		t.Fatalf("%s %s", d.Id, d.Serial)
	}
	p := d.Properties
	if p["vid"] != "303a" || p["pid"] != "1001" || p["jtag_interface"] != "2" || p["companion_cdc_tty"] != "/dev/ttyACM1" ||
		p["companion_cdc_driver"] != "cdc_acm" || p["interfaces"] != "3" || !strings.Contains(p["jtag_endpoints"], "OUT 0x02, IN 0x83") {
		t.Fatalf("%v", p)
	}
	if len(r.opens) != 0 {
		t.Fatal("listing must not claim anything")
	}
	have := map[string]bool{}
	for _, c := range d.Capabilities {
		have[c.Id] = true
	}
	for _, want := range []string{"JTAGTransport", "JTAGScan", "TargetIdentify", "DebugProbe", "MemoryAccess"} {
		if !have[want] {
			t.Errorf("missing %s", want)
		}
	}
	if !have["FlashProgramming"] || have["UART"] {
		t.Fatalf("overclaiming: %v", have)
	}
}

func TestDeclaredMethodsAreHonest(t *testing.T) {
	r := newRig(t)
	got := map[string]pluginv1.Danger{}
	for _, m := range r.Info().Methods {
		got[m.Capability+"."+m.Name] = m.Danger
	}
	for _, banned := range []string{"DebugProbe.attach", "FlashProgramming.write", "FlashProgramming.chip_erase"} {
		if _, ok := got[banned]; ok {
			t.Errorf("%s must not be declared", banned)
		}
		if _, err := r.call(strings.Split(banned, ".")[0], strings.Split(banned, ".")[1], nil); code(err) != codes.Unimplemented {
			t.Errorf("%s: %v", banned, err)
		}
	}
	for _, m := range []string{"DebugProbe.halt", "DebugProbe.resume", "DebugProbe.read_registers", "JTAGTransport.shift_ir", "JTAGTransport.shift_dr",
		"DebugProbe.step", "DebugProbe.reset", "DebugProbe.set_breakpoint", "MemoryAccess.write", "FlashProgramming.program", "FlashProgramming.erase", "FlashProgramming.restore", "FlashProgramming.backup"} {
		if got[m] != pluginv1.Danger_DANGER_CONFIRM {
			t.Errorf("%s danger %v", m, got[m])
		}
	}
	for _, m := range []string{"DebugProbe.chip_info", "MemoryAccess.read", "JTAGScan.scan", "TargetIdentify.identify", "JTAGTransport.tap_reset"} {
		if got[m] != pluginv1.Danger_DANGER_SAFE {
			t.Errorf("%s danger %v", m, got[m])
		}
	}
}

func TestInfoClaimsNothing(t *testing.T) {
	r := newRig(t)
	m := r.must(t, "DebugProbe", "info", nil)
	if m["open"] != false || m["jtag_interface"] != float64(2) || len(r.opens) != 0 {
		t.Fatalf("%v opens %v", m, r.opens)
	}
	if m["reset"] == nil || m["flash"] == nil || !strings.Contains(m["flash"].(string), "needs allow_flash") {
		t.Fatalf("%v", m)
	}
}

func TestOpenClaimsOnlyJTAGInterface(t *testing.T) {
	r := newRig(t)
	if err := r.OpenDevice(context.Background(), wantID, nil); err != nil {
		t.Fatal(err)
	}
	if len(r.opens) != 1 || r.opens[0] != 2 {
		t.Fatalf("opened interfaces %v", r.opens)
	}
	if r.opts[0].DetachDriver != "" {
		t.Fatalf("a kernel driver was to be detached: %q", r.opts[0].DetachDriver)
	}
	m := r.must(t, "JTAGTransport", "info", nil)
	tr := m["transport"].(map[string]any)
	if m["open"] != true || tr["tck_khz"] != float64(4000) || tr["divider"] != float64(6) {
		t.Fatalf("%v", m)
	}
	if err := r.CloseDevice(context.Background(), wantID); err != nil || !r.devs[0].Closed() {
		t.Fatalf("close: %v", err)
	}
}

func TestCDCInterfacesRefused(t *testing.T) {
	r := newRig(t)
	for _, i := range []float64{0, 1} {
		err := r.OpenDevice(context.Background(), wantID, map[string]any{"interface": i})
		if code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "uart plugin") {
			t.Errorf("interface %v: %v", i, err)
		}
	}
	if code(r.OpenDevice(context.Background(), wantID, map[string]any{"interface": float64(9)})) != codes.InvalidArgument {
		t.Fatal("missing interface accepted")
	}
	if len(r.opens) != 0 {
		t.Fatalf("claimed %v", r.opens)
	}
}

func TestOpenErrors(t *testing.T) {
	r := newRig(t)
	for _, tc := range []struct {
		err  error
		want codes.Code
	}{{usb.ErrBusy, codes.FailedPrecondition}, {usb.ErrPermission, codes.PermissionDenied}, {usb.ErrDisconnected, codes.Unavailable}} {
		r.openErr = tc.err
		if got := code(r.OpenDevice(context.Background(), wantID, nil)); got != tc.want {
			t.Errorf("%v -> %v", tc.err, got)
		}
	}
	r.openErr = nil
	if code(r.OpenDevice(context.Background(), "303a:1001:nope", nil)) != codes.NotFound {
		t.Fatal("unknown id")
	}
	for _, cfg := range []map[string]any{{"bogus": 1.0}, {"clock_khz": 0.0}, {"clock_khz": "fast"}, {"timeout_ms": 5.0}, {"hart": 99.0}, {"interface": 1.5}} {
		if code(r.OpenDevice(context.Background(), wantID, cfg)) != codes.InvalidArgument {
			t.Errorf("%v accepted", cfg)
		}
	}
}

func TestReopenReleasesOld(t *testing.T) {
	r := newRig(t)
	for i := 0; i < 3; i++ {
		if err := r.OpenDevice(context.Background(), wantID, nil); err != nil {
			t.Fatal(err)
		}
	}
	for i, d := range r.devs[:2] {
		if !d.Closed() {
			t.Fatalf("session %d leaked", i)
		}
	}
	if r.devs[2].Closed() {
		t.Fatal("the current session was closed")
	}
}

func TestSettingsOnOpenDeviceRefused(t *testing.T) {
	r := newRig(t)
	_ = r.OpenDevice(context.Background(), wantID, nil)
	if _, err := r.call("JTAGScan", "scan", map[string]any{"clock_khz": 500.0}); code(err) != codes.InvalidArgument {
		t.Fatalf("%v", err)
	}
}

func TestScanAndIdentify(t *testing.T) {
	r := newRig(t)
	m := r.must(t, "JTAGScan", "scan", nil)
	devs := m["devices"].([]any)
	if m["status"] != "ok" || len(devs) != 1 || m["count"] != float64(1) || m["total_ir_length"] != float64(5) {
		t.Fatalf("%v", m)
	}
	if m["synthetic"] != true || m["target_verified"] != false {
		t.Fatalf("a simulated answer must say so: %v", m)
	}
	m = r.must(t, "TargetIdentify", "identify", nil)
	tg := m["targets"].([]any)[0].(map[string]any)
	if tg["idcode"] != "0x0000dc25" || tg["ir_length"] != float64(5) || tg["part"] != "0x000d" {
		t.Fatalf("%v", tg)
	}
	hint := m["chip_hint"].(map[string]any)
	if hint["confidence"] != "idcode-only" || !strings.Contains(hint["basis"].(string), "not proof") {
		t.Fatalf("%v", hint)
	}
	if len(r.opens) != 2 || r.opens[0] != 2 || r.opens[1] != 2 {
		t.Fatalf("opens %v", r.opens)
	}
	for _, d := range r.devs {
		if !d.Closed() {
			t.Fatal("ephemeral lease leaked its port")
		}
	}
}

func TestChipInfo(t *testing.T) {
	r := newRig(t)
	m := r.must(t, "DebugProbe", "chip_info", nil)
	sba := m["sba"].(map[string]any)
	if m["debug_spec"] != "0.13" || m["harts"] != float64(2) || m["progbuf_words"] != float64(16) || m["hart_state"] != "running" ||
		sba["access32"] != true || m["idcode"] != "0x0000dc25" {
		t.Fatalf("%v", m)
	}
	if r.target.Halts != 0 {
		t.Fatal("chip_info halted the hart")
	}
	// The ephemeral session activated the DM and the hart ran: released again.
	if r.target.Active() {
		t.Fatal("debug module left active after a read-only call")
	}
}

func TestHaltResumeAcrossSessions(t *testing.T) {
	r := newRig(t)
	m := r.must(t, "DebugProbe", "halt", nil)
	if m["halted"] != true || m["state"] != "halted" || !strings.Contains(m["note"].(string), "resume") {
		t.Fatalf("%v", m)
	}
	// A later one-shot call sees the halted hart and the DM still active.
	if !r.target.Halted(0) || !r.target.Active() {
		t.Fatal("halt did not persist")
	}
	m = r.must(t, "DebugProbe", "resume", nil)
	if m["resumed"] != true || m["state"] != "running" || r.target.Halted(0) {
		t.Fatalf("%v", m)
	}
	m = r.must(t, "DebugProbe", "resume", nil)
	if m["resumed"] != false {
		t.Fatalf("resume of a running hart: %v", m)
	}
}

func TestRegisters(t *testing.T) {
	r := newRig(t)
	r.target.SetGPR(10, 0xCAFEF00D)
	m := r.must(t, "DebugProbe", "read_registers", nil)
	regs := m["registers"].(map[string]any)
	if regs["a0"] != "0xcafef00d" || regs["x10"] != "0xcafef00d" || regs["mvendorid"] != "0x00000612" || regs["marchid"] != "0x80000002" || regs["misa"] != "0x40903105" {
		t.Fatalf("%v", regs)
	}
	if m["was_running"] != true || m["halted"] != false || m["resumed"] != true || r.target.Halted(0) {
		t.Fatalf("a running hart must be resumed: %v", m)
	}
	if r.target.Halts != 1 || r.target.Resumes != 1 {
		t.Fatalf("halts %d resumes %d", r.target.Halts, r.target.Resumes)
	}
	m = r.must(t, "DebugProbe", "read_registers", map[string]any{"keep_halted": true})
	if m["halted"] != true || !r.target.Halted(0) {
		t.Fatalf("%v", m)
	}
	// Already halted: no extra halt/resume, state unchanged.
	h, rs := r.target.Halts, r.target.Resumes
	m = r.must(t, "DebugProbe", "read_registers", nil)
	if m["was_running"] != false || m["halted"] != true || r.target.Halts != h || r.target.Resumes != rs || !r.target.Halted(0) {
		t.Fatalf("%v", m)
	}
	if _, err := r.call("DebugProbe", "read_registers", map[string]any{"keep_halted": "yes"}); code(err) != codes.InvalidArgument {
		t.Fatalf("%v", err)
	}
}

func TestReadMemory(t *testing.T) {
	r := newRig(t)
	m := r.must(t, "MemoryAccess", "read", map[string]any{"address": "0x40800004", "length": 8.0})
	if m["hex"] != "0405060708090a0b" || m["address"] != "0x40800004" || m["length"] != float64(8) {
		t.Fatalf("%v", m)
	}
	m = r.must(t, "MemoryAccess", "read", map[string]any{"address": float64(0x42000001), "length": 3.0})
	if m["hex"] != "010203" {
		t.Fatalf("unaligned: %v", m)
	}
	if r.target.Halts != 0 {
		t.Fatal("a system-bus read halted the hart")
	}
}

func TestReadMemoryArgs(t *testing.T) {
	r := newRig(t)
	bad := []map[string]any{
		{"length": 4.0}, {"address": "0x40800000"}, {"address": "0x40800000", "length": 0.0}, {"address": "0x40800000", "length": 4097.0},
		{"address": "0x40800000", "length": 1.5}, {"address": "zzz", "length": 4.0}, {"address": "0x1FFFFFFFF", "length": 4.0},
		{"address": "0xFFFFFFFC", "length": 8.0}, {"address": -4.0, "length": 4.0}, {"address": true, "length": 4.0},
		{"address": "0x40800000", "length": 4.0, "allow_peripheral": "yes"},
	}
	for _, a := range bad {
		if _, err := r.call("MemoryAccess", "read", a); code(err) != codes.InvalidArgument {
			t.Errorf("%v: %v", a, err)
		}
	}
	// Peripheral window guard, including ranges that only touch it.
	for _, a := range []map[string]any{
		{"address": "0x60000000", "length": 4.0}, {"address": "0x600B0000", "length": 16.0}, {"address": "0x5FFFFFFE", "length": 4.0}, {"address": "0x6FFFFFFC", "length": 4.0},
	} {
		_, err := r.call("MemoryAccess", "read", a)
		if code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "allow_peripheral") {
			t.Errorf("%v: %v", a, err)
		}
	}
	if len(r.opens) != 0 {
		t.Fatalf("refused reads must not even open the device: %v", r.opens)
	}
	// Opting in reaches the bus (the simulator has no such region: bus error).
	if _, err := r.call("MemoryAccess", "read", map[string]any{"address": "0x60000000", "length": 4.0, "allow_peripheral": true}); err == nil || code(err) == codes.InvalidArgument {
		t.Fatalf("%v", err)
	}
	// The window ends at 0x6FFFFFFF.
	if _, err := r.call("MemoryAccess", "read", map[string]any{"address": "0x70000000", "length": 4.0}); code(err) == codes.InvalidArgument {
		t.Fatalf("0x70000000 is outside the window: %v", err)
	}
}

func TestBusErrorIsReportedAndRecovered(t *testing.T) {
	r := newRig(t)
	_ = r.OpenDevice(context.Background(), wantID, nil)
	if _, err := r.call("MemoryAccess", "read", map[string]any{"address": "0x50000000", "length": 4.0}); code(err) != codes.FailedPrecondition {
		t.Fatalf("%v", err)
	}
	m := r.must(t, "MemoryAccess", "read", map[string]any{"address": "0x40800000", "length": 4.0})
	if m["hex"] != "00010203" {
		t.Fatalf("%v", m)
	}
}

func TestRawShift(t *testing.T) {
	r := newRig(t)
	_ = r.OpenDevice(context.Background(), wantID, nil)
	_ = r.must(t, "DebugProbe", "chip_info", nil)
	m := r.must(t, "JTAGTransport", "shift_ir", map[string]any{"bits": 5.0, "data": "01"})
	if m["register"] != "IR" {
		t.Fatalf("%v", m)
	}
	m = r.must(t, "JTAGTransport", "shift_dr", map[string]any{"bits": 32.0, "capture": true})
	if m["out"] != "25dc0000" {
		t.Fatalf("IDCODE through raw DR: %v", m)
	}
	// The DTM must have reselected its instructions afterwards.
	m = r.must(t, "MemoryAccess", "read", map[string]any{"address": "0x40800000", "length": 4.0})
	if m["hex"] != "00010203" {
		t.Fatalf("%v", m)
	}
	for _, a := range []map[string]any{{}, {"bits": 0.0}, {"bits": 9000.0}, {"bits": 8.0, "data": "zz"}, {"bits": 16.0, "data": "01"}, {"bits": 8.0, "capture": "x"}} {
		if _, err := r.call("JTAGTransport", "shift_dr", a); code(err) != codes.InvalidArgument {
			t.Errorf("%v: %v", a, err)
		}
	}
	if _, err := hex.DecodeString(m["hex"].(string)); err != nil {
		t.Fatal(err)
	}
}

func TestTapReset(t *testing.T) {
	r := newRig(t)
	m := r.must(t, "JTAGTransport", "tap_reset", nil)
	if m["state"] != "Test-Logic-Reset" || m["ok"] != true {
		t.Fatalf("%v", m)
	}
}

func TestDisconnectDropsSession(t *testing.T) {
	r := newRig(t)
	_ = r.OpenDevice(context.Background(), wantID, nil)
	r.devs[0].Disconnect()
	_, err := r.call("JTAGScan", "scan", nil)
	if code(err) != codes.Unavailable || !strings.Contains(err.Error(), "unplugged") {
		t.Fatalf("%v", err)
	}
	if !r.devs[0].Closed() {
		t.Fatal("dead session kept its interface")
	}
	// The next call reopens cleanly.
	m := r.must(t, "JTAGScan", "scan", nil)
	if m["status"] != "ok" || len(r.devs) != 2 {
		t.Fatalf("%v", m)
	}
}

func TestRepeatedOpenClose(t *testing.T) {
	r := newRig(t)
	for i := 0; i < 20; i++ {
		if err := r.OpenDevice(context.Background(), wantID, nil); err != nil {
			t.Fatal(err)
		}
		if m := r.must(t, "TargetIdentify", "identify", nil); m["count"] != float64(1) {
			t.Fatalf("round %d: %v", i, m)
		}
		if err := r.CloseDevice(context.Background(), wantID); err != nil {
			t.Fatal(err)
		}
	}
	for i, d := range r.devs {
		if !d.Closed() {
			t.Fatalf("port %d leaked", i)
		}
	}
}

func TestNameEntries(t *testing.T) {
	a, b := realLayout(), realLayout()
	b.SysName = "2-2"
	es := nameEntries([]Entry{{Dev: a}, {Dev: b}})
	if es[0].ID != "303a:1001@2-1" || es[1].ID != "303a:1001@2-2" {
		t.Fatalf("duplicate serials must fall back to the port: %v %v", es[0].ID, es[1].ID)
	}
	a.Serial = ""
	if es = nameEntries([]Entry{{Dev: a}}); es[0].ID != "303a:1001@2-1" {
		t.Fatal(es[0].ID)
	}
}

func TestSyntheticSimEnv(t *testing.T) {
	t.Setenv(SimEnv, "1")
	p := New()
	var sawSim bool
	ds, _ := p.ListDevices(context.Background())
	for _, d := range ds {
		if d.Id == SimID {
			sawSim = d.Properties["synthetic"] == "true" && strings.Contains(d.Name, "SYNTHETIC")
		}
	}
	if !sawSim {
		t.Fatalf("%v", ds)
	}
}
