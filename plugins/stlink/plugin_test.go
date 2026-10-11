package stlinkplugin

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/cortexm"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/plugins/stlink/sim"
	"github.com/faronov/hwlab/plugins/stlink/stm32"
)

type rig struct {
	*Plugin
	es []Entry
}

func idOf(serial string) string { return "0483:3748:" + serial }

// newRig builds a plugin over synthetic probes, one per target.
func newRig(t *testing.T, targets map[string]*cortexm.Sim) *rig {
	t.Helper()
	r := &rig{}
	for serial, tg := range targets {
		r.es = append(r.es, Entry{Dev: SimDevice(serial), Serial: serial, Sim: sim.NewConfig(serial, tg)})
	}
	list := func() []Entry {
		var out []Entry
		for _, e := range r.es {
			if e.Sim.Present() {
				out = append(out, e)
			}
		}
		return nameEntries(out)
	}
	r.Plugin = NewWith(list, defaultOpen)
	t.Cleanup(func() {
		for _, e := range r.es {
			_ = r.CloseDevice(context.Background(), idOf(e.Serial))
		}
	})
	return r
}

func (r *rig) cfg(serial string) *sim.Config {
	for _, e := range r.es {
		if e.Serial == serial {
			return e.Sim
		}
	}
	return nil
}

func (r *rig) call(id, capName, method string, args map[string]any) (map[string]any, error) {
	st, err := structpb.NewStruct(args)
	if err != nil {
		return nil, err
	}
	resp, err := r.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: id, Capability: capName, Method: method, Args: st})
	if err != nil {
		return nil, err
	}
	return resp.GetResult().AsMap(), nil
}

func (r *rig) must(t *testing.T, id, capName, method string, args map[string]any) map[string]any {
	t.Helper()
	m, err := r.call(id, capName, method, args)
	if err != nil {
		t.Fatalf("%s.%s %v: %v", capName, method, args, err)
	}
	return m
}

func (r *rig) fail(t *testing.T, want codes.Code, sub, id, capName, method string, args map[string]any) {
	t.Helper()
	_, err := r.call(id, capName, method, args)
	if err == nil || status.Code(err) != want || !strings.Contains(err.Error(), sub) {
		t.Fatalf("%s.%s %v: want %v containing %q, got %v", capName, method, args, want, sub, err)
	}
}

const (
	dbg   = "DebugProbe"
	mem   = "MemoryAccess"
	flash = "FlashProgramming"
	ident = "TargetIdentify"
)

var allowAll = map[string]any{"allow_memory_write": true, "allow_reset": true, "allow_flash": true, "allow_erase": true}

func single(t *testing.T, cfg map[string]any) (*rig, string, *cortexm.Sim) {
	t.Helper()
	tg := cortexm.NewSim()
	r := newRig(t, map[string]*cortexm.Sim{"SIMA": tg})
	id := idOf("SIMA")
	if err := r.OpenDevice(context.Background(), id, cfg); err != nil {
		t.Fatal(err)
	}
	return r, id, tg
}

func TestIdentityKeepsEvidenceSeparate(t *testing.T) {
	r, id, _ := single(t, nil)
	m := r.must(t, id, ident, "identify", nil)
	probe := m["probe"].(map[string]any)
	if probe["serial"] != "SIMA" || m["synthetic"] != true || m["target_verified"] != false {
		t.Fatalf("%v", m)
	}
	core := m["core"].(map[string]any)
	if core["cpuid"] != "0x410fc241" || core["name"] != "Cortex-M4" {
		t.Fatalf("%v", core)
	}
	if v := m["vendor"].(map[string]any); v["recognised"] != false {
		t.Fatalf("a bare Cortex-M must not get a vendor: %v", v)
	}
	if m["breakpoint_unit"].(map[string]any)["code_comparators"] != float64(6) {
		t.Fatalf("%v", m["breakpoint_unit"])
	}
	if _, ok := m["watchpoint_unit"]; !ok {
		t.Fatalf("%v", m)
	}
}

