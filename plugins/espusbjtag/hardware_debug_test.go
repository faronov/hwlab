//go:build hardware

package espusbjtag

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
)

// These tests drive a REAL ESP32-C6 through the plugin. They never erase or
// program flash; the memory they write is SRAM that is saved first and put
// back, and every test leaves the CPU running.
//
// HWLAB_ESPJTAG_HW=1 go test -tags hardware -run HWDebug -v ./plugins/espusbjtag/

func (h hw) try(capName, method string, args map[string]any) (map[string]any, error) {
	s, _ := structpb.NewStruct(args)
	res, err := h.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: h.id, Capability: capName, Method: method, Args: s})
	if err != nil {
		return nil, err
	}
	return res.Result.AsMap(), nil
}

// hwOpen opens a persistent session with the given settings and, however the
// test ends, resumes hart 0 and closes the device.
func hwOpen(t *testing.T, cfg map[string]any) hw {
	t.Helper()
	h := hwRig(t)
	if err := h.OpenDevice(context.Background(), h.id, cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = h.try("DebugProbe", "clear_breakpoint", map[string]any{"all": true})
		_, _ = h.try("DebugProbe", "resume", map[string]any{"hart": 0})
		_ = h.CloseDevice(context.Background(), h.id)
	})
	return h
}

func (h hw) halt(t *testing.T) {
	t.Helper()
	if r := h.call(t, "DebugProbe", "halt", map[string]any{"hart": 0}); r["halted"] != true {
		t.Fatalf("halt: %v", r)
	}
}

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
	r := h.call(t, "MemoryAccess", "read", map[string]any{"address": hexAddr(addr), "length": n})
	b, err := hex.DecodeString(r["hex"].(string))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (h hw) write(t *testing.T, addr uint32, b []byte) map[string]any {
	t.Helper()
	return h.call(t, "MemoryAccess", "write", map[string]any{"address": hexAddr(addr), "data": hex.EncodeToString(b)})
}

// scratchSRAM finds a block of n bytes in HP SRAM that reads identically twice
// while the hart is halted, so nothing else is using it right now. The target
// is halted, so no other bus master except DMA could change it.
func (h hw) scratchSRAM(t *testing.T, n int) uint32 {
	t.Helper()
	const base, span = 0x40860000, 0x10000
	a := h.read(t, base, 4096)
	for off := 4096; off < span; off += 4096 {
		a = append(a, h.read(t, base+uint32(off), 4096)...)
	}
	var b []byte
	for off := 0; off < span; off += 4096 {
		b = append(b, h.read(t, base+uint32(off), 4096)...)
	}
	if !bytes.Equal(a, b) {
		t.Skip("SRAM scratch window is not stable while halted")
	}
	run, best, at := 0, 0, 0
	for i, v := range a {
		if v == 0 {
			run++
			if run > best {
				best, at = run, i-run+1
			}
		} else {
			run = 0
		}
	}
	if best < n+16 {
		t.Skipf("no idle %d-byte zero block found (longest %d)", n, best)
	}
	return base + uint32(at+8)&^3
}

