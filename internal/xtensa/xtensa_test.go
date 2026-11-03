package xtensa_test

import (
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/faronov/hwlab/internal/jtag"
	"github.com/faronov/hwlab/internal/xtensa"
	"github.com/faronov/hwlab/internal/xtensa/xtsim"
)

var loop = []uint32{0x40041a76, 0x40041a79, 0x40041a7c}

const (
	romBase  = 0x40000000
	dramBase = 0x3FC88000
)

func simCore(prid uint32, held bool) *xtsim.Target {
	rom := make([]byte, 0x2000)
	for i := 0; i < len(rom); i += 4 {
		binary.LittleEndian.PutUint32(rom[i:], 0xC0DE0000|uint32(i))
	}
	return xtsim.New(xtsim.Config{
		IDCode: 0x120034e5, OCDID: 0x0b339fd2, PRID: prid, Held: held, Loop: loop,
		Regions: []xtsim.Region{
			{Base: romBase, Data: rom, ReadOnly: true},
			{Base: dramBase, Data: make([]byte, 0x10000)},
		},
	})
}

type rig struct {
	chain *xtsim.Chain
	port  *xtsim.BatchPort
	drv   *jtag.Driver
	t     *xtsim.Target
	c     *xtensa.Core
}

// newRig builds a two-core chain like the ESP32-S3 in download mode: core 0
// (PRID 0xCDCD) nearest TDO and running, core 1 held in reset.
func newRig(t *testing.T) *rig {
	t.Helper()
	ch := &xtsim.Chain{TAPs: []*xtsim.Target{simCore(0xCDCD, false), simCore(0xABAB, true)}}
	return rigOn(t, ch, 0)
}

