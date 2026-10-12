// Package targetaccept is a backend-neutral acceptance run for a Cortex-M
// target behind one of hwlab's debug-probe plugins. It only speaks the plugins'
// DebugProbe and MemoryAccess capabilities through an [Invoker], so the same
// run drives the native XDS110 backend, the CMSIS-DAP backend and the
// simulators, and its [Report]s can be compared across backends.
//
// The run is destructive only in ways it undoes: every memory or register it
// changes is saved first and restored before the next phase, breakpoints are
// cleared, and flash is never written. Nothing here is evidence by itself; a
// result counts as real-hardware evidence only when the Invoker is wired to a
// real probe.
package targetaccept

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Invoker calls one plugin method (capability, method, args) and returns the
// decoded result.
type Invoker func(capability, method string, args map[string]any) (map[string]any, error)

// Check statuses.
const (
	Pass          = "PASS"
	Fail          = "FAIL"
	Info          = "INFO" // an observation, not a pass/fail criterion
	Skip          = "SKIP"
	UnsupportedHW = "UNSUPPORTED_BY_ATTACHED_HW"
)

// Check is one verified (or observed) statement.
type Check struct {
	Phase  string `json:"phase"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Report is the outcome of one [Run].
type Report struct {
	Backend string  `json:"backend"`
	Checks  []Check `json:"checks"`
	// Facts are values that must agree between backends on the same target.
	// Volatile values (PCs, timings, live register contents) never go here.
	Facts map[string]string `json:"facts"`
	// PerfMS holds median wall times in milliseconds per operation.
	PerfMS map[string]float64 `json:"perf_ms"`
	// Raw keeps selected observations for the log.
	Raw map[string]any `json:"raw,omitempty"`
}

// Failed lists the failed checks.
func (r *Report) Failed() []Check {
	var out []Check
	for _, c := range r.Checks {
		if c.Status == Fail {
			out = append(out, c)
		}
	}
	return out
}

// Count returns how many checks have the status.
func (r *Report) Count(status string) int {
	n := 0
	for _, c := range r.Checks {
		if c.Status == status {
			n++
		}
	}
	return n
}

// Find returns the first check with the name, or a zero Check.
func (r *Report) Find(name string) Check {
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	return Check{}
}

// Options tunes a run.
type Options struct {
	Backend string
	Log     func(format string, args ...any)
	// ClockHz is passed to connect (0 = the plugin default).
	ClockHz float64
	// Steps is the number of single steps (default 100).
	Steps int
	// BreakpointRounds is the number of set/hit/clear rounds (default 5).
	BreakpointRounds int
	// Reset enables the reset phase.
	Reset bool
	// ResetVectorBreakpoint also breaks on the reset handler through a
	// system reset; whether the BPU survives a reset is recorded, not required.
	ResetVectorBreakpoint bool
	// DebugGatedAfterReset says the target's boot code re-gates the MEM-AP
	// after a reset (a CC2340 does): a failed reconnect is then recorded as
	// information and Recover is called instead of failing the run.
	DebugGatedAfterReset bool
	// Recover brings the target back to a debuggable state (a power cycle);
	// it is called after a reset left the debug access down.
	Recover func() error
	// PerfRepeats is the number of timed repetitions per operation (default 15).
	PerfRepeats int
	// SkipROMTable skips the ROM table walk (for simulators that do not model one).
	SkipROMTable bool
	// Phases limits the run to the named phases (sram, registers, stepping,
	// breakpoints, watchpoints, perf, reset); empty runs all of them. The
	// identity and discovery phases always run.
	Phases []string
	// ScratchAddr overrides the SRAM scratch window start (default: chosen
	// from the initial stack pointer so that it straddles a 1 KiB boundary).
	ScratchAddr uint32
}

type runner struct {
	inv Invoker
	o   Options
	rep *Report
}

func (r *runner) logf(f string, a ...any) {
	if r.o.Log != nil {
		r.o.Log(f, a...)
	}
}

func (r *runner) add(phase, name, st, detail string, a ...any) {
	if len(a) > 0 {
		detail = fmt.Sprintf(detail, a...)
	}
	r.rep.Checks = append(r.rep.Checks, Check{Phase: phase, Name: name, Status: st, Detail: detail})
	r.logf("%-10s %-52s %-28s %s", phase, name, st, detail)
}

func (r *runner) ok(phase, name string, cond bool, detail string, a ...any) bool {
	st := Pass
	if !cond {
		st = Fail
	}
	r.add(phase, name, st, detail, a...)
	return cond
}

func (r *runner) fact(k, v string) { r.rep.Facts[k] = v }

func (r *runner) dp(method string, args map[string]any) (map[string]any, error) {
	return r.inv("DebugProbe", method, args)
}

func (r *runner) mem(method string, args map[string]any) (map[string]any, error) {
	return r.inv("MemoryAccess", method, args)
}

func hexw(v uint32) string { return fmt.Sprintf("0x%08x", v) }

func parseU32(v any) (uint32, bool) {
	switch x := v.(type) {
	case string:
		n, err := strconv.ParseUint(strings.TrimPrefix(x, "0x"), 16, 32)
		return uint32(n), err == nil
	case float64:
		return uint32(x), x >= 0 && x <= math.MaxUint32
	}
	return 0, false
}

func isCode(err error, c codes.Code) bool {
	st, ok := status.FromError(err)
	return ok && st.Code() == c
}

func unsupportedHW(err error) bool {
	return isCode(err, codes.FailedPrecondition) && strings.Contains(err.Error(), UnsupportedHW)
}

// class names an error independently of its wording, for cross-backend facts.
func class(err error) string {
	switch {
	case err == nil:
		return "ok"
	case unsupportedHW(err):
		return UnsupportedHW
	}
	if st, ok := status.FromError(err); ok {
		return st.Code().String()
	}
	return "error"
}

func (r *runner) read(addr uint32, n int) ([]byte, error) {
	var out []byte
	for n > 0 {
		c := n
		if c > 4096 {
			c = 4096
		}
		res, err := r.mem("read", map[string]any{"address": hexw(addr), "length": float64(c)})
		if err != nil {
			return nil, err
		}
		h, _ := res["hex"].(string)
		b, err := hex.DecodeString(h)
		if err != nil {
			return nil, fmt.Errorf("read 0x%08x+%d: got %d bytes (%w)", addr, c, len(b), err)
		}
		if len(b) != c {
			return nil, fmt.Errorf("read 0x%08x+%d: got %d bytes", addr, c, len(b))
		}
		out = append(out, b...)
		addr += uint32(c)
		n -= c
	}
	return out, nil
}

func (r *runner) write(addr uint32, data []byte) error {
	_, err := r.mem("write", map[string]any{"address": hexw(addr), "data": hex.EncodeToString(data), "verify": true})
	return err
}

func (r *runner) regs() (map[string]uint32, error) {
	res, err := r.dp("read_registers", nil)
	if err != nil {
		return nil, err
	}
	m, _ := res["registers"].(map[string]any)
	out := map[string]uint32{}
	for k, v := range m {
		if n, ok := parseU32(v); ok {
			out[k] = n
		}
	}
	return out, nil
}

func diffRegs(a, b map[string]uint32) []string {
	var d []string
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			d = append(d, fmt.Sprintf("%s %08x!=%08x", k, v, w))
		}
	}
	sort.Strings(d)
	return d
}

func median(d []time.Duration) float64 {
	if len(d) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return float64(s[len(s)/2].Microseconds()) / 1000
}

// Run executes every phase and returns the report. It stops early only when
// the target cannot be reached at all.
func Run(inv Invoker, o Options) *Report {
	if o.Steps <= 0 {
		o.Steps = 100
	}
	if o.BreakpointRounds <= 0 {
		o.BreakpointRounds = 5
	}
	if o.PerfRepeats <= 0 {
		o.PerfRepeats = 15
	}
	r := &runner{inv: inv, o: o, rep: &Report{Backend: o.Backend, Facts: map[string]string{}, PerfMS: map[string]float64{}, Raw: map[string]any{}}}
	defer func() {
		_, _ = r.dp("clear_breakpoint", map[string]any{"all": true})
		_, _ = r.dp("clear_watchpoint", map[string]any{"all": true})
		_, _ = r.dp("resume", nil)
	}()
	if !r.identify() {
		return r.rep
	}
	r.discover()
	want := func(p string) bool {
		if len(o.Phases) == 0 {
			return true
		}
		for _, w := range o.Phases {
			if w == p {
				return true
			}
		}
		return false
	}
	for _, ph := range []struct {
		name string
		fn   func()
	}{
		{"sram", r.sram}, {"registers", r.registers}, {"stepping", r.stepping},
		{"breakpoints", r.breakpoints}, {"watchpoints", r.watchpoints}, {"perf", r.perf},
		{"reset", func() {
			if o.Reset {
				r.resets()
			}
		}},
	} {
		if want(ph.name) {
			ph.fn()
		}
	}
	return r.rep
}

func (r *runner) connectArgs() map[string]any {
	a := map[string]any{}
	if r.o.ClockHz > 0 {
		a["clock_hz"] = r.o.ClockHz
	}
	return a
}

func (r *runner) identify() bool {
	const ph = "identify"
	c, err := r.dp("connect", r.connectArgs())
	if !r.ok(ph, "connect", err == nil, "%v", err) {
		return false
	}
	for _, k := range []string{"dpidr", "ap_idr", "cpuid", "core", "architecture", "revision"} {
		r.fact(k, fmt.Sprint(c[k]))
	}
	r.ok(ph, "core is identified", c["core"] != "" && c["core"] != nil, "core %v arch %v %v cpuid %v", c["core"], c["architecture"], c["revision"], c["cpuid"])
	if r.o.SkipROMTable {
		return true
	}
	aps, err := r.dp("ap_scan", nil)
	if !r.ok(ph, "ap_scan", err == nil, "%v", err) {
		return true
	}
	var comps []string
	rom, _ := aps["rom_table"].([]any)
	for _, e := range rom {
		m, _ := e.(map[string]any)
		if m["error"] != nil {
			comps = append(comps, fmt.Sprint("error:", m["error"]))
			continue
		}
		comps = append(comps, fmt.Sprintf("%v:%v:%v", m["address"], m["part"], m["name"]))
	}
	r.fact("rom_table", strings.Join(comps, ","))
	r.ok(ph, "ROM table lists components", len(comps) > 0, "%d components: %s", len(comps), strings.Join(comps, " "))
	list, _ := aps["aps"].([]any)
	r.fact("ap_count", fmt.Sprint(len(list)))
	return true
}

func (r *runner) discover() {
	const ph = "discover"
	lb, err := r.dp("list_breakpoints", nil)
	if err != nil {
		r.add(ph, "list_breakpoints", Fail, "%v", err)
	} else {
		present := lb["present"] != false
		r.fact("bp.present", fmt.Sprint(present))
		r.fact("bp.slots", fmt.Sprint(lb["slots_total"]))
		r.fact("bp.revision", fmt.Sprint(lb["fpb_revision"]))
		r.add(ph, "breakpoint resources", Info, "present=%v slots=%v fpb_revision=%v", present, lb["slots_total"], lb["fpb_revision"])
	}
	lw, err := r.dp("list_watchpoints", nil)
	if err != nil {
		r.add(ph, "list_watchpoints", Fail, "%v", err)
	} else {
		present := lw["present"] != false
		r.fact("wp.present", fmt.Sprint(present))
		r.fact("wp.comparators", fmt.Sprint(lw["comparators_total"]))
		r.add(ph, "watchpoint resources", Info, "present=%v comparators=%v", present, lw["comparators_total"])
	}
	if b, err := r.read(0, 32); err == nil {
		r.fact("flash_0_32", hex.EncodeToString(b))
	} else {
		r.add(ph, "read vector table", Fail, "%v", err)
	}
}

// scratchWindow picks 256 bytes of SRAM centred on a 1 KiB boundary, away from
// the vector table, the stack top and the bottom of RAM.
func (r *runner) scratchWindow() (uint32, error) {
	if r.o.ScratchAddr != 0 {
		return r.o.ScratchAddr, nil
	}
	w, err := r.read(0, 4)
	if err != nil {
		return 0, err
	}
	sp := uint32(w[0]) | uint32(w[1])<<8 | uint32(w[2])<<16 | uint32(w[3])<<24
	const base = 0x20000000
	if sp <= base+0x2000 || sp > base+0x10000000 {
		return 0, fmt.Errorf("initial SP 0x%08x does not describe an SRAM top; pass ScratchAddr", sp)
	}
	mid := base + (sp-base)/2
	boundary := mid &^ 0x3FF
	return boundary - 0x80, nil
}

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7) + seed
	}
	return b
}

func (r *runner) sram() {
	const ph = "sram"
	if _, err := r.dp("halt", nil); !r.ok(ph, "halt", err == nil, "%v", err) {
		return
	}
	addr, err := r.scratchWindow()
	if !r.ok(ph, "scratch window chosen", err == nil, "%v", err) {
		return
	}
	const guard, size = 16, 256
	lo := addr - guard
	orig, err := r.read(lo, size+2*guard)
	if !r.ok(ph, "save original", err == nil, "window 0x%08x+%d: %v", addr, size, err) {
		return
	}
	r.add(ph, "scratch window", Info, "0x%08x..0x%08x straddles %#x", addr, addr+size, (addr+size/2)&^0x3FF)
	want := append([]byte(nil), orig...)
	defer func() {
		if err := r.write(lo, orig); err != nil {
			r.add(ph, "restore write", Fail, "%v", err)
		}
		back, err := r.read(lo, len(orig))
		r.ok(ph, "restoration exact", err == nil && string(back) == string(orig), "%d bytes incl. %d-byte guards, err %v", len(orig), guard, err)
	}()
	check := func(name string, off int, data []byte) {
		copy(want[guard+off:], data)
		if err := r.write(addr+uint32(off), data); err != nil {
			r.add(ph, name, Fail, "write: %v", err)
			return
		}
		got, err := r.read(lo, len(want))
		r.ok(ph, name, err == nil && string(got) == string(want), "%d B at +0x%x; window and guards match the expected image (err %v)", len(data), off, err)
	}
	check("bulk 256 B pattern", 0, pattern(size, 0x5A))
	check("8-bit write", 1, []byte{0xC3})
	check("8-bit write, next byte", 2, []byte{0x3C})
	check("16-bit aligned write", 0x10, []byte{0xBE, 0xEF})
	check("16-bit write, upper half-word", 0x12, []byte{0xCA, 0xFE})
	check("32-bit aligned write", 0x20, []byte{0x78, 0x56, 0x34, 0x12})
	check("32-bit write at the 1 KiB boundary", size/2, []byte{0xEF, 0xBE, 0xAD, 0xDE})
	check("16-bit write ending at the boundary", size/2-2, []byte{0x11, 0x22})
	check("16-bit write starting at the boundary", size/2+4, []byte{0x33, 0x44})
	check("unaligned 3-byte write", 0x41, []byte{0xA1, 0xB2, 0xC3})
	check("6-byte write across a word pair", 0x7E, []byte{1, 2, 3, 4, 5, 6})
	check("1 byte at window start", 0, []byte{0x99})
	check("1 byte at window end", size-1, []byte{0x66})
	check("all-zero pattern", 0x90, make([]byte, 32))
	check("all-ones pattern", 0xB0, []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})
	// A write the target refuses must fail loudly and leave the link usable.
	_, err = r.mem("write", map[string]any{"address": "0x90000000", "data": "deadbeef"})
	r.ok(ph, "write to an unmapped address is refused", err != nil, "error class %s", class(err))
	_, err = r.mem("read", map[string]any{"address": "0x90000000", "length": 4.0})
	r.ok(ph, "read of an unmapped address is refused", err != nil, "error class %s", class(err))
	got, err := r.read(lo, len(want))
	r.ok(ph, "link recovers after a bus fault", err == nil && string(got) == string(want), "err %v", err)
	_, err = r.mem("write", map[string]any{"address": hexw(addr), "data": ""})
	r.ok(ph, "empty write is refused", isCode(err, codes.InvalidArgument), "error class %s", class(err))
	_, err = r.mem("write", map[string]any{"address": hexw(addr), "data": "abc"})
	r.ok(ph, "odd-length hex is refused", isCode(err, codes.InvalidArgument), "error class %s", class(err))
	_, err = r.mem("write", map[string]any{"address": hexw(addr), "data": "zz"})
	r.ok(ph, "non-hex data is refused", isCode(err, codes.InvalidArgument), "error class %s", class(err))
	_, err = r.mem("write", map[string]any{"address": hexw(addr), "data": strings.Repeat("00", 4097)})
	r.ok(ph, "oversize write is refused", isCode(err, codes.InvalidArgument), "error class %s", class(err))
	_, err = r.mem("read", map[string]any{"address": hexw(addr), "length": 0.0})
	r.ok(ph, "zero-length read is refused", isCode(err, codes.InvalidArgument), "error class %s", class(err))
	_, err = r.mem("read", map[string]any{"address": "0xFFFFFFFC", "length": 8.0})
	r.ok(ph, "read wrapping the address space is refused", err != nil, "error class %s", class(err))
	got, err = r.read(lo, len(want))
	r.ok(ph, "window untouched by refused accesses", err == nil && string(got) == string(want), "err %v", err)
	r.fact("sram.scratch", hexw(addr))
}

var gprs = []string{"r0", "r1", "r2", "r3", "r4", "r5", "r6", "r7", "r8", "r9", "r10", "r11", "r12", "lr"}

func (r *runner) registers() {
	const ph = "registers"
	if _, err := r.dp("resume", nil); !r.ok(ph, "resume for the running-state check", err == nil, "%v", err) {
		return
	}
	_, err := r.dp("write_register", map[string]any{"name": "r0", "value": "0x1"})
	r.ok(ph, "register write while running is refused", err != nil, "error class %s", class(err))
	if _, err := r.dp("halt", nil); !r.ok(ph, "halt", err == nil, "%v", err) {
		return
	}
	orig, err := r.regs()
	if !r.ok(ph, "read all", err == nil && len(orig) >= 17, "%d registers, err %v", len(orig), err) {
		return
	}
	names := make([]string, 0, len(orig))
	for k := range orig {
		names = append(names, k)
	}
	sort.Strings(names)
	r.fact("registers.names", strings.Join(names, ","))
	r.fact("registers.msp.in_sram", fmt.Sprint(orig["msp"]>>28 == 2))
	defer func() {
		for _, k := range gprs {
			if _, err := r.dp("write_register", map[string]any{"name": k, "value": hexw(orig[k])}); err != nil {
				r.add(ph, "restore "+k, Fail, "%v", err)
			}
		}
		now, err := r.regs()
		d := diffRegs(orig, now)
		r.ok(ph, "restoration exact incl. PC/SP/xPSR", err == nil && len(d) == 0, "differences %v err %v", d, err)
	}()
	known := map[string]uint32{"r0": 0xA5A50001, "r4": 0x5A5A0004, "r8": 0xC3C30008, "r12": 0x3C3C000C, "lr": 0x11223345}
	for _, k := range []string{"r0", "r4", "r8", "r12", "lr"} {
		res, err := r.dp("write_register", map[string]any{"name": k, "value": hexw(known[k])})
		if !r.ok(ph, "write "+k, err == nil, "%v", err) {
			continue
		}
		v, _ := parseU32(res["value"])
		now, err := r.regs()
		r.ok(ph, "readback "+k, err == nil && v == known[k] && now[k] == known[k], "wrote %08x read %08x (err %v)", known[k], now[k], err)
		others := map[string]uint32{}
		for n, x := range orig {
			if n != k {
				others[n] = x
			}
		}
		d := diffRegs(others, now)
		r.ok(ph, "no other register changed by "+k, err == nil && len(d) == 0, "differences %v", d)
		if _, err := r.dp("write_register", map[string]any{"name": k, "value": hexw(orig[k])}); err != nil {
			r.add(ph, "restore "+k, Fail, "%v", err)
		}
	}
	_, err = r.dp("write_register", map[string]any{"name": "nosuchreg", "value": "0x1"})
	r.ok(ph, "unknown register is refused", isCode(err, codes.InvalidArgument), "error class %s", class(err))
}

func (r *runner) stepping() {
	const ph = "step"
	n := r.o.Steps
	if _, err := r.dp("halt", nil); !r.ok(ph, "halt", err == nil, "%v", err) {
		return
	}
	deltas := map[string]int{}
	var prev uint32
	havePrev := false
	bad, cont := 0, 0
	for i := 0; i < n; i++ {
		res, err := r.dp("step", map[string]any{"count": 1.0, "mask_interrupts": true})
		if err != nil {
			bad++
			r.add(ph, fmt.Sprintf("step %d", i+1), Fail, "%v", err)
			break
		}
		first, _ := parseU32(res["pc_first"])
		after, _ := parseU32(res["pc"])
		if res["all_halted"] != true || res["halted"] != true {
			bad++
		}
		if havePrev && first != prev {
			cont++
		}
		prev, havePrev = after, true
		switch int64(after) - int64(first) {
		case 2:
			deltas["+2"]++
		case 4:
			deltas["+4"]++
		case 0:
			deltas["same"]++
		default:
			deltas["branch"]++
		}
	}
	st, err := r.dp("status", nil)
	pc, _ := parseU32(st["pc"])
	r.ok(ph, fmt.Sprintf("%d single steps each end halted", n), bad == 0, "%d bad steps; PC deltas %v", bad, deltas)
	r.ok(ph, "PC chains from step to step", cont == 0, "%d discontinuities (a core that changed PC while halted would show here)", cont)
	r.ok(ph, "status after the steps: halted at the last PC", err == nil && st["halted"] == true && pc == prev, "status pc %08x last step pc %08x err %v", pc, prev, err)
	r.ok(ph, "the PC moves", deltas["+2"]+deltas["+4"]+deltas["branch"] > 0, "%v", deltas)
	r.rep.Raw["step_deltas"] = deltas
	r.add(ph, "instruction widths seen", Info, "+2:%d +4:%d branch/other:%d same:%d (not every instruction is 32-bit)", deltas["+2"], deltas["+4"], deltas["branch"], deltas["same"])
	res, err := r.dp("step", map[string]any{"count": float64(n), "mask_interrupts": true})
	r.ok(ph, fmt.Sprintf("one call of %d steps", n), err == nil && res["all_halted"] == true && res["steps"] == float64(n), "pc_changed %v err %v", res["pc_changed"], err)
	// With interrupts unmasked an exception may take the PC anywhere; only the
	// halted state is required.
	res, err = r.dp("step", map[string]any{"count": 20.0})
	r.ok(ph, "20 steps with interrupts unmasked", err == nil && res["all_halted"] == true, "pc_changed %v err %v", res["pc_changed"], err)
	_, err = r.dp("resume", nil)
	r.ok(ph, "resume normally", err == nil, "%v", err)
	time.Sleep(50 * time.Millisecond)
	st, err = r.dp("status", nil)
	r.ok(ph, "running after resume", err == nil && st["halted"] == false, "halted=%v err %v", st["halted"], err)
	if _, err := r.dp("halt", nil); err != nil {
		r.add(ph, "halt after run", Fail, "%v", err)
	}
}

// fpbSnapshot reads FP_CTRL and the first four FP_COMPn at their
// architectural addresses (FP_REMAP in between is skipped).
func (r *runner) fpbSnapshot() (string, error) {
	c, err := r.read(0xE0002000, 4)
	if err != nil {
		return "", err
	}
	b, err := r.read(0xE0002008, 4*4)
	return hex.EncodeToString(c) + "/" + hex.EncodeToString(b), err
}

func (r *runner) breakpoints() {
	const ph = "breakpoint"
	lb, err := r.dp("list_breakpoints", nil)
	if err != nil || lb["present"] == false {
		if err != nil && unsupportedHW(err) || lb["present"] == false {
			r.add(ph, "hardware breakpoints", UnsupportedHW, "the attached core exposes no breakpoint unit")
			return
		}
		r.add(ph, "list_breakpoints", Fail, "%v", err)
		return
	}
	slots, _ := lb["slots_total"].(float64)
	r.ok(ph, "slot count is reported", slots >= 1, "slots %v fpb_revision %v", slots, lb["fpb_revision"])
	snap0, _ := r.fpbSnapshot()
	if _, err := r.dp("halt", nil); !r.ok(ph, "halt", err == nil, "%v", err) {
		return
	}
	rewind := []string{"r0", "r1", "r2", "r3", "r4", "r5", "r6", "r7", "r8", "r9", "r10", "r11", "r12", "lr", "msp", "psp", "special", "xpsr", "pc"}
	hits := 0
	for round := 1; round <= r.o.BreakpointRounds; round++ {
		saved, err := r.regs()
		if err != nil {
			r.add(ph, fmt.Sprintf("round %d save", round), Fail, "%v", err)
			return
		}
		// Pick a target by stepping 6 instructions ahead, then rewind the
		// registers so the core re-executes the same path up to it.
		res, err := r.dp("step", map[string]any{"count": 6.0, "mask_interrupts": true})
		if err != nil {
			r.add(ph, fmt.Sprintf("round %d step", round), Fail, "%v", err)
			return
		}
		target, _ := parseU32(res["pc"])
		for _, k := range rewind {
			if _, err := r.dp("write_register", map[string]any{"name": k, "value": hexw(saved[k])}); err != nil {
				r.add(ph, fmt.Sprintf("round %d rewind %s", round, k), Fail, "%v", err)
				return
			}
		}
		now, _ := r.regs()
		if d := diffRegs(saved, now); len(d) != 0 {
			r.add(ph, fmt.Sprintf("round %d rewind", round), Fail, "registers differ: %v", d)
			return
		}
		bp, err := r.dp("set_breakpoint", map[string]any{"address": hexw(target), "run_wait_ms": 1500.0})
		if err != nil {
			r.add(ph, fmt.Sprintf("round %d set+run", round), Fail, "%v", err)
			return
		}
		hpc, _ := parseU32(bp["halted_pc"])
		hit := bp["hit"] == true && hpc == target
		if hit {
			hits++
		}
		r.ok(ph, fmt.Sprintf("round %d: breakpoint at 0x%08x hit", round, target), hit, "hit=%v halted_pc=%v cause=%v", bp["hit"], bp["halted_pc"], bp["cause"])
		if !hit {
			_, _ = r.dp("halt", nil)
		}
		lst, _ := r.dp("list_breakpoints", nil)
		r.ok(ph, fmt.Sprintf("round %d: one slot in use", round), lst["slots_in_use"] == 1.0, "slots_in_use %v", lst["slots_in_use"])
		if _, err := r.dp("clear_breakpoint", map[string]any{"address": hexw(target)}); err != nil {
			r.add(ph, fmt.Sprintf("round %d clear", round), Fail, "%v", err)
		}
		snap, err := r.fpbSnapshot()
		r.ok(ph, fmt.Sprintf("round %d: BPU registers restored exactly", round), err == nil && snap == snap0, "before %s after %s", snap0, snap)
		if _, err := r.dp("resume", nil); err != nil {
			r.add(ph, fmt.Sprintf("round %d resume", round), Fail, "%v", err)
		}
		time.Sleep(30 * time.Millisecond)
		if _, err := r.dp("halt", nil); err != nil {
			r.add(ph, fmt.Sprintf("round %d halt", round), Fail, "%v", err)
			return
		}
	}
	r.ok(ph, "every round hit its breakpoint", hits == r.o.BreakpointRounds, "%d of %d", hits, r.o.BreakpointRounds)
	// Fill every comparator with an address nothing executes, then one more.
	var set []uint32
	for i := 0; i < int(slots); i++ {
		a := uint32(0x1FFFF000 + 4*i)
		if _, err := r.dp("set_breakpoint", map[string]any{"address": hexw(a)}); err != nil {
			r.add(ph, fmt.Sprintf("fill slot %d", i), Fail, "%v", err)
			break
		}
		set = append(set, a)
	}
	r.ok(ph, fmt.Sprintf("all %v comparators can be used", slots), len(set) == int(slots), "%d set", len(set))
	_, err = r.dp("set_breakpoint", map[string]any{"address": "0x1FFFE000"})
	r.ok(ph, "one more than the hardware has is refused", isCode(err, codes.ResourceExhausted), "error class %s", class(err))
	_, err = r.dp("set_breakpoint", map[string]any{"address": "0x20000100"})
	r.ok(ph, "an address outside the Code region is refused", err != nil, "error class %s", class(err))
	if _, err := r.dp("clear_breakpoint", map[string]any{"all": true}); err != nil {
		r.add(ph, "clear all", Fail, "%v", err)
	}
	snap, err := r.fpbSnapshot()
	r.ok(ph, "BPU registers restored exactly after a full fill", err == nil && snap == snap0, "before %s after %s", snap0, snap)
	r.fact("bp.rounds_hit", fmt.Sprintf("%d/%d", hits, r.o.BreakpointRounds))
}

func (r *runner) resetVectorBreakpoint() {
	const ph = "breakpoint"
	w, err := r.read(4, 4)
	if err != nil {
		r.add(ph, "reset vector", Fail, "%v", err)
		return
	}
	vec := (uint32(w[0]) | uint32(w[1])<<8 | uint32(w[2])<<16 | uint32(w[3])<<24) &^ 1
	if _, err := r.dp("halt", nil); err != nil {
		r.add(ph, "halt", Fail, "%v", err)
		return
	}
	if _, err := r.dp("set_breakpoint", map[string]any{"address": hexw(vec)}); err != nil {
		r.add(ph, "reset-handler breakpoint", Fail, "%v", err)
		return
	}
	defer func() { _, _ = r.dp("clear_breakpoint", map[string]any{"all": true}) }()
	if _, err := r.dp("resume", nil); err != nil {
		r.add(ph, "resume", Fail, "%v", err)
		return
	}
	_, rerr := r.dp("reset", map[string]any{"kind": "sysreset"})
	wh, err := r.dp("wait_halt", map[string]any{"wait_ms": 2000.0})
	if err != nil {
		r.add(ph, "reset-handler breakpoint through sysreset", Info, "no halt after the reset (reset %s, wait %s): the BPU does not survive this reset, or the reset dropped debug", class(rerr), class(err))
		r.fact("bp.survives_sysreset", "false")
		return
	}
	pc, _ := parseU32(wh["pc"])
	hit := wh["breakpoint_hit"] == true && pc == vec
	r.ok(ph, "reset-handler breakpoint hit through sysreset", hit, "pc %08x want %08x cause %v", pc, vec, wh["cause"])
	r.fact("bp.survives_sysreset", fmt.Sprint(hit))
}

func (r *runner) watchpoints() {
	const ph = "watchpoint"
	lw, err := r.dp("list_watchpoints", nil)
	if err != nil {
		r.add(ph, "list_watchpoints", Fail, "%v", err)
		return
	}
	if lw["present"] != false {
		r.add(ph, "hardware watchpoints", Skip, "DWT reports %v comparators; a triggering write needs target code and is not exercised by this run", lw["comparators_total"])
		return
	}
	r.add(ph, "hardware watchpoints", UnsupportedHW, "%v", lw["reason"])
	addr, err := r.scratchWindow()
	if err != nil {
		r.add(ph, "scratch", Fail, "%v", err)
		return
	}
	before, _ := r.read(addr, 16)
	demcr, _ := r.read(0xE000EDFC, 4)
	_, err = r.dp("set_watchpoint", map[string]any{"address": hexw(addr), "kind": "write", "mask_bits": 2.0})
	r.ok(ph, "set_watchpoint reports UNSUPPORTED_BY_ATTACHED_HW", unsupportedHW(err), "error class %s: %v", class(err), err)
	after, _ := r.read(addr, 16)
	demcr2, _ := r.read(0xE000EDFC, 4)
	r.ok(ph, "SRAM and DEMCR untouched by the refused call", string(before) == string(after) && string(demcr) == string(demcr2), "demcr %x -> %x", demcr, demcr2)
	_, err = r.dp("clear_watchpoint", map[string]any{"all": true})
	r.ok(ph, "clearing with no watchpoint is a clean error", err != nil, "error class %s", class(err))
}

func (r *runner) perf() {
	const ph = "perf"
	if _, err := r.dp("halt", nil); err != nil {
		r.add(ph, "halt", Fail, "%v", err)
		return
	}
	addr, err := r.scratchWindow()
	if err != nil {
		r.add(ph, "scratch", Fail, "%v", err)
		return
	}
	orig, err := r.read(addr, 256)
	if err != nil {
		r.add(ph, "save", Fail, "%v", err)
		return
	}
	defer func() { _ = r.write(addr, orig) }()
	timed := func(name string, f func() error) {
		var ds []time.Duration
		for i := 0; i < r.o.PerfRepeats; i++ {
			t := time.Now()
			if err := f(); err != nil {
				r.add(ph, name, Fail, "%v", err)
				return
			}
			ds = append(ds, time.Since(t))
		}
		r.rep.PerfMS[name] = median(ds)
		r.add(ph, name, Info, "median %.2f ms over %d runs", r.rep.PerfMS[name], len(ds))
	}
	timed("register block read", func() error { _, err := r.dp("read_registers", nil); return err })
	timed("16 B SRAM read", func() error {
		_, err := r.mem("read", map[string]any{"address": hexw(addr), "length": 16.0})
		return err
	})
	timed("2 KiB SRAM read", func() error {
		_, err := r.mem("read", map[string]any{"address": "0x20000000", "length": 2048.0})
		return err
	})
	data := hex.EncodeToString(pattern(256, 0x11))
	timed("256 B SRAM write (verified)", func() error {
		_, err := r.mem("write", map[string]any{"address": hexw(addr), "data": data, "verify": true})
		return err
	})
	timed("256 B SRAM write (unverified)", func() error {
		_, err := r.mem("write", map[string]any{"address": hexw(addr), "data": data, "verify": false})
		return err
	})
	t := time.Now()
	if _, err = r.dp("step", map[string]any{"count": 100.0, "mask_interrupts": true}); err != nil {
		r.add(ph, "100 steps (one call)", Fail, "%v", err)
		return
	}
	r.rep.PerfMS["100 steps (one call)"] = float64(time.Since(t).Microseconds()) / 1000
	r.add(ph, "100 steps (one call)", Info, "%.2f ms", r.rep.PerfMS["100 steps (one call)"])
}

// reconnectAndVerify brings the link back after a reset and checks that the
// same DP and core answer and that memory and registers are usable.
func (r *runner) reconnectAndVerify(label string) {
	const ph = "reset"
	var c map[string]any
	var err error
	connect := func() {
		for i := 0; i < 5; i++ {
			if c, err = r.dp("connect", r.connectArgs()); err == nil {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	connect()
	if err != nil && r.o.DebugGatedAfterReset && r.o.Recover != nil {
		r.fact("reset."+label+".debug_after", "gated")
		r.add(ph, label+": debug access after the reset", Info, "the MEM-AP did not come back (%v); recovering with a target power cycle", err)
		_, _ = r.dp("disconnect", nil)
		if rerr := r.o.Recover(); rerr != nil {
			r.add(ph, label+": recover", Fail, "%v", rerr)
			return
		}
		connect()
	} else if err == nil {
		r.fact("reset."+label+".debug_after", "returned")
	}
	if !r.ok(ph, label+": reconnect", err == nil, "%v", err) {
		return
	}
	r.ok(ph, label+": same DPIDR and CPUID", fmt.Sprint(c["dpidr"]) == r.rep.Facts["dpidr"] && fmt.Sprint(c["cpuid"]) == r.rep.Facts["cpuid"],
		"dpidr %v cpuid %v", c["dpidr"], c["cpuid"])
	b, err := r.read(0, 32)
	r.ok(ph, label+": flash readable and unchanged", err == nil && hex.EncodeToString(b) == r.rep.Facts["flash_0_32"], "err %v", err)
	if _, err := r.dp("halt", nil); err != nil {
		r.add(ph, label+": halt", Fail, "%v", err)
		return
	}
	regs, err := r.regs()
	r.ok(ph, label+": registers readable", err == nil && len(regs) >= 17, "%d regs err %v", len(regs), err)
	_, err = r.dp("resume", nil)
	r.ok(ph, label+": resume", err == nil, "%v", err)
}

func (r *runner) resets() {
	const ph = "reset"
	run := func(label string, args map[string]any, halted bool) {
		if halted {
			_, _ = r.dp("halt", nil)
		} else {
			_, _ = r.dp("resume", nil)
		}
		res, err := r.dp("reset", args)
		kind, _ := args["kind"].(string)
		r.rep.Raw["reset."+label] = fmt.Sprint(class(err), " ", res)
		refused := err != nil && isCode(err, codes.FailedPrecondition) && strings.Contains(err.Error(), "nothing was reset")
		if !refused && !isCode(err, codes.InvalidArgument) { // refusals are backend-specific, not a property of the target
			r.fact("reset."+label, class(err))
		}
		switch {
		case refused:
			r.add(ph, label, Info, "refused without touching the target: %v", err)
		case err == nil:
			r.add(ph, label, Pass, "halted=%v reset_seen=%v debug_kept=%v debug_lost=%v core_changed=%v at_vector=%v", res["halted"], res["reset_seen"], res["debug_kept"], res["debug_lost"], res["core_registers_changed"], res["at_reset_vector"])
			if res["debug_lost"] == true {
				r.fact("reset."+label+".evidence", "debug_lost")
			}
			if args["halt"] == true && res["debug_lost"] != true {
				r.ok(ph, label+": core is halted", res["halted"] == true, "halted=%v", res["halted"])
			}
		case unsupportedHW(err):
			r.add(ph, label, UnsupportedHW, "%v", err)
		case isCode(err, codes.InvalidArgument):
			r.add(ph, label, Info, "kind %q is not offered by this backend: %v", kind, err)
		default:
			r.add(ph, label, Fail, "%v", err)
		}
		if !refused && (err == nil || !isCode(err, codes.InvalidArgument)) {
			r.reconnectAndVerify(label)
		}
	}
	run("sysreset while running", map[string]any{"kind": "sysreset"}, false)
	run("sysreset while halted", map[string]any{"kind": "sysreset"}, true)
	run("sysreset and halt", map[string]any{"kind": "sysreset", "halt": true}, false)
	run("core reset (VECTRESET)", map[string]any{"kind": "core"}, true)
	run("hardware nRESET while running", map[string]any{"kind": "hardware"}, false)
	run("hardware nRESET and halt", map[string]any{"kind": "hardware", "halt": true}, false)
	run("dap_reset", map[string]any{"kind": "dap_reset"}, false)
	if r.o.ResetVectorBreakpoint {
		r.resetVectorBreakpoint()
		r.reconnectAndVerify("reset-handler breakpoint")
	}
}

// Compare lists the differences between two reports of the same target: facts
// that must match, and checks decided differently on the two backends.
// Timings and observations never count as differences.
func Compare(a, b *Report) []string {
	var d []string
	keys := map[string]bool{}
	for k := range a.Facts {
		keys[k] = true
	}
	for k := range b.Facts {
		keys[k] = true
	}
	var ks []string
	for k := range keys {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	for _, k := range ks {
		// Whether DAP_ResetTarget is offered at all is a property of the
		// transport, not of the target.
		if strings.HasPrefix(k, "reset.dap_reset") {
			continue
		}
		if a.Facts[k] != b.Facts[k] {
			d = append(d, fmt.Sprintf("fact %s: %s=%q %s=%q", k, a.Backend, a.Facts[k], b.Backend, b.Facts[k]))
		}
	}
	decided := func(s string) bool { return s == Pass || s == Fail || s == UnsupportedHW }
	names := map[string]string{}
	for _, c := range a.Checks {
		if decided(c.Status) {
			names[c.Phase+"/"+c.Name] = c.Status
		}
	}
	for _, c := range b.Checks {
		if !decided(c.Status) {
			continue
		}
		if s, ok := names[c.Phase+"/"+c.Name]; ok && s != c.Status {
			d = append(d, fmt.Sprintf("check %s/%s: %s=%s %s=%s", c.Phase, c.Name, a.Backend, s, b.Backend, c.Status))
		}
	}
	return d
}

// ErrFailed wraps the error Err returns.
var ErrFailed = errors.New("acceptance checks failed")

// Err summarises the failed checks, or returns nil.
func (r *Report) Err() error {
	f := r.Failed()
	if len(f) == 0 {
		return nil
	}
	var parts []string
	for _, c := range f {
		parts = append(parts, c.Phase+"/"+c.Name+": "+c.Detail)
	}
	return fmt.Errorf("%w (%s): %s", ErrFailed, r.Backend, strings.Join(parts, "; "))
}
