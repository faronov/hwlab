//go:build hardware

package stlinkplugin

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
)

// These tests drive a REAL ST-Link probe and the target behind it.
//
//	HWLAB_STLINK_HW=1 go test -tags hardware -run HW -v ./plugins/stlink/
//
// HWLAB_STLINK_SERIAL selects a probe when several are attached. They never
// erase or program flash and never touch option bytes: the memory they write
// is SRAM that is saved first and put back, and every test leaves the core
// running. The attached target is whatever is wired to the probe; the tests do
// not assume a vendor (the flash tests assert that a non-STM32 target is
// refused).

type hw struct {
	*Plugin
	id string
}

func hwRig(t *testing.T) hw {
	t.Helper()
	if os.Getenv("HWLAB_STLINK_HW") != "1" {
		t.Skip("set HWLAB_STLINK_HW=1 to run against a real ST-Link")
	}
	p := New()
	devs, err := p.ListDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := os.Getenv("HWLAB_STLINK_SERIAL")
	for _, d := range devs {
		if strings.HasPrefix(d.Serial, "SIM") {
			continue
		}
		if want == "" || d.Serial == want {
			return hw{p, d.Id}
		}
	}
	t.Skip("no ST-Link attached")
	return hw{}
}

var hwCfg = map[string]any{"allow_memory_write": true, "allow_reset": true}

func hwOpen(t *testing.T, cfg map[string]any) hw {
	t.Helper()
	h := hwRig(t)
	if cfg == nil {
		cfg = hwCfg
	}
	if err := h.OpenDevice(context.Background(), h.id, cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = h.try("DebugProbe", "clear_breakpoint", map[string]any{"all": true})
		_, _ = h.try("DebugProbe", "clear_watchpoint", map[string]any{"all": true})
		_, _ = h.try("DebugProbe", "resume", nil)
		_ = h.CloseDevice(context.Background(), h.id)
	})
	return h
}

func (h hw) try(capName, method string, args map[string]any) (map[string]any, error) {
	s, _ := structpb.NewStruct(args)
	res, err := h.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: h.id, Capability: capName, Method: method, Args: s})
	if err != nil {
		return nil, err
	}
	return res.Result.AsMap(), nil
}

func (h hw) call(t *testing.T, capName, method string, args map[string]any) map[string]any {
	t.Helper()
	r, err := h.try(capName, method, args)
	if err != nil {
		t.Fatalf("%s.%s %v: %v", capName, method, args, err)
	}
	return r
}

func (h hw) halt(t *testing.T) {
	t.Helper()
	if r := h.call(t, "DebugProbe", "halt", nil); r["halted"] != true {
		t.Fatalf("halt: %v", r)
	}
}

func (h hw) resume(t *testing.T) { t.Helper(); h.call(t, "DebugProbe", "resume", nil) }

func hexAddr(a uint32) string { return fmt.Sprintf("0x%08x", a) }

func parseHex(t *testing.T, s any) uint32 {
	t.Helper()
	str, _ := s.(string)
	v, err := strconv.ParseUint(strings.TrimPrefix(str, "0x"), 16, 32)
	if err != nil {
		t.Fatalf("not a hex value: %v", s)
	}
	return uint32(v)
}