func TestVendorHintFromFICR(t *testing.T) {
	tg := cortexm.NewSim()
	tg.Hooks = append(tg.Hooks, func(a uint32, w bool, v uint32) (uint32, bool, error) {
		if a == 0x10000100 && !w {
			return 0x52840, true, nil
		}
		return 0, false, nil
	})
	r := newRig(t, map[string]*cortexm.Sim{"SIMA": tg})
	m := r.must(t, idOf("SIMA"), ident, "identify", nil)
	v := m["vendor"].(map[string]any)
	if v["recognised"] != true || !strings.Contains(v["hint"].(string), "nRF52840") {
		t.Fatalf("%v", v)
	}
}

func TestTwoProbesAreIsolated(t *testing.T) {
	a, b := cortexm.NewSim(), cortexm.NewSim()
	_ = a.Write32(context.Background(), 0x20000000, 0xAAAA0001)
	_ = b.Write32(context.Background(), 0x20000000, 0xBBBB0002)
	r := newRig(t, map[string]*cortexm.Sim{"SIMA": a, "SIMB": b})
	for _, s := range []string{"SIMA", "SIMB"} {
		if err := r.OpenDevice(context.Background(), idOf(s), nil); err != nil {
			t.Fatal(err)
		}
	}
	rd := func(s string) string {
		return r.must(t, idOf(s), mem, "read", map[string]any{"address": "0x20000000", "length": 4.0})["hex"].(string)
	}
	for i := 0; i < 6; i++ {
		if rd("SIMA") != "0100aaaa" || rd("SIMB") != "0200bbbb" {
			t.Fatalf("round %d: sessions mixed up", i)
		}
	}
	if m := r.must(t, idOf("SIMA"), dbg, "halt", nil); m["probe_serial"] != "SIMA" {
		t.Fatal(m)
	}
	if st := r.must(t, idOf("SIMB"), dbg, "harts", nil)["harts"].([]any)[0].(map[string]any)["state"]; st != "running" {
		t.Fatalf("halting probe A changed probe B: %v", st)
	}
	// Unplugging one leaves the other usable.
	r.cfg("SIMA").Unplug()
	if _, err := r.call(idOf("SIMA"), mem, "read", map[string]any{"address": 0x20000000, "length": 4.0}); err == nil {
		t.Fatal("read on an unplugged probe succeeded")
	}
	if rd("SIMB") != "0200bbbb" {
		t.Fatal("probe B affected")
	}
}

func TestUnknownProbeListsCandidates(t *testing.T) {
	r := newRig(t, map[string]*cortexm.Sim{"SIMA": cortexm.NewSim(), "SIMB": cortexm.NewSim()})
	_, err := r.call("0483:3748:NOPE", dbg, "info", nil)
	if status.Code(err) != codes.NotFound || !strings.Contains(err.Error(), "SIMA") || !strings.Contains(err.Error(), "SIMB") {
		t.Fatalf("%v", err)
	}
}

func TestMemoryWritePolicy(t *testing.T) {
	r, id, tg := single(t, nil)
	r.fail(t, codes.PermissionDenied, "allow_memory_write", id, mem, "write", map[string]any{"address": "0x20000100", "data": "01"})
	_ = tg
	r2, id2, _ := single2(t, allowAll)
	for _, a := range []string{"0x00000100", "0x40000000", "0xE000ED00", "0x22000000", "0x2200"} {
		r2.fail(t, codes.InvalidArgument, "SRAM", id2, mem, "write", map[string]any{"address": a, "data": "00112233"})
	}
	r2.fail(t, codes.InvalidArgument, "limit", id2, mem, "write", map[string]any{"address": "0x20000000", "data": strings.Repeat("ab", 4097)})
	r2.fail(t, codes.InvalidArgument, "hex", id2, mem, "write", map[string]any{"address": "0x20000000", "data": "zz"})
}

func single2(t *testing.T, cfg map[string]any) (*rig, string, *cortexm.Sim) {
	t.Helper()
	tg := cortexm.NewSim()
	r := newRig(t, map[string]*cortexm.Sim{"SIMW": tg})
	id := idOf("SIMW")
	if err := r.OpenDevice(context.Background(), id, cfg); err != nil {
		t.Fatal(err)
	}
	return r, id, tg
}