func TestHWDebugSRAMWriteRestore(t *testing.T) {
	h := hwOpen(t, map[string]any{"allow_memory_write": true})
	h.halt(t)
	addr := h.scratchSRAM(t, 64)
	t.Logf("scratch SRAM at %s", hexAddr(addr))
	orig := h.read(t, addr-4, 72)

	// 8/16/32-bit and odd-length patterns at every alignment of a word.
	pats := [][]byte{{0xA5}, {0x5A, 0xC3}, {0x01, 0x23, 0x45, 0x67}, {0xDE, 0xAD, 0xBE, 0xEF, 0x55}, {1, 2, 3, 4, 5, 6, 7, 8}}
	for _, p := range pats {
		for off := 0; off < 4; off++ {
			a := addr + uint32(off)
			m := h.write(t, a, p)
			if m["verified"] != true {
				t.Fatalf("%v", m)
			}
			if got := h.read(t, a, len(p)); !bytes.Equal(got, p) {
				t.Fatalf("len %d off %d: %x != %x", len(p), off, got, p)
			}
		}
	}
	// neighbour preservation: byte write inside a word keeps the other bytes
	h.write(t, addr+16, []byte{0x11, 0x22, 0x33, 0x44})
	h.write(t, addr+17, []byte{0xEE})
	if got := h.read(t, addr+16, 4); !bytes.Equal(got, []byte{0x11, 0xEE, 0x33, 0x44}) {
		t.Fatalf("RMW lost neighbours: %x", got)
	}
	// boundary: 4 KiB page edge inside SRAM is never crossed by the scratch area, but a
	// write straddling the end of SRAM must be refused without side effects.
	if _, err := h.try("MemoryAccess", "write", map[string]any{"address": hexAddr(0x4087FFFE), "data": "00112233"}); err == nil {
		t.Fatal("write past the end of SRAM accepted")
	}
	// invalid accesses
	for _, a := range []uint32{0x40000000, 0x42000000, 0x600B0000, 0x50004000} {
		if _, err := h.try("MemoryAccess", "write", map[string]any{"address": hexAddr(a), "data": "00"}); err == nil {
			t.Fatalf("write to %s accepted", hexAddr(a))
		}
	}
	// restore, byte for byte
	h.write(t, addr-4, orig[:4])
	h.write(t, addr, orig[4:68])
	h.write(t, addr+64, orig[68:72])
	if got := h.read(t, addr-4, 72); !bytes.Equal(got, orig) {
		t.Fatalf("restore mismatch:\n got %x\nwant %x", got, orig)
	}
	t.Logf("SRAM write/restore OK: %d bytes restored identically", len(orig))
}

// rvInfo classifies the instruction at pc.
func rvInfo(inst uint32) (n int, controlFlow, system bool) {
	if inst&3 != 3 {
		q, f3 := inst&3, inst>>13&7
		switch {
		case q == 1 && (f3 == 1 || f3 == 5 || f3 == 6 || f3 == 7):
			return 2, true, false
		case q == 2 && f3 == 4 && inst>>2&0x1F == 0: // c.jr / c.jalr / c.ebreak
			return 2, true, false
		}
		return 2, false, false
	}
	switch inst & 0x7F {
	case 0x6F, 0x67, 0x63:
		return 4, true, false
	case 0x73:
		return 4, false, true
	}
	return 4, false, false
}

func TestHWDebugSingleStep100(t *testing.T) {
	h := hwOpen(t, nil)
	h.halt(t)
	regs0 := h.call(t, "DebugProbe", "read_registers", map[string]any{"keep_halted": true})["registers"].(map[string]any)
	dcsr0 := parseHex(t, regs0["dcsr"])
	var deltas = map[int64]int{}
	checked, flow, sys := 0, 0, 0
	for i := 0; i < 100; i++ {
		r := h.call(t, "DebugProbe", "step", nil)
		st := r["steps"].([]any)[0].(map[string]any)
		before, after := parseHex(t, st["pc_before"]), parseHex(t, st["pc_after"])
		if st["cause_name"] != "step" || r["halted"] != true {
			t.Fatalf("step %d: %v", i, r)
		}
		var inst []byte
		if c := before >> 24; c == 0x40 || c == 0x42 || c == 0x40800000>>24 {
			inst = h.read(t, before, 4)
		}
		if inst == nil {
			continue
		}
		w := uint32(inst[0]) | uint32(inst[1])<<8 | uint32(inst[2])<<16 | uint32(inst[3])<<24
		n, cf, sy := rvInfo(w)
		d := int64(int32(after - before))
		deltas[d]++
		switch {
		case sy:
			sys++
		case cf:
			flow++
			if after&1 != 0 {
				t.Fatalf("step %d: odd PC %#x", i, after)
			}
		default:
			if d != int64(n) {
				t.Fatalf("step %d at %#x: instruction %#x length %d but PC moved by %d", i, before, w, n, d)
			}
			checked++
		}
	}
	regs1 := h.call(t, "DebugProbe", "read_registers", map[string]any{"keep_halted": true})["registers"].(map[string]any)
	dcsr1 := parseHex(t, regs1["dcsr"])
	if dcsr1&(1<<2) != 0 {
		t.Fatalf("dcsr.step left set: %#x", dcsr1)
	}
	t.Logf("100 single steps: %d straight-line instructions checked against decoded length, %d control-flow, %d system; PC deltas %v; dcsr before %#x after %#x", checked, flow, sys, deltas, dcsr0, dcsr1)
	if checked < 20 {
		t.Fatalf("too few decodable straight-line steps (%d)", checked)
	}
	// a multi-step call too
	r := h.call(t, "DebugProbe", "step", map[string]any{"count": 50})
	if r["count"] != float64(50) || r["cause_name"] != "step" {
		t.Fatalf("%v", r)
	}
	if r := h.call(t, "DebugProbe", "resume", map[string]any{"hart": 0}); r["resumed"] != true {
		t.Fatalf("%v", r)
	}
	time.Sleep(50 * time.Millisecond)
	if st := h.call(t, "DebugProbe", "chip_info", nil)["hart_state"]; st != "running" {
		t.Fatalf("after a normal resume the hart is %v (step bit must be cleared)", st)
	}
}