func (h hw) read(t *testing.T, addr uint32, n int) []byte {
	t.Helper()
	r := h.call(t, "MemoryAccess", "read", map[string]any{"address": hexAddr(addr), "length": float64(n)})
	b, err := hex.DecodeString(r["hex"].(string))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (h hw) write(t *testing.T, addr uint32, b []byte) {
	t.Helper()
	h.call(t, "MemoryAccess", "write", map[string]any{"address": hexAddr(addr), "data": hex.EncodeToString(b)})
}

func (h hw) resetHalt(t *testing.T, kind string) uint32 {
	t.Helper()
	r := h.call(t, "DebugProbe", "reset", map[string]any{"kind": kind, "halt": true})
	if r["reset_observed"] != true || r["halted"] != true {
		t.Fatalf("reset-halt not observed: %v", r)
	}
	return parseHex(t, r["pc"])
}

func TestHWDiscovery(t *testing.T) {
	h := hwRig(t)
	devs, err := h.ListDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, d := range devs {
		if strings.HasPrefix(d.Serial, "SIM") {
			continue
		}
		n++
		t.Logf("probe id=%s serial=%s name=%q props=%v", d.Id, d.Serial, d.Name, d.Properties)
		if d.Properties["vid"] != "0483" || d.Serial == "" || !strings.HasSuffix(d.Id, d.Serial) {
			t.Errorf("unexpected probe entry %+v", d)
		}
	}
	if n == 0 {
		t.Fatal("no probe listed")
	}
	// Info before open must not claim the probe.
	r, err := h.try("DebugProbe", "info", nil)
	if err != nil || r["open"] != false {
		t.Fatalf("info claimed the probe or failed: %v %v", r, err)
	}
}

func TestHWIdentify(t *testing.T) {
	h := hwOpen(t, nil)
	m := h.call(t, "TargetIdentify", "identify", nil)
	probe := m["probe"].(map[string]any)
	core := m["core"].(map[string]any)
	dp := m["debug_port"].(map[string]any)
	t.Logf("probe=%v core=%v dp=%v vendor=%v", probe, core, dp, m["vendor"])
	if m["synthetic"] == true || m["target_verified"] != true {
		t.Fatalf("not a verified real target: %v", m)
	}
	if probe["vid"] != "0483" || probe["serial"] == "" || probe["firmware"] == "" {
		t.Fatalf("probe facts missing: %v", probe)
	}
	if !strings.HasPrefix(fmt.Sprint(core["cpuid"]), "0x41") || dp["dp_idcode"] == "" {
		t.Fatalf("core/dp: %v %v", core, dp)
	}
	if m["probe_serial"] != probe["serial"] {
		t.Fatalf("serial not persisted: %v", m)
	}
}

func TestHWHaltResumeRegisters(t *testing.T) {
	h := hwOpen(t, nil)
	for i := 0; i < 25; i++ {
		h.halt(t)
		st := h.call(t, "DebugProbe", "read_registers", map[string]any{"crosscheck": true, "keep_halted": true})
		if st["halted"] != true {
			t.Fatalf("%v", st)
		}
		regs := st["registers"].(map[string]any)
		for _, n := range []string{"r0", "r12", "sp", "lr", "pc", "xpsr", "msp", "psp", "control", "primask"} {
			if _, ok := regs[n]; !ok {
				t.Fatalf("register %s missing", n)
			}
		}
		if st["native_matches_dcrsr"] != true {
			t.Fatalf("READALLREGS and DCRSR disagree on pc: %v", st)
		}
		if i == 0 {
			t.Logf("registers: %v pc via DCRSR %v", regs, st["pc_via_dcrsr"])
		}
		h.resume(t)
	}
	// Reading registers of a running core halts and resumes it.
	r := h.call(t, "DebugProbe", "read_registers", nil)
	if r["resumed"] != true {
		t.Fatalf("a running core must be resumed again: %v", r)
	}
	id := h.call(t, "DebugProbe", "chip_info", nil)
	if id["state"].(map[string]any)["halted"] == true {
		t.Fatal("core left halted")
	}
}

// scratch finds n bytes of SRAM that read identically while the core is
// halted, scanning downward from the end of the SRAM the target reports as
// readable. A halted core and no DMA mean nothing else uses it.
func (h hw) scratch(t *testing.T, n int) uint32 {
	t.Helper()
	sp := parseHex(t, h.call(t, "DebugProbe", "read_registers", map[string]any{"keep_halted": true})["registers"].(map[string]any)["msp"])
	// Start below the stack pointer so the live stack is never touched.
	for a := (sp - 0x400 - uint32(n)) &^ 0xFF; a >= 0x20000400; a -= 0x100 {
		x := h.read(t, a, n)
		y := h.read(t, a, n)
		if bytes.Equal(x, y) {
			return a
		}
	}
	t.Fatal("no stable SRAM scratch area")
	return 0
}

func TestHWSRAMWriteRestore(t *testing.T) {
	h := hwOpen(t, nil)
	h.halt(t)
	base := h.scratch(t, 64)
	orig := h.read(t, base, 64)
	t.Logf("scratch SRAM at 0x%08x, original %x...", base, orig[:8])
	pat := func(seed byte, n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = seed ^ byte(i*37+11)
		}
		return b
	}
	cases := []struct {
		off  uint32
		data []byte
		name string
	}{
		{0, pat(0xA5, 4), "32-bit aligned"},
		{4, pat(0x5A, 2), "16-bit aligned"},
		{6, pat(0x3C, 1), "8-bit"},
		{9, pat(0xC3, 3), "unaligned start"},
		{13, pat(0x99, 7), "unaligned both ends"},
		{20, pat(0x11, 32), "block"},
	}
	for _, c := range cases {
		h.write(t, base+c.off, c.data)
		if got := h.read(t, base+c.off, len(c.data)); !bytes.Equal(got, c.data) {
			t.Fatalf("%s: wrote %x read %x", c.name, c.data, got)
		}
		// Neighbours must be untouched.
		all := h.read(t, base, 64)
		for i := range all {
			in := uint32(i) >= c.off && uint32(i) < c.off+uint32(len(c.data))
			if !in && all[i] != orig[i] && !coveredByEarlier(cases, c.off, uint32(i)) {
				t.Fatalf("%s: byte %d outside the write changed", c.name, i)
			}
		}
	}
	// Intentionally invalid accesses must be refused and leave nothing behind.
	for _, bad := range []uint32{0x00000000, 0x08000000, 0x40000000, 0xE000ED00, 0x22000000} {
		_, err := h.try("MemoryAccess", "write", map[string]any{"address": hexAddr(bad), "data": "00112233"})
		if err == nil {
			t.Fatalf("write to %08x was accepted", bad)
		}
	}
	h.write(t, base, orig)
	if got := h.read(t, base, 64); !bytes.Equal(got, orig) {
		t.Fatalf("restore failed: %x vs %x", got, orig)
	}
	t.Log("SRAM restored byte for byte")
}