func TestMemoryWriteWidthsAndRestore(t *testing.T) {
	r, id, _ := single(t, allowAll)
	const base = 0x20000200
	orig := r.must(t, id, mem, "read", map[string]any{"address": float64(base), "length": 64.0})["hex"]
	pat := []byte{0xA5, 0x5A, 0xC3, 0x3C, 0x96, 0x69, 0xF0, 0x0F}
	for off := 0; off < 4; off++ {
		for n := 1; n <= 8; n++ {
			m := r.must(t, id, mem, "write", map[string]any{"address": float64(base + off), "data": hex.EncodeToString(pat[:n])})
			if m["verified"] != true {
				t.Fatalf("%v", m)
			}
			got := r.must(t, id, mem, "read", map[string]any{"address": float64(base + off), "length": float64(n)})["hex"]
			if got != hex.EncodeToString(pat[:n]) {
				t.Fatalf("off %d n %d: %v", off, n, got)
			}
		}
	}
	r.must(t, id, mem, "write", map[string]any{"address": float64(base + 32), "data": "11223344"})
	r.must(t, id, mem, "write", map[string]any{"address": float64(base + 33), "data": "ee"})
	if g := r.must(t, id, mem, "read", map[string]any{"address": float64(base + 32), "length": 4.0})["hex"]; g != "11ee3344" {
		t.Fatalf("%v", g)
	}
	r.must(t, id, mem, "write", map[string]any{"address": float64(base), "data": orig})
	if g := r.must(t, id, mem, "read", map[string]any{"address": float64(base), "length": 64.0})["hex"]; g != orig {
		t.Fatalf("not restored: %v vs %v", g, orig)
	}
}

func TestReadPolicyAndFile(t *testing.T) {
	r, id, _ := single(t, nil)
	r.fail(t, codes.InvalidArgument, "allow_peripheral", id, mem, "read", map[string]any{"address": "0x40000000", "length": 4.0})
	r.fail(t, codes.InvalidArgument, "allow_peripheral", id, mem, "read", map[string]any{"address": "0xE000EDF0", "length": 4.0})
	r.fail(t, codes.InvalidArgument, "inline", id, mem, "read", map[string]any{"address": "0x20000000", "length": 5000.0})
	r.fail(t, codes.InvalidArgument, "wraps", id, mem, "read", map[string]any{"address": "0xFFFFFFFC", "length": 8.0, "allow_peripheral": true})
	p := filepath.Join(t.TempDir(), "ram.bin")
	m := r.must(t, id, mem, "read", map[string]any{"address": "0x20000000", "length": 8192.0, "path": p})
	if m["path"] != p || m["sha256"] == "" {
		t.Fatalf("%v", m)
	}
	if st, err := os.Stat(p); err != nil || st.Size() != 8192 || st.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", st, err)
	}
	r.fail(t, codes.InvalidArgument, "exist", id, mem, "read", map[string]any{"address": "0x20000000", "length": 8.0, "path": p})
}

func TestStepRequiresHaltAndRestoresStepBit(t *testing.T) {
	r, id, tg := single(t, nil)
	r.fail(t, codes.FailedPrecondition, "halt it first", id, dbg, "step", nil)
	r.must(t, id, dbg, "halt", nil)
	m := r.must(t, id, dbg, "step", map[string]any{"count": 100.0})
	if m["count"] != float64(100) || m["step_bit_restored"] != true {
		t.Fatalf("%v", m)
	}
	// the sim mixes 16- and 32-bit instructions: the deltas must not all be equal
	seen := map[float64]bool{}
	r.must(t, id, dbg, "halt", nil)
	for _, s := range m["steps"].([]any) {
		seen[s.(map[string]any)["pc_delta"].(float64)] = true
	}
	if !seen[2] || !seen[4] || tg.Steps < 100 {
		t.Fatalf("deltas %v steps %d", seen, tg.Steps)
	}
	r.fail(t, codes.InvalidArgument, "count", id, dbg, "step", map[string]any{"count": 5000.0})
}