type slotState struct{ t1, t2 string }

func slotsOf(m map[string]any) map[int]slotState {
	out := map[int]slotState{}
	for _, s := range m["slots"].([]any) {
		x := s.(map[string]any)
		out[int(x["index"].(float64))] = slotState{x["tdata1"].(string), x["tdata2"].(string)}
	}
	return out
}

func TestHWDebugTriggersAndBreakpoints(t *testing.T) {
	h := hwOpen(t, nil)
	h.halt(t)
	l := h.call(t, "DebugProbe", "list_breakpoints", nil)
	saved := slotsOf(l)
	t.Logf("trigger slots: %v of which free %v; slots=%v", l["trigger_slots"], l["free_slots"], saved)
	if len(saved) == 0 {
		t.Fatal("no trigger slots enumerated")
	}

	// Sample where the CPU spends its time to find addresses that run again.
	var pcs []uint32
	for i := 0; i < 24; i++ {
		h.call(t, "DebugProbe", "resume", map[string]any{"hart": 0})
		time.Sleep(time.Duration(3+i%7) * time.Millisecond)
		h.halt(t)
		r := h.call(t, "DebugProbe", "read_registers", map[string]any{"keep_halted": true})["registers"].(map[string]any)
		pcs = append(pcs, parseHex(t, r["pc"]))
	}
	freq := map[uint32]int{}
	for _, p := range pcs {
		freq[p]++
	}
	var cands []uint32
	for p := range freq {
		cands = append(cands, p)
	}
	sort.Slice(cands, func(i, j int) bool { return freq[cands[i]] > freq[cands[j]] })
	t.Logf("sampled pcs %d distinct: %v", len(cands), freq)

	hits, trivial := 0, 0
	for round := 0; round < 4; round++ {
		h.halt(t)
		cur := parseHex(t, h.call(t, "DebugProbe", "read_registers", map[string]any{"keep_halted": true})["registers"].(map[string]any)["pc"])
		pick := cands[round%len(cands)]
		if pick == cur && len(cands) > 1 {
			pick = cands[(round+1)%len(cands)]
		}
		if esp := pick; esp&1 != 0 {
			pick &^= 1
		}
		h.call(t, "DebugProbe", "set_breakpoint", map[string]any{"address": hexAddr(pick)})
		h.call(t, "DebugProbe", "resume", map[string]any{"hart": 0})
		w, err := h.try("DebugProbe", "wait_halt", map[string]any{"wait_ms": 2000})
		if err != nil {
			t.Logf("round %d: breakpoint at %#x not reached: %v", round, pick, err)
		} else {
			if w["cause_name"] != "trigger" || parseHex(t, w["pc"]) != pick || w["breakpoint_hit"] != true {
				t.Fatalf("round %d: halted for the wrong reason: %v (wanted trigger at %#x)", round, w, pick)
			}
			hits++
			if pick == cur {
				trivial++
			}
			t.Logf("round %d: HIT at %#x cause=%v", round, pick, w["cause_name"])
		}
		h.halt(t) // no-op when it already halted
		h.call(t, "DebugProbe", "clear_breakpoint", map[string]any{"address": hexAddr(pick)})
		now := slotsOf(h.call(t, "DebugProbe", "list_breakpoints", nil))
		for i, s := range saved {
			if now[i] != s {
				t.Fatalf("round %d: slot %d not restored: %v vs %v", round, i, now[i], s)
			}
		}
	}
	if hits == 0 {
		t.Fatal("no breakpoint was ever hit")
	}
	t.Logf("breakpoint hits %d of 4 rounds (%d at the resume pc)", hits, trivial)
}