func coveredByEarlier(cases []struct {
	off  uint32
	data []byte
	name string
}, upTo, i uint32) bool {
	for _, c := range cases {
		if i >= c.off && i < c.off+uint32(len(c.data)) {
			return true
		}
		if c.off == upTo {
			break
		}
	}
	return false
}

func TestHWWritePolicyDefaultDeny(t *testing.T) {
	h := hwOpen(t, map[string]any{})
	_, err := h.try("MemoryAccess", "write", map[string]any{"address": "0x20000100", "data": "00"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a write without allow_memory_write must be denied: %v", err)
	}
	_, err = h.try("DebugProbe", "reset", map[string]any{"kind": "system"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a reset without allow_reset must be denied: %v", err)
	}
}

// instrLen is the Thumb instruction length from its first halfword.
func instrLen(hw uint16) uint32 {
	switch hw >> 11 {
	case 0x1D, 0x1E, 0x1F:
		return 4
	}
	return 2
}

func TestHWSingleStep(t *testing.T) {
	h := hwOpen(t, nil)
	h.resetHalt(t, "system")
	seq, other := 0, 0
	var last string
	for round := 0; round < 4; round++ {
		r := h.call(t, "DebugProbe", "step", map[string]any{"count": 25.0})
		if r["step_bit_restored"] != true || r["halted"] != true {
			t.Fatalf("%v", r)
		}
		for _, s := range r["steps"].([]any) {
			m := s.(map[string]any)
			before := parseHex(t, m["pc_before"])
			after := parseHex(t, m["pc_after"])
			word := binary.LittleEndian.Uint16(h.read(t, before&^1, 2))
			if after-before == instrLen(word) {
				seq++
			} else {
				other++
				if after&1 != 0 {
					t.Fatalf("odd PC after a step: %v", m)
				}
			}
			last = fmt.Sprint(m)
		}
		if round == 0 {
			t.Logf("first steps: %v", r["steps"])
		}
		if c := fmt.Sprint(r["cause"]); !strings.Contains(c, "halted") && !strings.Contains(c, "step") {
			t.Logf("cause=%v", r["cause"])
		}
	}
	t.Logf("100 steps: %d sequential (delta == decoded length), %d other (branches/calls); last %s", seq, other, last)
	if seq < 60 {
		t.Fatalf("only %d of 100 steps advanced by the decoded instruction length", seq)
	}
	if h.call(t, "DebugProbe", "chip_info", nil)["state"].(map[string]any)["stepping"] == true {
		t.Fatal("C_STEP still set")
	}
	h.resume(t)
	time.Sleep(50 * time.Millisecond)
	if st := h.call(t, "DebugProbe", "chip_info", nil)["state"].(map[string]any); st["halted"] == true {
		t.Fatalf("core did not run after resume: %v", st)
	}
}

func TestHWBreakpoint(t *testing.T) {
	h := hwOpen(t, nil)
	info := h.call(t, "DebugProbe", "list_breakpoints", nil)
	t.Logf("FPB: %v", info)
	for round := 0; round < 5; round++ {
		h.resetHalt(t, "system")
		// A deterministic address: the PC after 12 instructions of start-up code.
		st := h.call(t, "DebugProbe", "step", map[string]any{"count": 12.0})
		target := parseHex(t, st["pc"])
		h.resetHalt(t, "system")
		sb := h.call(t, "DebugProbe", "set_breakpoint", map[string]any{"address": hexAddr(target), "run_wait_ms": 2000.0})
		if sb["hit"] != true || parseHex(t, sb["halted_pc"]) != target {
			t.Fatalf("round %d: breakpoint at 0x%08x not hit: %v", round, target, sb)
		}
		t.Logf("round %d: hit at 0x%08x cause=%v slot=%v", round, target, sb["cause"], sb["slot"])
		h.call(t, "DebugProbe", "clear_breakpoint", map[string]any{"address": hexAddr(target)})
		if l := h.call(t, "DebugProbe", "list_breakpoints", nil); fmt.Sprint(l["installed"]) != "0" && fmt.Sprint(l["breakpoints"]) != "[]" {
			t.Logf("after clear: %v", l)
		}
		h.resume(t)
	}
	// With the breakpoint cleared the core runs on.
	time.Sleep(100 * time.Millisecond)
	if h.call(t, "DebugProbe", "chip_info", nil)["state"].(map[string]any)["halted"] == true {
		t.Fatal("core halted with no breakpoint installed")
	}
}

func TestHWWatchpointInvestigation(t *testing.T) {
	h := hwOpen(t, nil)
	wu := h.call(t, "DebugProbe", "list_watchpoints", nil)
	t.Logf("DWT: %v", wu)
	h.resetHalt(t, "system")
	// The reset handler of most firmwares writes the start of RAM (.data/.bss
	// init); a write watchpoint there is a deterministic trigger if it does.
	h.call(t, "DebugProbe", "set_watchpoint", map[string]any{"address": "0x20000000", "kind": "write", "mask_bits": 4.0})
	h.resume(t)
	w, err := h.try("DebugProbe", "wait_halt", map[string]any{"wait_ms": 1500.0})
	if err != nil {
		t.Logf("no watchpoint hit within 1.5 s (%v): the start-up code does not write 0x20000000..+16; hit NOT demonstrated", err)
	} else {
		t.Logf("halted: %v", w)
		if w["watchpoint_hit"] != true {
			t.Fatalf("halted for another reason: %v", w)
		}
	}
	h.call(t, "DebugProbe", "clear_watchpoint", map[string]any{"all": true})
	l := h.call(t, "DebugProbe", "list_watchpoints", nil)
	t.Logf("after clear: %v", l)
}

func TestHWResetKinds(t *testing.T) {
	h := hwOpen(t, nil)
	for _, kind := range []string{"system", "core"} {
		for _, halt := range []bool{false, true} {
			r := h.call(t, "DebugProbe", "reset", map[string]any{"kind": kind, "halt": halt})
			t.Logf("reset %s halt=%v -> %v", kind, halt, r)
			if r["reset_observed"] != true {
				t.Fatalf("%s reset not observed: %v", kind, r)
			}
			if halt && r["halted"] != true {
				t.Fatalf("%s reset-halt did not halt: %v", kind, r)
			}
			if halt {
				h.resume(t)
			}
			time.Sleep(50 * time.Millisecond)
			if h.call(t, "TargetIdentify", "identify", nil)["target_verified"] != true {
				t.Fatalf("identity lost after %s reset", kind)
			}
		}
	}
	// A pin reset is only reported when something proves it happened.
	_, err := h.try("DebugProbe", "reset", map[string]any{"kind": "pin"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("pin reset without its confirmation must be denied: %v", err)
	}
	r, err := h.try("DebugProbe", "reset", map[string]any{"kind": "pin", "confirm": "reset-pin", "halt": true})
	if err != nil {
		t.Logf("NRST: NOT claimed (%v)", err)
	} else {
		t.Logf("NRST reset observed: %v", r)
		if r["reset_observed"] != true {
			t.Fatalf("%v", r)
		}
		h.resume(t)
	}
}

func TestHWReopenAndReconnect(t *testing.T) {
	h := hwOpen(t, nil)
	serial := h.call(t, "TargetIdentify", "identify", nil)["probe_serial"]
	id0 := fmt.Sprint(h.call(t, "TargetIdentify", "identify", nil)["core"])
	// open, identify, halt, read memory, resume, close - 20 rounds.
	for i := 0; i < 20; i++ {
		if err := h.CloseDevice(context.Background(), h.id); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
		if err := h.OpenDevice(context.Background(), h.id, hwCfg); err != nil {
			t.Fatalf("reopen %d: %v", i, err)
		}
		m := h.call(t, "TargetIdentify", "identify", nil)
		if m["probe_serial"] != serial || fmt.Sprint(m["core"]) != id0 {
			t.Fatalf("reopen %d changed identity: %v", i, m)
		}
		h.halt(t)
		if got := h.read(t, 0x20000000, 4); len(got) != 4 {
			t.Fatal("read")
		}
		h.resume(t)
	}
	rc := h.call(t, "DebugProbe", "reconnect", nil)
	if rc["reconnected"] != true || rc["identity_verified"] != true {
		t.Fatalf("%v", rc)
	}
	if fmt.Sprint(h.call(t, "TargetIdentify", "identify", nil)["core"]) != id0 {
		t.Fatal("identity differs after reconnect")
	}
}

func TestHWConcurrentOpenIsRefused(t *testing.T) {
	h := hwOpen(t, nil)
	other := New()
	err := other.OpenDevice(context.Background(), h.id, nil)
	if err == nil {
		_ = other.CloseDevice(context.Background(), h.id)
		t.Fatal("a second process-level open of the same probe succeeded")
	}
	t.Logf("second open refused: %v", err)
	if m := h.call(t, "TargetIdentify", "identify", nil); m["target_verified"] != true {
		t.Fatalf("the first session was disturbed: %v", m)
	}
}

// TestHWFlashInfoReadOnly covers the non-destructive flash path on a
// recognised STM32: geometry, protection, two identical full reads and the
// refusals that apply without any allow_* switch.
func TestHWFlashInfoReadOnly(t *testing.T) {
	h := hwOpen(t, nil)
	info := h.call(t, "FlashProgramming", "info", nil)
	if info["supported"] != true {
		t.Skipf("the attached target is not a supported STM32: %v", info["status"])
	}
	t.Logf("flash: %v %v KB (%v), %v sectors of %v B, protection %v", info["chip"], info["flash_kb"], info["flash_size_source"], info["sector_count"], info["page_size"], info["protection"])
	if info["sector_count"] == nil || info["flash_kb"] == nil || info["protection"] == nil {
		t.Fatalf("incomplete flash info: %v", info)
	}
	bk := h.call(t, "FlashProgramming", "backup", map[string]any{"dir": t.TempDir()})
	if bk["identical_reads"] != true || bk["sha256"] != bk["second_read_sha256"] {
		t.Fatalf("%v", bk)
	}
	t.Logf("full flash: %v bytes, sha256 %v, two identical reads in %.2fs", bk["size_bytes"], bk["sha256"], bk["read_seconds"])
	for _, c := range []struct {
		m string
		a map[string]any
	}{
		{"erase", map[string]any{"mode": "region", "address": "0x08000000", "length": 1024.0, "confirm": tokEraseReg, "backup": bk["path"]}},
		{"restore", map[string]any{"path": bk["path"], "confirm": tokRestore}},
	} {
		if _, err := h.try("FlashProgramming", c.m, c.a); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("%s without allow_flash/allow_erase must be denied: %v", c.m, err)
		}
	}
}

func TestHWFlashRefusedOnNonSTM32(t *testing.T) {
	h := hwOpen(t, map[string]any{"allow_memory_write": true, "allow_reset": true, "allow_flash": true, "allow_erase": true, "allow_chip_erase": true})
	info := h.call(t, "FlashProgramming", "info", nil)
	if info["supported"] != false || info["status"] != unsupportedHW {
		t.Skipf("the attached target is a recognised STM32 (%v): the destructive-path assertions below do not apply", info["status"])
	}
	t.Logf("flash info: %v", info)
	before := h.read(t, 0, 256)
	img := t.TempDir() + "/i.bin"
	_ = os.WriteFile(img, []byte{1, 2, 3, 4}, 0o600)
	for _, c := range []struct {
		m string
		a map[string]any
	}{
		{"erase", map[string]any{"mode": "chip", "confirm": tokEraseChip, "backup": "x"}},
		{"erase", map[string]any{"mode": "region", "address": "0x0", "length": 4096.0, "confirm": tokEraseReg, "backup": "x"}},
		{"program", map[string]any{"path": img, "address": "0x0", "confirm": tokProgram, "backup": "x"}},
		{"backup", map[string]any{"dir": t.TempDir()}},
	} {
		_, err := h.try("FlashProgramming", c.m, c.a)
		if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), unsupportedHW) {
			t.Fatalf("%s must be refused as %s: %v", c.m, unsupportedHW, err)
		}
	}
	if after := h.read(t, 0, 256); !bytes.Equal(before, after) {
		t.Fatal("flash content changed")
	}
}