func rigOn(t *testing.T, ch *xtsim.Chain, index int) *rig {
	t.Helper()
	p := &xtsim.BatchPort{Port: xtsim.Port{C: ch}}
	d := jtag.NewDriver(p)
	irl := make([]int, len(ch.TAPs))
	for i := range irl {
		irl[i] = xtensa.IRLen
	}
	dm, err := xtensa.NewDM(d, irl, index)
	if err != nil {
		t.Fatal(err)
	}
	c := xtensa.NewCore(dm, 6)
	c.Poll = 0
	c.Timeout = 200 * time.Millisecond
	if _, err := c.Examine(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &rig{chain: ch, port: p, drv: d, t: ch.TAPs[index], c: c}
}

func halted(t *testing.T, r *rig) xtensa.Halt {
	t.Helper()
	h, err := r.c.Halt(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestChainScan(t *testing.T) {
	ch := &xtsim.Chain{TAPs: []*xtsim.Target{simCore(0xCDCD, false), simCore(0xABAB, true)}}
	d := jtag.NewDriver(&xtsim.BatchPort{Port: xtsim.Port{C: ch}})
	c, err := jtag.ScanChain(context.Background(), d, jtag.ScanOptions{MeasureIR: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Devices) != 2 || c.TotalIR != 10 || c.BypassBits != 2 {
		t.Fatalf("chain %+v", c)
	}
	for _, dev := range c.Devices {
		if dev.IDCode == nil || dev.IDCode.Raw != 0x120034e5 {
			t.Fatalf("device %+v", dev)
		}
	}
}

func TestNewDMErrors(t *testing.T) {
	d := jtag.NewDriver(xtsim.Port{C: simCore(0, false)})
	for _, tc := range []struct {
		irl   []int
		index int
	}{{[]int{5}, 1}, {[]int{5}, -1}, {[]int{4}, 0}, {[]int{5, 0}, 0}} {
		if _, err := xtensa.NewDM(d, tc.irl, tc.index); !errors.Is(err, xtensa.ErrChain) {
			t.Errorf("%v/%d: %v", tc.irl, tc.index, err)
		}
	}
}

func TestExamine(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	in, err := r.c.Examine(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if in.OCDID != 0x0b339fd2 || in.DCR&xtensa.DCREnableOCD == 0 || in.DSR&xtensa.DSRStopped != 0 {
		t.Fatalf("info %+v", in)
	}
	if in.PwrCtlPrev != xtensa.PwrOn {
		t.Errorf("second examine saw PWRCTL 0x%x", in.PwrCtlPrev)
	}
	// The held core 1 answers nothing.
	dm1, _ := xtensa.NewDM(r.drv, []int{5, 5}, 1)
	if _, err := xtensa.NewCore(dm1, 6).Examine(ctx); !errors.Is(err, xtensa.ErrNoDebugModule) {
		t.Fatalf("core 1: %v", err)
	}
}

func TestExamineClearsWasReset(t *testing.T) {
	ch := &xtsim.Chain{TAPs: []*xtsim.Target{simCore(0xCDCD, false)}}
	d := jtag.NewDriver(xtsim.Port{C: ch}) // non-batching port path
	dm, _ := xtensa.NewDM(d, []int{5}, 0)
	c := xtensa.NewCore(dm, 6)
	in, err := c.Examine(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if in.PwrStat&xtensa.StatCoreWasReset == 0 || in.PwrStatNow&(xtensa.StatCoreWasReset|xtensa.StatDebugWasReset) != 0 {
		t.Fatalf("pwrstat 0x%x -> 0x%x", in.PwrStat, in.PwrStatNow)
	}
}

func TestHaltResumeRestoresScratch(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.t.SetAR(3, 0x1234abcd)
	h := halted(t, r)
	if h.Already || h.DebugCause&xtensa.CauseDebugInt == 0 || !contains(loop, h.PC) {
		t.Fatalf("halt %+v", h)
	}
	if h.PS&0xF != 0 {
		t.Fatalf("PS 0x%x is not the interrupted program's", h.PS)
	}
	if v, err := r.c.ReadRegister(ctx, "a3"); err != nil || v != 0x1234abcd {
		t.Fatalf("a3 = %#x, %v", v, err)
	}
	if _, err := r.c.ReadWords(ctx, dramBase, 4); err != nil {
		t.Fatal(err)
	}
	if r.t.AR(3) == 0x1234abcd {
		t.Fatal("expected the memory read to use a3 as a pointer")
	}
	h2 := halted(t, r)
	if !h2.Already || h2.PC != h.PC {
		t.Fatalf("second halt %+v", h2)
	}
	if err := r.c.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if r.t.Stopped() || r.t.AR(3) != 0x1234abcd {
		t.Fatalf("after resume stopped=%v a3=%#x", r.t.Stopped(), r.t.AR(3))
	}
	if err := r.c.Resume(ctx); !errors.Is(err, xtensa.ErrNotHalted) {
		t.Fatalf("resume while running: %v", err)
	}
	st, err := r.c.Status(ctx)
	if err != nil || st.Halted {
		t.Fatalf("status %+v %v", st, err)
	}
}

func TestRegisters(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	for i := 0; i < 16; i++ {
		r.t.SetAR(i, 0x100+uint32(i))
	}
	halted(t, r)
	regs, err := r.c.ReadRegisters(ctx, xtensa.CoreRegisters)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]xtensa.RegValue{}
	for _, v := range regs {
		got[v.Name] = v
	}
	for i := 0; i < 16; i++ {
		n := "a" + itoa(i)
		if got[n].Value != 0x100+uint32(i) {
			t.Errorf("%s = %#x", n, got[n].Value)
		}
	}
	if got["prid"].Value != 0xCDCD || !contains(loop, got["pc"].Value) || got["debugcause"].Value != xtensa.CauseDebugInt {
		t.Errorf("regs %+v", got)
	}
	// Unimplemented special registers are reported per register.
	regs, err = r.c.ReadRegisters(ctx, []string{"sr133", "sar", "sr200"})
	if err != nil {
		t.Fatal(err)
	}
	if regs[0].Error == "" || regs[1].Error != "" || regs[2].Error == "" {
		t.Fatalf("%+v", regs)
	}
	if _, err := r.c.ReadRegister(ctx, "sr133"); err == nil {
		t.Fatal("expected an error for an unimplemented register")
	}
	if _, err := r.c.ReadRegisters(ctx, []string{"a16"}); err == nil {
		t.Fatal("a16 accepted")
	}
	if _, err := r.c.ReadRegisters(ctx, []string{"bogus"}); err == nil {
		t.Fatal("bogus accepted")
	}
	// Writes.
	for name, v := range map[string]uint32{"a5": 0xdead0005, "a3": 0xdead0003, "sar": 7, "pc": loop[2], "ps": 0x00060020} {
		if err := r.c.WriteRegister(ctx, name, v); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		if got, err := r.c.ReadRegister(ctx, name); err != nil || got != v {
			t.Fatalf("%s read back %#x %v", name, got, err)
		}
	}
	if err := r.c.WriteRegister(ctx, "ddr", 1); err == nil {
		t.Fatal("DDR write accepted")
	}
	if err := r.c.WriteRegister(ctx, "prid", 1); err == nil {
		t.Fatal("read-only register write accepted")
	}
	if err := r.c.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if r.t.AR(5) != 0xdead0005 || r.t.AR(3) != 0xdead0003 {
		t.Fatalf("a5 %#x a3 %#x", r.t.AR(5), r.t.AR(3))
	}
	if sr, _ := r.t.SR(xtensa.SRSAR); sr != 7 {
		t.Fatalf("sar %d", sr)
	}
	if r.t.PS() != 0x00060020 {
		t.Fatalf("ps %#x", r.t.PS())
	}
	if _, err := r.c.ReadRegisters(ctx, []string{"pc"}); !errors.Is(err, xtensa.ErrNotHalted) {
		t.Fatal(err)
	}
	if err := r.c.WriteRegister(ctx, "pc", 0); !errors.Is(err, xtensa.ErrNotHalted) {
		t.Fatal(err)
	}
}

func TestRegisterDumpIsBatched(t *testing.T) {
	r := newRig(t)
	halted(t, r)
	before := r.port.Calls
	if _, err := r.c.ReadRegisters(context.Background(), xtensa.CoreRegisters); err != nil {
		t.Fatal(err)
	}
	if n := r.port.Calls - before; n > 3 {
		t.Fatalf("%d JTAG batches for one register dump", n)
	}
}

func TestMemory(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if _, err := r.c.ReadWords(ctx, romBase, 1); !errors.Is(err, xtensa.ErrNotHalted) {
		t.Fatal(err)
	}
	halted(t, r)
	w, err := r.c.ReadWords(ctx, romBase+8, 3)
	if err != nil || w[0] != 0xC0DE0008 || w[2] != 0xC0DE0010 {
		t.Fatalf("%08x %v", w, err)
	}
	// A transfer longer than one chunk and one JTAG batch.
	big := make([]uint32, 2500)
	for i := range big {
		big[i] = uint32(i) * 0x01010101
	}
	if err := r.c.WriteWords(ctx, dramBase, big); err != nil {
		t.Fatal(err)
	}
	back, err := r.c.ReadWords(ctx, dramBase, len(big))
	if err != nil {
		t.Fatal(err)
	}
	for i := range big {
		if back[i] != big[i] {
			t.Fatalf("word %d: %#x != %#x", i, back[i], big[i])
		}
	}
	// Byte-granular writes keep the neighbours.
	r.t.SetMem(dramBase+0x100, []byte{1, 2, 3, 4, 5, 6, 7, 8})
	if err := r.c.WriteMem(ctx, dramBase+0x101, []byte{0xAA, 0xBB, 0xCC, 0xDD}); err != nil {
		t.Fatal(err)
	}
	if got := r.t.Mem(dramBase+0x100, 8); string(got) != string([]byte{1, 0xAA, 0xBB, 0xCC, 0xDD, 6, 7, 8}) {
		t.Fatalf("% x", got)
	}
	if err := r.c.WriteMem(ctx, dramBase+0x106, []byte{0xEE}); err != nil {
		t.Fatal(err)
	}
	if got, err := r.c.ReadMem(ctx, dramBase+0x101, 6); err != nil || string(got) != string([]byte{0xAA, 0xBB, 0xCC, 0xDD, 6, 0xEE}) {
		t.Fatalf("% x %v", got, err)
	}
	if got, err := r.c.ReadMem(ctx, dramBase, 0); err != nil || len(got) != 0 {
		t.Fatal(got, err)
	}
	if err := r.c.WriteMem(ctx, dramBase, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.ReadWords(ctx, dramBase+1, 1); err == nil {
		t.Fatal("unaligned word read accepted")
	}
	if _, err := r.c.ReadMem(ctx, 0xFFFFFFFE, 4); err == nil {
		t.Fatal("wrapping read accepted")
	}
}

func TestMemoryFaultRestoresExceptionState(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.t.SetSR(xtensa.SREXCCAUSE, 5)
	r.t.SetSR(xtensa.SREXCVADDR, 0x1111)
	r.t.SetSR(xtensa.SRDEPC, 0x2222)
	halted(t, r)
	_, err := r.c.ReadWords(ctx, 0x10000000, 2)
	var ee *xtensa.ExecError
	if !errors.As(err, &ee) || ee.DSR&xtensa.DSRExecException == 0 || !strings.Contains(err.Error(), "exception") {
		t.Fatalf("read fault: %v", err)
	}
	if v, _ := r.t.SR(xtensa.SREXCCAUSE); v != 28 {
		t.Fatalf("sim EXCCAUSE %d", v)
	}
	// The debugger keeps working after a fault (sticky bits cleared).
	if _, err := r.c.ReadWords(ctx, romBase, 1); err != nil {
		t.Fatal(err)
	}
	if err := r.c.WriteWords(ctx, romBase, []uint32{1}); err == nil {
		t.Fatal("ROM write succeeded")
	}
	if err := r.c.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	for sr, want := range map[uint8]uint32{xtensa.SREXCCAUSE: 5, xtensa.SREXCVADDR: 0x1111, xtensa.SRDEPC: 0x2222} {
		if v, _ := r.t.SR(sr); v != want {
			t.Errorf("SR %d = %#x after resume, want %#x", sr, v, want)
		}
	}
	if r.t.DSR()&(xtensa.DSRExecException|xtensa.DSRExecOverrun) != 0 {
		t.Fatalf("sticky DSR left set: %#x", r.t.DSR())
	}
}

// Release leaves the core halted with a3 and the exception registers written
// back; a fresh Core (a new debugger session) attaches to the halt and can
// resume it.
func TestReleaseAndReattach(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.t.SetAR(3, 0xfeedf00d)
	r.t.SetSR(xtensa.SREXCCAUSE, 7)
	h := halted(t, r)
	if _, err := r.c.ReadWords(ctx, 0x10000000, 1); err == nil {
		t.Fatal("expected a fault")
	}
	if err := r.c.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if !r.t.Stopped() || r.c.Halted() || r.t.AR(3) != 0xfeedf00d {
		t.Fatalf("after release: stopped=%v halted=%v a3=%#x", r.t.Stopped(), r.c.Halted(), r.t.AR(3))
	}
	if v, _ := r.t.SR(xtensa.SREXCCAUSE); v != 7 {
		t.Fatalf("EXCCAUSE %d not restored", v)
	}
	if err := r.c.Release(ctx); err != nil {
		t.Fatal("second release:", err)
	}
	c2 := xtensa.NewCore(r.c.DM, 6)
	c2.Poll = 0
	h2, err := c2.Halt(ctx)
	if err != nil || !h2.Already || h2.PC != h.PC {
		t.Fatalf("reattach %+v %v", h2, err)
	}
	if v, err := c2.ReadRegister(ctx, "a3"); err != nil || v != 0xfeedf00d {
		t.Fatalf("a3 %#x %v", v, err)
	}
	if err := c2.Resume(ctx); err != nil || r.t.Stopped() || r.t.AR(3) != 0xfeedf00d {
		t.Fatalf("resume %v stopped=%v a3=%#x", err, r.t.Stopped(), r.t.AR(3))
	}
}

func TestStep(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	h := halted(t, r)
	r.t.SetSR(xtensa.SRICOUNTLEVEL, 0)
	pc := h.PC
	for i := 0; i < 5; i++ {
		s, err := r.c.Step(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if want := next(pc); s.PC != want || s.DebugCause&xtensa.CauseICount == 0 {
			t.Fatalf("step %d: %+v, want pc %#x", i, s, want)
		}
		pc = s.PC
	}
	if v, _ := r.t.SR(xtensa.SRICOUNTLEVEL); v != 0 {
		t.Fatalf("ICOUNTLEVEL left at %d", v)
	}
	// A program running at the debug level cannot be stepped.
	if err := r.c.WriteRegister(ctx, "ps", 0x26); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.Step(ctx); err == nil {
		t.Fatal("step at INTLEVEL 6 accepted")
	}
	if err := r.c.WriteRegister(ctx, "ps", 0x20); err != nil {
		t.Fatal(err)
	}
	if err := r.c.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.Step(ctx); !errors.Is(err, xtensa.ErrNotHalted) {
		t.Fatal(err)
	}
}

func TestBreakpoints(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if _, err := r.c.SetIBreak(ctx, loop[1]); !errors.Is(err, xtensa.ErrNotHalted) {
		t.Fatal(err)
	}
	halted(t, r)
	if n, err := r.c.IBreakSlots(ctx); err != nil || n != 2 {
		t.Fatalf("slots %d %v", n, err)
	}
	bp, err := r.c.SetIBreak(ctx, loop[1])
	if err != nil || bp.Slot != 0 {
		t.Fatalf("%+v %v", bp, err)
	}
	if again, _ := r.c.SetIBreak(ctx, loop[1]); again != bp {
		t.Fatalf("duplicate breakpoint got %+v", again)
	}
	for i := 0; i < 3; i++ {
		if err := r.c.Resume(ctx); err != nil {
			t.Fatal(err)
		}
		h, err := r.c.WaitHalt(ctx, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if h.PC != loop[1] || h.DebugCause&xtensa.CauseIBreak == 0 {
			t.Fatalf("hit %d: %+v", i, h)
		}
	}
	if h, err := r.c.WaitHalt(ctx, time.Second); err != nil || !h.Already {
		t.Fatalf("wait while halted: %+v %v", h, err)
	}
	// Slot 1 is enabled by someone else: no slot left for us.
	ibe, _ := r.c.ReadRegister(ctx, "ibreakenable")
	if err := r.c.WriteRegister(ctx, "ibreakenable", ibe|2); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.SetIBreak(ctx, loop[2]); !errors.Is(err, xtensa.ErrNoSlot) {
		t.Fatalf("expected ErrNoSlot, got %v", err)
	}
	if err := r.c.WriteRegister(ctx, "ibreakenable", ibe); err != nil {
		t.Fatal(err)
	}
	if bp2, err := r.c.SetIBreak(ctx, loop[2]); err != nil || bp2.Slot != 1 {
		t.Fatalf("%+v %v", bp2, err)
	}
	if l := r.c.IBreaks(); len(l) != 2 {
		t.Fatalf("list %+v", l)
	}
	for _, a := range []uint32{loop[1], loop[2]} {
		if err := r.c.ClearIBreak(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.c.ClearIBreak(ctx, loop[2]); err == nil {
		t.Fatal("clearing a missing breakpoint succeeded")
	}
	if v, _ := r.t.SR(xtensa.SRIBREAKENABLE); v != 0 {
		t.Fatalf("IBREAKENABLE %#x", v)
	}
	if err := r.c.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.WaitHalt(ctx, 20*time.Millisecond); !errors.Is(err, xtensa.ErrTimeout) {
		t.Fatalf("expected timeout, got %v", err)
	}
}

func TestReset(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	halted(t, r)
	if _, err := r.c.SetIBreak(ctx, loop[2]); err != nil {
		t.Fatal(err)
	}
	if err := r.c.Reset(ctx, true); err != nil {
		t.Fatal(err)
	}
	h, err := r.c.WaitHalt(ctx, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if h.PC != loop[0] || h.DebugCause&xtensa.CauseDebugInt == 0 || len(r.c.IBreaks()) != 0 {
		t.Fatalf("after reset-halt %+v, ibreaks %v", h, r.c.IBreaks())
	}
	st, _ := r.c.Status(ctx)
	if !st.CoreWasReset {
		t.Fatal("CoreWasReset not reported")
	}
	if err := r.c.Reset(ctx, false); err != nil {
		t.Fatal(err)
	}
	if r.t.Stopped() || r.t.InReset() {
		t.Fatal("core not running after reset")
	}
}

func TestOtherChainPositions(t *testing.T) {
	ch := &xtsim.Chain{TAPs: []*xtsim.Target{simCore(0xCDCD, true), simCore(0xABAB, false), simCore(0x1111, false)}}
	for _, idx := range []int{1, 2} {
		r := rigOn(t, ch, idx)
		ctx := context.Background()
		halted(t, r)
		if v, err := r.c.ReadRegister(ctx, "prid"); err != nil || v != []uint32{0, 0xABAB, 0x1111}[idx] {
			t.Fatalf("TAP %d prid %#x %v", idx, v, err)
		}
		if err := r.c.WriteWords(ctx, dramBase, []uint32{uint32(idx)}); err != nil {
			t.Fatal(err)
		}
		if err := r.c.Resume(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for i, want := range []uint32{0, 1, 2} {
		if got := binary.LittleEndian.Uint32(ch.TAPs[i].Mem(dramBase, 4)); got != want {
			t.Errorf("TAP %d memory %d, want %d", i, got, want)
		}
		if ch.TAPs[i].Stopped() {
			t.Errorf("TAP %d left halted", i)
		}
	}
}

func TestEncodings(t *testing.T) {
	// Spot checks against encodings worked out by hand from the ISA manual.
	for _, tc := range []struct {
		got, want uint32
	}{
		{xtensa.RSR(xtensa.SRDDR, 3), 0x036830},
		{xtensa.WSR(xtensa.SRDDR, 3), 0x136830},
		{xtensa.XSR(xtensa.SRPS, 2), 0x61E620},
		{xtensa.LDDR32P(3), 0x0073E0},
		{xtensa.SDDR32P(3), 0x0073F0},
		{xtensa.L32I(4, 5, 2), 0x022452},
		{xtensa.S32I(4, 5, 2), 0x026452},
		{xtensa.L8UI(4, 5, 2), 0x020452},
		{xtensa.S8I(4, 5, 2), 0x024452},
		{xtensa.L16UI(4, 5, 2), 0x021452},
		{xtensa.S16I(4, 5, 2), 0x025452},
		{xtensa.ADDI(1, 1, -16), 0xF0C112}, // the usual "addi a1, a1, -16" prologue
		{xtensa.ADDI(3, 3, 0x54), 0x54C332},
	} {
		if tc.got != tc.want {
			t.Errorf("got %06x want %06x", tc.got, tc.want)
		}
	}
	if xtensa.EPC(6) != 182 || xtensa.EPS(6) != 198 {
		t.Fatal("EPC/EPS numbering")
	}
	if c := xtensa.CauseNames(xtensa.CauseIBreak | xtensa.CauseDebugInt); len(c) != 2 {
		t.Fatal(c)
	}
	if len(xtensa.SRNames()) == 0 {
		t.Fatal("no SR names")
	}
}

func next(pc uint32) uint32 {
	for i, p := range loop {
		if p == pc {
			return loop[(i+1)%len(loop)]
		}
	}
	return 0
}

func contains(l []uint32, v uint32) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return "1" + string(rune('0'+i-10))
}

func TestMemBatch(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if _, err := r.c.MemBatch(ctx, []xtensa.MemOp{{Addr: dramBase, Words: 1}}); err == nil {
		t.Fatal("MemBatch on a running core")
	}
	r.t.SetAR(3, 0x5a5a1234)
	halted(t, r)
	out, err := r.c.MemBatch(ctx, []xtensa.MemOp{
		{Write: true, Addr: dramBase + 0x10, Vals: []uint32{1, 2, 3}},
		{Addr: romBase + 8, Words: 2},
		{Addr: dramBase + 0x10, Words: 3},
		{Write: true, Addr: dramBase + 0x20, Vals: []uint32{0xdeadbeef}},
		{Addr: dramBase + 0x20, Words: 0},
		{Addr: dramBase + 0x20, Words: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Ops 3 and 5 sit within ADDI reach of where the previous one left the
	// pointer, so they move it instead of reloading it through DDR.
	if len(out) != 6 || out[0] != nil || out[3] != nil || out[4] != nil ||
		out[1][0] != 0xC0DE0008 || out[1][1] != 0xC0DE000C ||
		out[2][0] != 1 || out[2][1] != 2 || out[2][2] != 3 || out[5][0] != 0xdeadbeef {
		t.Fatalf("%#v", out)
	}
	if _, err := r.c.MemBatch(ctx, []xtensa.MemOp{{Addr: dramBase + 2, Words: 1}}); err == nil {
		t.Fatal("unaligned address accepted")
	}
	// A faulting access fails the whole batch with the DSR exception and the
	// debugger keeps working.
	_, err = r.c.MemBatch(ctx, []xtensa.MemOp{{Addr: dramBase, Words: 1}, {Write: true, Addr: romBase, Vals: []uint32{7}}})
	var ee *xtensa.ExecError
	if !errors.As(err, &ee) || ee.DSR&xtensa.DSRExecException == 0 {
		t.Fatalf("fault: %v", err)
	}
	if out, err := r.c.MemBatch(ctx, []xtensa.MemOp{{Addr: dramBase + 0x20, Words: 1}}); err != nil || out[0][0] != 0xdeadbeef {
		t.Fatalf("after fault %v %v", out, err)
	}
	if err := r.c.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if r.t.AR(3) != 0x5a5a1234 || r.t.DSR()&(xtensa.DSRExecException|xtensa.DSRExecOverrun) != 0 {
		t.Fatalf("a3 %#x, DSR %#x", r.t.AR(3), r.t.DSR())
	}
}