func TestHWDebugHarts(t *testing.T) {
	h := hwOpen(t, nil)
	m := h.call(t, "DebugProbe", "harts", nil)
	t.Logf("harts: %v", m)
	hs := m["harts"].([]any)
	if len(hs) < 1 {
		t.Fatal("no harts")
	}
	before := h.call(t, "DebugProbe", "chip_info", map[string]any{"hart": 0})["hart_state"]
	for i := 0; i < 6; i++ {
		h.call(t, "DebugProbe", "chip_info", map[string]any{"hart": i % 2})
		if st := h.call(t, "DebugProbe", "chip_info", map[string]any{"hart": 0})["hart_state"]; st != before && before == "running" && st != "running" {
			t.Fatalf("hart 0 state changed by selecting hart %d: %v", i%2, st)
		}
	}
	if len(hs) > 1 {
		if _, err := h.try("DebugProbe", "halt", map[string]any{"hart": 1}); err != nil {
			t.Logf("hart 1 halt: %v", err)
		}
		if st := h.call(t, "DebugProbe", "chip_info", map[string]any{"hart": 0})["hart_state"]; st != "running" {
			t.Fatalf("hart 0 disturbed by a hart 1 halt attempt: %v", st)
		}
	}
}

func TestHWDebugResetKinds(t *testing.T) {
	h := hwOpen(t, map[string]any{"allow_reset": true})
	for _, k := range []string{"hart", "system"} {
		r := h.call(t, "DebugProbe", "reset", map[string]any{"kind": k, "halt": true, "hart": 0})
		if r["reset_observed"] != true || r["halted"] != true {
			t.Fatalf("%s: %v", k, r)
		}
		t.Logf("%s reset halted: pc=%v cause=%v usb_dropped=%v elapsed=%vms", k, r["pc"], r["cause_name"], r["usb_dropped"], r["elapsed_ms"])
	}
	r := h.call(t, "DebugProbe", "reset", map[string]any{"kind": "system", "halt": false, "hart": 0})
	t.Logf("system reset run: %v", r["elapsed_ms"])
	time.Sleep(1500 * time.Millisecond)
	ci := h.call(t, "DebugProbe", "chip_info", nil)
	if ci["hart_state"] != "running" {
		t.Fatalf("after a reset without halt the firmware should run: %v", ci["hart_state"])
	}
}

func TestHWDebugSoCReset(t *testing.T) {
	h := hwOpen(t, map[string]any{"allow_reset": true})
	before := h.call(t, "TargetIdentify", "identify", nil)
	r, err := h.try("DebugProbe", "reset", map[string]any{"kind": "soc", "confirm": "reset-soc", "hart": 0})
	if err != nil {
		t.Fatalf("soc reset: %v", err)
	}
	t.Logf("soc reset: %v", r)
	if r["usb_dropped"] != true || r["identity_verified"] != true {
		t.Fatalf("%v", r)
	}
	after := h.call(t, "TargetIdentify", "identify", nil)
	if fmt.Sprint(before["targets"]) != fmt.Sprint(after["targets"]) {
		t.Fatalf("identity changed: %v vs %v", before["targets"], after["targets"])
	}
	if got := h.read(t, 0x40001000, 4); !bytes.Equal(got, []byte{0x6f, 0x80, 0xe0, 0x2c}) {
		t.Fatalf("ROM word after SoC reset: %x", got)
	}
	// repeat
	if _, err := h.try("DebugProbe", "reset", map[string]any{"kind": "soc", "confirm": "reset-soc"}); err != nil {
		t.Fatalf("second soc reset: %v", err)
	}
}

// A hart parked in ROM after a reset-halt runs boot code that disables the
// watchdog again; the SoC reset must still be observed (re-armed).
func TestHWDebugSoCResetFromHalted(t *testing.T) {
	h := hwOpen(t, map[string]any{"allow_reset": true})
	h.call(t, "DebugProbe", "reset", map[string]any{"kind": "system", "halt": true})
	r, err := h.try("DebugProbe", "reset", map[string]any{"kind": "soc", "confirm": "reset-soc"})
	if err != nil {
		t.Fatalf("soc reset from a halted hart: %v", err)
	}
	if r["usb_dropped"] != true || r["identity_verified"] != true {
		t.Fatalf("%v", r)
	}
	if got := h.read(t, 0x40001000, 4); !bytes.Equal(got, []byte{0x6f, 0x80, 0xe0, 0x2c}) {
		t.Fatalf("ROM word after SoC reset: %x", got)
	}
}