func TestHWSWDClocks(t *testing.T) {
	h := hwOpen(t, nil)
	prev := h.call(t, "TargetIdentify", "identify", nil)["debug_port"].(map[string]any)["clock_khz"]
	for _, khz := range []float64{4000, 1800, 950, 480, 240, 1800} {
		r, err := h.try("DebugProbe", "set_speed", map[string]any{"khz": khz})
		if err != nil {
			t.Logf("%v kHz: %v", khz, err)
			continue
		}
		if h.read(t, 0x20000000, 16) == nil {
			t.Fatal("read")
		}
		t.Logf("requested %v actual %v", khz, r["actual_khz"])
	}
	t.Logf("opened at %v kHz", prev)
}

func TestHWThroughput(t *testing.T) {
	h := hwOpen(t, nil)
	h.halt(t)
	timeIt := func(name string, n int, f func()) {
		const reps = 5
		s := time.Now()
		for i := 0; i < reps; i++ {
			f()
		}
		d := time.Since(s) / reps
		rate := ""
		if n > 0 {
			rate = fmt.Sprintf(" (%.1f KB/s)", float64(n)/1024/d.Seconds())
		}
		t.Logf("THROUGHPUT %-22s %v%s", name, d.Round(10*time.Microsecond), rate)
	}
	timeIt("register block", 0, func() { h.call(t, "DebugProbe", "read_registers", map[string]any{"keep_halted": true}) })
	timeIt("read 16 B", 16, func() { h.read(t, 0x20000000, 16) })
	timeIt("read 2 KiB", 2048, func() { h.call(t, "MemoryAccess", "read", map[string]any{"address": "0x20000000", "length": 2048.0}) })
	path := t.TempDir() + "/r.bin"
	n := 0
	timeIt("read 64 KiB (flash)", 65536, func() {
		n++
		h.call(t, "MemoryAccess", "read", map[string]any{"address": "0x08000000", "length": 65536.0, "path": fmt.Sprintf("%s.%d", path, n)})
	})
	base := h.scratch(t, 4096)
	orig := h.read(t, base, 4096)
	pat := bytes.Repeat([]byte{0xA5, 0x5A, 0x3C, 0xC3}, 1024)
	timeIt("write 4 KiB SRAM", 4096, func() { h.write(t, base, pat) })
	h.write(t, base, orig)
	if !bytes.Equal(h.read(t, base, 4096), orig) {
		t.Fatal("SRAM not restored")
	}
}