func TestBreakpointHitAndRestore(t *testing.T) {
	r, id, _ := single(t, allowAll)
	// Learn the instruction stream from a reset-halt, then break on a later PC.
	rr := func() {
		r.must(t, id, dbg, "reset", map[string]any{"kind": "system", "halt": true})
	}
	rr()
	var target string
	for i := 0; i < 6; i++ {
		m := r.must(t, id, dbg, "step", nil)
		target = m["pc"].(string)
	}
	rr()
	before := r.must(t, id, dbg, "list_breakpoints", nil)
	set := r.must(t, id, dbg, "set_breakpoint", map[string]any{"address": target})
	if set["installed"] != float64(1) || set["slots_total"] != float64(6) {
		t.Fatalf("%v", set)
	}
	r.fail(t, codes.AlreadyExists, "already set", id, dbg, "set_breakpoint", map[string]any{"address": target})
	m := r.must(t, id, dbg, "resume", nil)
	if m["resumed"] != true {
		t.Fatalf("%v", m)
	}
	w := r.must(t, id, dbg, "wait_halt", map[string]any{"wait_ms": 2000.0})
	if w["pc"] != target || w["breakpoint_hit"] != true {
		t.Fatalf("want a hit at %s: %v", target, w)
	}
	// Resume again: the breakpoint at the current PC is stepped over first.
	m = r.must(t, id, dbg, "resume", nil)
	if m["stepped_over_breakpoint"] != target {
		t.Fatalf("%v", m)
	}
	r.must(t, id, dbg, "halt", nil)
	cl := r.must(t, id, dbg, "clear_breakpoint", map[string]any{"address": target})
	if cl["installed"] != float64(0) || cl["original_fpb_state_restored"] != true {
		t.Fatalf("%v", cl)
	}
	after := r.must(t, id, dbg, "list_breakpoints", nil)
	if after["slots_in_use"] != before["slots_in_use"] {
		t.Fatalf("trigger state not restored: %v vs %v", after, before)
	}
	r.fail(t, codes.NotFound, "no breakpoint", id, dbg, "clear_breakpoint", map[string]any{"address": target})
}