func TestHWDebugReconnect(t *testing.T) {
	h := hwOpen(t, nil)
	h.call(t, "TargetIdentify", "identify", nil)
	for i := 0; i < 3; i++ {
		r := h.call(t, "DebugProbe", "reconnect", nil)
		if r["reconnected"] != true || r["identity_verified"] != true {
			t.Fatalf("%v", r)
		}
		h.read(t, 0x40001000, 4)
	}
}

func TestHWDebugFlashRead(t *testing.T) {
	h := hwOpen(t, nil)
	info := h.call(t, "FlashProgramming", "info", nil)
	fl := info["flash"].(map[string]any)
	t.Logf("flash: %v partitions=%v", fl, info["partitions"])
	size := int(fl["size_bytes"].(float64))
	first := h.call(t, "FlashProgramming", "read", map[string]any{"address": 0, "length": 4096})
	again := h.call(t, "FlashProgramming", "read", map[string]any{"address": 0, "length": 4096})
	if first["sha256"] != again["sha256"] {
		t.Fatal("two reads of the same flash differ")
	}
	if first["hex"].(string)[:2] != "e9" {
		t.Fatalf("flash does not start with the ESP image magic: %v", first["hex"].(string)[:16])
	}
	un := h.call(t, "FlashProgramming", "read", map[string]any{"address": 0x1001, "length": 1000})
	al := h.call(t, "FlashProgramming", "read", map[string]any{"address": 0x1000, "length": 2048})
	if un["hex"].(string) != al["hex"].(string)[2:2+2000] {
		t.Fatal("unaligned read differs from the aligned one")
	}
	// the end of flash and one byte past it
	h.call(t, "FlashProgramming", "read", map[string]any{"address": size - 16, "length": 16})
	if _, err := h.try("FlashProgramming", "read", map[string]any{"address": size - 8, "length": 16}); err == nil {
		t.Fatal("a read past the end of flash succeeded")
	}
	if _, err := h.try("FlashProgramming", "read", map[string]any{"address": 0, "length": 0}); err == nil {
		t.Fatal("zero-length read accepted")
	}
	for _, c := range info["mapped_window_check"].([]any) {
		t.Logf("mapped window check: %v", c)
	}
	if st := h.call(t, "DebugProbe", "chip_info", nil)["hart_state"]; st != "running" {
		t.Fatalf("a read-only flash call must leave the hart running: %v", st)
	}
}

func TestHWDebugPerformance(t *testing.T) {
	h := hwOpen(t, nil)
	h.halt(t)
	timeit := func(name string, n int, f func()) {
		start := time.Now()
		for i := 0; i < n; i++ {
			f()
		}
		d := time.Since(start) / time.Duration(n)
		t.Logf("PERF %-26s %8.2f ms", name, float64(d.Microseconds())/1000)
	}
	timeit("registers (all, halted)", 5, func() { h.call(t, "DebugProbe", "read_registers", map[string]any{"keep_halted": true}) })
	timeit("mem read 16 B", 10, func() { h.read(t, 0x40800000, 16) })
	timeit("mem read 2 KiB", 5, func() { h.read(t, 0x40800000, 2048) })
	path := t.TempDir() + "/m.bin"
	i := 0
	timeit("mem read 64 KiB (to file)", 2, func() {
		i++
		h.call(t, "MemoryAccess", "read", map[string]any{"address": "0x40800000", "length": 65536, "path": fmt.Sprintf("%s.%d", path, i)})
	})
	timeit("flash read 4 KiB", 3, func() { h.call(t, "FlashProgramming", "read", map[string]any{"address": 0, "length": 4096}) })
	fp := t.TempDir() + "/f.bin"
	j := 0
	timeit("flash read 64 KiB (to file)", 2, func() {
		j++
		h.call(t, "FlashProgramming", "read", map[string]any{"address": 0, "length": 65536, "path": fmt.Sprintf("%s.%d", fp, j)})
	})
	_ = os.Remove(path)
}