func TestBreakpointsRemovedOnClose(t *testing.T) {
	r, id, tg := single(t, allowAll)
	r.must(t, id, dbg, "halt", nil)
	r.must(t, id, dbg, "set_breakpoint", map[string]any{"address": "0x00000200"})
	if err := r.CloseDevice(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if v, _ := tg.Read32(context.Background(), cortexm.AddrFPComp); v&1 != 0 {
		t.Fatalf("FP_COMP0 0x%08x left enabled after close", v)
	}
}

func TestWatchpointLifecycle(t *testing.T) {
	r, id, tg := single(t, nil)
	demcr, _ := tg.Read32(context.Background(), cortexm.AddrDEMCR)
	m := r.must(t, id, dbg, "set_watchpoint", map[string]any{"address": "0x20000010", "kind": "write"})
	if m["installed"] != float64(1) || m["comparators_total"] == nil {
		t.Fatalf("%v", m)
	}
	l := r.must(t, id, dbg, "list_watchpoints", nil)
	if l["in_use"] != float64(1) {
		t.Fatalf("%v", l)
	}
	r.fail(t, codes.NotFound, "no watchpoint", id, dbg, "clear_watchpoint", map[string]any{"address": "0x20000999"})
	r.must(t, id, dbg, "clear_watchpoint", map[string]any{"address": "0x20000010"})
	if g, _ := tg.Read32(context.Background(), cortexm.AddrDEMCR); g != demcr {
		t.Fatalf("DEMCR 0x%08x not restored to 0x%08x", g, demcr)
	}
	if l := r.must(t, id, dbg, "list_watchpoints", nil); l["in_use"] != float64(0) {
		t.Fatalf("%v", l)
	}
}

func TestResetNeedsPermissionAndPinIsNotClaimedBlindly(t *testing.T) {
	r, id, _ := single(t, nil)
	r.fail(t, codes.PermissionDenied, "allow_reset", id, dbg, "reset", map[string]any{"kind": "system"})
	r2, id2, _ := single2(t, allowAll)
	r2.fail(t, codes.PermissionDenied, "reset-pin", id2, dbg, "reset", map[string]any{"kind": "pin"})
	r2.fail(t, codes.InvalidArgument, "kind", id2, dbg, "reset", map[string]any{"kind": "bogus"})
	m := r2.must(t, id2, dbg, "reset", map[string]any{"kind": "system", "halt": true})
	if m["reset_observed"] != true || m["at_reset_vector"] != true || m["halted"] != true {
		t.Fatalf("%v", m)
	}
}

func TestSettingsCannotChangeOnOpenDevice(t *testing.T) {
	r, id, _ := single(t, nil)
	r.fail(t, codes.InvalidArgument, "cannot change", id, dbg, "halt", map[string]any{"allow_memory_write": true})
}

func TestUnknownSettingRejected(t *testing.T) {
	r := newRig(t, map[string]*cortexm.Sim{"SIMA": cortexm.NewSim()})
	if err := r.OpenDevice(context.Background(), idOf("SIMA"), map[string]any{"allow_everything": true}); err == nil {
		t.Fatal("unknown setting accepted")
	}
}

func TestReopenReplacesTheSession(t *testing.T) {
	r, id, _ := single(t, nil)
	if err := r.OpenDevice(context.Background(), id, nil); err != nil {
		t.Fatal(err)
	}
	r.must(t, id, ident, "identify", nil)
	if n := len(r.devs); n != 1 {
		t.Fatalf("%d open sessions for one probe", n)
	}
}

func TestUnplugReplugReconnectChecksIdentity(t *testing.T) {
	r, id, _ := single(t, nil)
	m0 := r.must(t, id, ident, "identify", nil)
	r.cfg("SIMA").Unplug()
	_, err := r.call(id, mem, "read", map[string]any{"address": "0x20000000", "length": 4.0})
	if err == nil || !IsDeviceGone(err) {
		t.Fatalf("want DEVICE_GONE, got %v", err)
	}
	r.cfg("SIMA").Replug()
	rc := r.must(t, id, dbg, "reconnect", map[string]any{})
	if rc["reconnected"] != true || rc["identity_verified"] != true {
		t.Fatalf("%v", rc)
	}
	m1 := r.must(t, id, ident, "identify", nil)
	if fmt.Sprint(m0["core"]) != fmt.Sprint(m1["core"]) {
		t.Fatalf("%v vs %v", m0["core"], m1["core"])
	}
}

func TestReconnectRefusesADifferentTarget(t *testing.T) {
	r, id, _ := single(t, map[string]any{"reconnect_timeout_ms": 300.0})
	r.must(t, id, ident, "identify", nil)
	r.cfg("SIMA").Unplug()
	_, _ = r.call(id, mem, "read", map[string]any{"address": "0x20000000", "length": 4.0})
	// A different core behind the same USB serial must not be accepted.
	r.es[0].Sim.DPIDCode = 0x1BA01477
	r.cfg("SIMA").Replug()
	_, err := r.call(id, dbg, "reconnect", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("a different target was accepted: %v", err)
	}
}

func TestGoneWhenNeverComesBack(t *testing.T) {
	r, id, _ := single(t, map[string]any{"reconnect_timeout_ms": 200.0})
	r.must(t, id, ident, "identify", nil)
	r.cfg("SIMA").Unplug()
	_, err := r.call(id, mem, "read", map[string]any{"address": "0x20000000", "length": 4.0})
	if !IsDeviceGone(err) {
		t.Fatalf("%v", err)
	}
}

func TestSetSpeed(t *testing.T) {
	r, id, _ := single(t, nil)
	m := r.must(t, id, dbg, "set_speed", map[string]any{"khz": 950.0})
	if m["actual_khz"] != float64(950) {
		t.Fatalf("%v", m)
	}
	r.fail(t, codes.InvalidArgument, "khz", id, dbg, "set_speed", map[string]any{"khz": 99999.0})
}

func TestInfoDoesNotClaimWhenClosed(t *testing.T) {
	r := newRig(t, map[string]*cortexm.Sim{"SIMA": cortexm.NewSim()})
	m := r.must(t, idOf("SIMA"), dbg, "info", nil)
	if m["open"] != false || m["serial"] != "SIMA" {
		t.Fatalf("%v", m)
	}
	if r.devs[idOf("SIMA")] != nil {
		t.Fatal("info opened the probe")
	}
}

// ---- flash ----

func flashRig(t *testing.T, cfg map[string]any) (*rig, string, *stm32.SimF4) {
	t.Helper()
	f := stm32.NewSimF4(512)
	r := newRig(t, map[string]*cortexm.Sim{"SIMF": f.Sim})
	id := idOf("SIMF")
	if err := r.OpenDevice(context.Background(), id, cfg); err != nil {
		t.Fatal(err)
	}
	return r, id, f
}

func TestFlashOnNonSTM32IsUnsupportedAndWritesNothing(t *testing.T) {
	r, id, _ := single(t, map[string]any{"allow_memory_write": true, "allow_reset": true, "allow_flash": true, "allow_erase": true, "allow_chip_erase": true})
	m := r.must(t, id, flash, "info", nil)
	if m["supported"] != false || m["status"] != unsupportedHW {
		t.Fatalf("%v", m)
	}
	for _, c := range []struct {
		method string
		args   map[string]any
	}{
		{"read", map[string]any{"address": "0x08000000", "length": 16.0}},
		{"backup", map[string]any{"dir": t.TempDir()}},
		{"erase", map[string]any{"mode": "chip", "confirm": tokEraseChip, "backup": "x"}},
		{"program", map[string]any{"path": "x", "address": "0x08000000", "confirm": tokProgram, "backup": "x"}},
	} {
		if c.method == "program" {
			f := filepath.Join(t.TempDir(), "i.bin")
			_ = os.WriteFile(f, []byte{1, 2, 3, 4}, 0o600)
			c.args["path"] = f
		}
		r.fail(t, codes.FailedPrecondition, unsupportedHW, id, flash, c.method, c.args)
	}
}

func TestFlashInfoGeometry(t *testing.T) {
	r, id, _ := flashRig(t, nil)
	m := r.must(t, id, flash, "info", nil)
	if m["supported"] != true || m["flash_kb"] != float64(512) || m["sector_count"] != float64(8) {
		t.Fatalf("%v", m)
	}
	if p := m["protection"].(map[string]any); p["read_only"] != true || p["rdp_level"] != float64(0) {
		t.Fatalf("%v", p)
	}
}

func writeFile(t *testing.T, name string, b []byte) string {
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFlashBackupTwiceIdentical(t *testing.T) {
	r, id, f := flashRig(t, nil)
	for i := range f.Flash {
		f.Flash[i] = byte(i * 7)
	}
	dir := t.TempDir()
	m := r.must(t, id, flash, "backup", map[string]any{"dir": dir})
	if m["identical_reads"] != true || m["size_bytes"] != float64(512<<10) {
		t.Fatalf("%v", m)
	}
	b, err := os.ReadFile(m["path"].(string))
	if err != nil || sha(b) != m["sha256"] {
		t.Fatalf("%v %v", err, m)
	}
	if st, _ := os.Stat(m["path"].(string)); st.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode %v", st.Mode())
	}
	if f.Erases+f.Programs != 0 {
		t.Fatal("backup modified flash")
	}
}

func TestFlashGates(t *testing.T) {
	// No permission at all.
	r, id, f := flashRig(t, nil)
	img := writeFile(t, "i.bin", []byte{1, 2, 3, 4})
	r.fail(t, codes.PermissionDenied, "allow_flash", id, flash, "program", map[string]any{"path": img, "address": "0x08000000", "confirm": tokProgram, "backup": "x"})
	r.fail(t, codes.PermissionDenied, "allow_flash", id, flash, "erase", map[string]any{"mode": "region", "address": "0x08000000", "length": 16384.0, "confirm": tokEraseReg, "backup": "x"})
	r4, id4, _ := flashRig(t, map[string]any{"allow_flash": true})
	r4.fail(t, codes.PermissionDenied, "allow_erase", id4, flash, "erase", map[string]any{"mode": "region", "address": "0x08000000", "length": 16384.0, "confirm": tokEraseReg, "backup": "x"})
	r.fail(t, codes.InvalidArgument, "mode", id, flash, "erase", map[string]any{"confirm": tokEraseReg, "backup": "x"})
	if f.Erases+f.Programs != 0 {
		t.Fatal("a refused call touched flash")
	}
	// Permissions without tokens / backup.
	r2, id2, f2 := flashRig(t, allowAll)
	r2.fail(t, codes.PermissionDenied, "confirm", id2, flash, "program", map[string]any{"path": img, "address": "0x08000000", "backup": "x"})
	r2.fail(t, codes.PermissionDenied, "verified full backup", id2, flash, "program", map[string]any{"path": img, "address": "0x08000000", "confirm": tokProgram})
	r2.fail(t, codes.FailedPrecondition, "backup", id2, flash, "program", map[string]any{"path": img, "address": "0x08000000", "confirm": tokProgram, "backup": filepath.Join(t.TempDir(), "none.bin")})
	// Chip erase needs its own permission, then its own token.
	r2.fail(t, codes.PermissionDenied, "allow_chip_erase", id2, flash, "erase", map[string]any{"mode": "chip", "confirm": tokEraseChip, "backup": "x"})
	// A generic region token must not unlock a chip erase.
	r3, id3, f3 := flashRig(t, map[string]any{"allow_flash": true, "allow_erase": true, "allow_chip_erase": true})
	r3.fail(t, codes.PermissionDenied, tokEraseChip, id3, flash, "erase", map[string]any{"mode": "chip", "confirm": tokEraseReg, "backup": "x"})
	if f2.Erases+f2.Programs+f3.Erases+f3.Programs != 0 {
		t.Fatal("a refused call touched flash")
	}
}

func TestFlashErrorPaths(t *testing.T) {
	r, id, f := flashRig(t, allowAll)
	bk := r.must(t, id, flash, "backup", map[string]any{"dir": t.TempDir()})["path"].(string)
	args := func(a map[string]any) map[string]any {
		a["confirm"], a["backup"] = tokEraseReg, bk
		return a
	}
	r.fail(t, codes.OutOfRange, "outside the flash", id, flash, "erase", args(map[string]any{"mode": "region", "address": "0x08100000", "length": 16384.0}))
	r.fail(t, codes.InvalidArgument, "at least 1", id, flash, "erase", args(map[string]any{"mode": "region", "address": "0x08000000", "length": 0.0}))
	r.fail(t, codes.InvalidArgument, "sector boundaries", id, flash, "erase", args(map[string]any{"mode": "region", "address": "0x08000100", "length": 16384.0}))
	r.fail(t, codes.OutOfRange, "does not fit", id, flash, "program", map[string]any{"path": writeFile(t, "i", []byte{1, 2, 3, 4}), "address": "0x080FFFFE", "confirm": tokProgram, "backup": bk})
	r.fail(t, codes.OutOfRange, "outside", id, flash, "read", map[string]any{"address": "0x20000000", "length": 16.0})
	if f.Erases+f.Programs != 0 {
		t.Fatal("an error path touched flash")
	}
	// A backup of another chip is refused.
	g := stm32.NewSimF4(1024)
	_ = g
	other := filepath.Join(t.TempDir(), "o.bin")
	_ = os.WriteFile(other, make([]byte, 512<<10), 0o600)
	r.fail(t, codes.FailedPrecondition, "manifest", id, flash, "program", map[string]any{"path": writeFile(t, "i", []byte{1, 2, 3, 4}), "address": "0x08000000", "confirm": tokProgram, "backup": other})
}

func TestFlashProgramVerifyRestoreCycle(t *testing.T) {
	r, id, f := flashRig(t, allowAll)
	for i := range f.Flash {
		f.Flash[i] = byte(i*13 + 5)
	}
	orig := append([]byte(nil), f.Flash...)
	bk := r.must(t, id, flash, "backup", map[string]any{"dir": t.TempDir()})
	img := make([]byte, 20000)
	for i := range img {
		img[i] = byte(i * 3)
	}
	imgPath := writeFile(t, "app.bin", img)
	// Program into the middle of sectors: neighbours must survive.
	addr := uint32(0x08004100)
	pm := r.must(t, id, flash, "program", map[string]any{"path": imgPath, "address": float64(addr), "confirm": tokProgram, "backup": bk["path"], "reset": "none"})
	if pm["verified"] != true || pm["sectors_erased"].(float64) < 1 {
		t.Fatalf("%v", pm)
	}
	off := addr - stm32.FlashBase
	if string(f.Flash[off:int(off)+len(img)]) != string(img) {
		t.Fatal("image not in flash")
	}
	if string(f.Flash[:0x4100]) != string(orig[:0x4100]) {
		t.Fatal("bytes before the image were not preserved")
	}
	end := int(off) + len(img)
	if string(f.Flash[end:]) != string(orig[end:]) {
		t.Fatal("bytes after the image were not preserved")
	}
	vm := r.must(t, id, flash, "verify", map[string]any{"path": imgPath, "address": float64(addr)})
	if vm["verified"] != true {
		t.Fatalf("%v", vm)
	}
	// A corrupted verify must fail loudly.
	f.Flash[off+10] ^= 0x01
	r.fail(t, codes.DataLoss, "verify FAILED", id, flash, "verify", map[string]any{"path": imgPath, "address": float64(addr)})
	// Identical re-program is a no-op.
	f.Flash[off+10] ^= 0x01
	if _, err := r.call(id, flash, "program", map[string]any{"path": imgPath, "address": float64(addr), "confirm": tokProgram, "backup": bk["path"]}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("a stale backup must be refused: %v", err)
	}
	bk2 := r.must(t, id, flash, "backup", map[string]any{"dir": t.TempDir()})
	er, pr := f.Erases, f.Programs
	r.must(t, id, flash, "program", map[string]any{"path": imgPath, "address": float64(addr), "confirm": tokProgram, "backup": bk2["path"]})
	if f.Erases != er || f.Programs != pr {
		t.Fatal("re-programming identical bytes touched flash")
	}
	// Restore.
	rm := r.must(t, id, flash, "restore", map[string]any{"path": bk["path"], "confirm": tokRestore})
	if rm["whole_flash_matches_backup"] != true || sha(f.Flash) != bk["sha256"] || string(f.Flash) != string(orig) {
		t.Fatalf("%v", rm)
	}
}

func TestProgramCorruptionIsReported(t *testing.T) {
	r, id, f := flashRig(t, allowAll)
	bk := r.must(t, id, flash, "backup", map[string]any{"dir": t.TempDir()})
	f.CorruptProgram = true
	_, err := r.call(id, flash, "program", map[string]any{"path": writeFile(t, "i", []byte{1, 2, 3, 4, 5, 6, 7, 8}), "address": "0x08000000", "confirm": tokProgram, "backup": bk["path"]})
	if status.Code(err) != codes.DataLoss {
		t.Fatalf("a corrupted program must not report success: %v", err)
	}
}

func TestChipEraseTierAndBackupBinding(t *testing.T) {
	r, id, f := flashRig(t, map[string]any{"allow_flash": true, "allow_erase": true, "allow_chip_erase": true})
	for i := range f.Flash {
		f.Flash[i] = 0x11
	}
	bk := r.must(t, id, flash, "backup", map[string]any{"dir": t.TempDir()})
	f.Flash[0] = 0x22 // changed after the backup
	r.fail(t, codes.FailedPrecondition, "no longer matches", id, flash, "erase", map[string]any{"mode": "chip", "confirm": tokEraseChip, "backup": bk["path"]})
	f.Flash[0] = 0x11
	m := r.must(t, id, flash, "erase", map[string]any{"mode": "chip", "confirm": tokEraseChip, "backup": bk["path"]})
	if m["verified_erased"] != true || f.Erases != 8 {
		t.Fatalf("%v erases %d", m, f.Erases)
	}
	for _, b := range f.Flash {
		if b != 0xFF {
			t.Fatal("not erased")
		}
	}
}
